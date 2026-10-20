package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/config"
)

// A layer sent in pieces is one entry, under the digest of the whole: the pieces are only
// how it travelled.
func TestUploadInPiecesIsStoredAsOne(t *testing.T) {
	h := newHarness(t)
	id, err := h.engine.StartUpload("global", "oci")
	if err != nil {
		t.Fatal(err)
	}
	for _, piece := range []string{"first half, ", "second half"} {
		if _, err := h.engine.AppendUpload("global", "oci", id, strings.NewReader(piece), 0); err != nil {
			t.Fatal(err)
		}
	}
	if size, ok := h.engine.UploadSize("global", "oci", id); !ok || size != int64(len("first half, second half")) {
		t.Fatalf("upload holds %d bytes (open %v)", size, ok)
	}
	want := blob.DigestBytes([]byte("first half, second half"))
	result, err := h.engine.CommitUpload("global", "oci", id, "hosted/blob/x", nil,
		PutOptions{ExpectDigest: want, Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Digest != want {
		t.Fatalf("stored %s, want %s", result.Digest, want)
	}
	entry, err := h.engine.Entry("global", "oci", "hosted/blob/x")
	if err != nil || entry.Digest != want {
		t.Fatalf("entry = %+v, %v", entry, err)
	}
	if _, ok := h.engine.UploadSize("global", "oci", id); ok {
		t.Fatal("a committed upload is still open")
	}
}

// A digest that does not match leaves no entry, and the upload is over: the client starts
// that layer again rather than appending to bytes it has just been told are wrong.
func TestUploadThatDoesNotMatchItsDigestLeavesNothing(t *testing.T) {
	h := newHarness(t)
	id, err := h.engine.StartUpload("global", "oci")
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.engine.CommitUpload("global", "oci", id, "hosted/blob/x", strings.NewReader("bytes"),
		PutOptions{ExpectDigest: blob.DigestBytes([]byte("other bytes"))})
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if _, err := h.engine.Entry("global", "oci", "hosted/blob/x"); err == nil {
		t.Fatal("a mismatched upload was stored")
	}
	if _, err := h.engine.AppendUpload("global", "oci", id, strings.NewReader("more"), 0); !errors.Is(err, ErrUploadUnknown) {
		t.Fatalf("append after a failed commit: %v, want ErrUploadUnknown", err)
	}
}

// An upload id is only good in the project that opened it.
func TestUploadIsOnlyItsOwnProjects(t *testing.T) {
	h := newHarness(t)
	id, err := h.engine.StartUpload("work", "oci")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.AppendUpload("global", "oci", id, strings.NewReader("x"), 0); !errors.Is(err, ErrUploadUnknown) {
		t.Fatalf("another project appended to the upload: %v", err)
	}
	if _, ok := h.engine.UploadSize("work", "oci", id); !ok {
		t.Fatal("the refused append closed the owner's upload")
	}
}

// A push that died halfway does not keep its staged layer for the life of the process.
func TestIdleUploadIsAbandonedWhenTheNextStarts(t *testing.T) {
	h := newHarness(t)
	stale, err := h.engine.StartUpload("global", "oci")
	if err != nil {
		t.Fatal(err)
	}
	h.engine.uploads.open[stale].touched = time.Now().Add(-2 * uploadIdle)
	if _, err := h.engine.StartUpload("global", "oci"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.engine.UploadSize("global", "oci", stale); ok {
		t.Fatal("an upload idle for twice the limit is still open")
	}
}

// The air-gapped side is a mirror: an upload there would be lost at the next import.
func TestOfflineProjectOpensNoUpload(t *testing.T) {
	h := newHarness(t)
	if err := h.cfg.Apply(func(s *config.Snapshot) error {
		s.Upstream.Offline = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.StartUpload("global", "oci"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("err = %v, want ErrReadOnly", err)
	}
}

// Linking files content the store holds under a second key, and refuses content it does
// not hold rather than recording an entry nothing backs.
func TestLinkFilesExistingContentAndRefusesMissing(t *testing.T) {
	h := newHarness(t)
	digest, err := h.engine.PutBytes("global", "oci", "blob/x", []byte("layer"), "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Link("global", "oci", "hosted/blob/x", digest, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	body, entry, err := h.engine.ReadEntry("global", "oci", "hosted/blob/x", 1024)
	if err != nil || string(body) != "layer" || entry.Digest != digest {
		t.Fatalf("read back %q, %+v, %v", body, entry, err)
	}
	if err := h.engine.Link("global", "oci", "hosted/blob/y", blob.DigestBytes([]byte("absent")), ""); err == nil {
		t.Fatal("linked a digest the store does not hold")
	}
	if _, _, err := h.engine.ReadEntry("global", "oci", "hosted/blob/x", 2); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("an entry over the bound was read: %v", err)
	}
}
