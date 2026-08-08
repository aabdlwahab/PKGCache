package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func put(t *testing.T, s *Store, data string) Digest {
	t.Helper()
	w, err := s.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	d, n, err := w.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n != int64(len(data)) {
		t.Fatalf("Commit size = %d, want %d", n, len(data))
	}
	return d
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestParseDigest(t *testing.T) {
	valid := sha("hello")
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "bare hex", in: valid, want: valid},
		{name: "sha256 prefixed", in: "sha256:" + valid, want: valid},
		{name: "uppercase normalised", in: strings.ToUpper(valid), want: valid},
		{name: "empty", in: "", wantErr: true},
		{name: "too short", in: "abcd", wantErr: true},
		{name: "too long", in: valid + "ab", wantErr: true},
		{name: "non-hex", in: strings.Repeat("g", 64), wantErr: true},
		{name: "wrong algorithm", in: "md5:" + valid, wantErr: true},
		{name: "path traversal", in: strings.Repeat("../", 21) + "x", wantErr: true},
		{name: "embedded slash", in: valid[:32] + "/" + valid[33:], wantErr: true},
		{name: "embedded NUL", in: valid[:32] + "\x00" + valid[33:], wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseDigest(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseDigest(%q) = %q, want error", c.in, got)
				}
				if !errors.Is(err, ErrInvalidDigest) {
					t.Fatalf("error should wrap ErrInvalidDigest, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDigest(%q): %v", c.in, err)
			}
			if string(got) != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	s := newStore(t)
	const body = "the quick brown fox"
	d := put(t, s, body)

	if got := string(d); got != sha(body) {
		t.Fatalf("digest = %s, want %s", got, sha(body))
	}
	if !s.Exists(d) {
		t.Fatal("Exists = false after commit")
	}

	f, st, err := s.Open(d)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if st.Size != int64(len(body)) {
		t.Fatalf("Stat.Size = %d, want %d", st.Size, len(body))
	}
	got, _ := io.ReadAll(f)
	if string(got) != body {
		t.Fatalf("content = %q, want %q", got, body)
	}
}

func TestOpenMissingIsErrNotFound(t *testing.T) {
	s := newStore(t)
	_, _, err := s.Open(MustParseDigest(sha("absent")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A blob is world-readable so operators and backup jobs need no chmod dance.
func TestCommittedBlobIsWorldReadable(t *testing.T) {
	s := newStore(t)
	d := put(t, s, "x")
	p, _ := s.Path(d)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != blobMode {
		t.Fatalf("mode = %v, want %v", fi.Mode().Perm(), os.FileMode(blobMode))
	}
}

// P1-07 acceptance: concurrent identical commits must all succeed, and the winning
// inode must be stable — nothing may rewrite a blob in place.
func TestConcurrentIdenticalCommits(t *testing.T) {
	s := newStore(t)
	const body = "same bytes from every writer"
	const writers = 16

	var wg sync.WaitGroup
	digests := make([]Digest, writers)
	errs := make([]error, writers)
	start := make(chan struct{})

	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, err := s.Create()
			if err != nil {
				errs[i] = err
				return
			}
			<-start
			if _, err := io.WriteString(w, body); err != nil {
				errs[i] = err
				return
			}
			digests[i], _, errs[i] = w.Commit()
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
		if string(digests[i]) != sha(body) {
			t.Fatalf("writer %d: digest = %s", i, digests[i])
		}
	}
	// Exactly one blob, and no staging debris left behind.
	count, _, err := s.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if count != 1 {
		t.Fatalf("blob count = %d, want 1", count)
	}
	assertStagingEmpty(t, s)
}

// P1-07 acceptance: an interrupted write leaves only staging garbage — never a
// truncated file published under a digest that does not match its content.
func TestAbortLeavesNoBlob(t *testing.T) {
	s := newStore(t)
	w, err := s.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := io.WriteString(w, "partial"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if s.Exists(MustParseDigest(sha("partial"))) {
		t.Fatal("aborted write was published")
	}
	assertStagingEmpty(t, s)
}

func TestAbortAfterCommitIsNoOp(t *testing.T) {
	s := newStore(t)
	w, _ := s.Create()
	_, _ = io.WriteString(w, "body")
	d, _, err := w.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Abort(); err != nil { // the `defer w.Abort()` idiom must be safe
		t.Fatalf("Abort after Commit: %v", err)
	}
	if !s.Exists(d) {
		t.Fatal("Abort after Commit deleted the blob")
	}
}

func TestWriteAfterCloseFails(t *testing.T) {
	s := newStore(t)
	w, _ := s.Create()
	if _, _, err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := io.WriteString(w, "late"); err == nil {
		t.Fatal("write after commit should fail")
	}
	if _, _, err := w.Commit(); err == nil {
		t.Fatal("second commit should fail")
	}
}

// P1-08 acceptance: staging debris from a kill -9 is swept at startup, and only
// staging debris — committed blobs are untouched.
func TestCleanStaging(t *testing.T) {
	s := newStore(t)
	keep := put(t, s, "committed")

	orphan := filepath.Join(s.staging, "crashed"+partSuffix)
	if err := os.WriteFile(orphan, []byte("half a wheel"), 0o644); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	unrelated := filepath.Join(s.staging, "notours.txt")
	if err := os.WriteFile(unrelated, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed unrelated: %v", err)
	}

	n, err := s.CleanStaging()
	if err != nil {
		t.Fatalf("CleanStaging: %v", err)
	}
	if n != 1 {
		t.Fatalf("removed = %d, want 1", n)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan survived")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal("CleanStaging removed a file that was not a staging part")
	}
	if !s.Exists(keep) {
		t.Fatal("CleanStaging removed a committed blob")
	}
}

func TestDeleteAndWalk(t *testing.T) {
	s := newStore(t)
	want := map[Digest]int64{}
	for _, body := range []string{"a", "bb", "ccc"} {
		want[put(t, s, body)] = int64(len(body))
	}

	got := map[Digest]int64{}
	if err := s.Walk(func(d Digest, st Stat) error {
		got[d] = st.Size
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("walked %d blobs, want %d", len(got), len(want))
	}
	for d, size := range want {
		if got[d] != size {
			t.Fatalf("blob %s size = %d, want %d", d, got[d], size)
		}
	}

	var victim Digest
	for d := range want {
		victim = d
		break
	}
	if err := s.Delete(victim); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Exists(victim) {
		t.Fatal("blob survived Delete")
	}
	// Deleting twice must not error: two GC passes may race.
	if err := s.Delete(victim); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestUsage(t *testing.T) {
	s := newStore(t)
	put(t, s, "aaaa")
	put(t, s, "bb")
	put(t, s, "aaaa") // duplicate content must not be counted twice

	count, bytes, err := s.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if count != 2 || bytes != 6 {
		t.Fatalf("Usage = (%d, %d), want (2, 6)", count, bytes)
	}
}

func TestManagedDirRejectsTraversal(t *testing.T) {
	s := newStore(t)
	for _, bad := range []string{"..", ".", "", "a/b", `a\b`, "x\x00y", "../../etc"} {
		if _, err := s.ManagedDir("git", bad); err == nil {
			t.Fatalf("ManagedDir accepted project %q", bad)
		}
		if _, err := s.ManagedDir(bad, "global"); err == nil {
			t.Fatalf("ManagedDir accepted eco %q", bad)
		}
	}
	p, err := s.ManagedDir("git", "global")
	if err != nil {
		t.Fatalf("ManagedDir: %v", err)
	}
	if !strings.HasPrefix(p, s.managed) {
		t.Fatalf("managed dir %q escaped %q", p, s.managed)
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		t.Fatalf("managed dir not created: %v", err)
	}
}

func TestImportKnownHardlinksWithoutHashing(t *testing.T) {
	source := filepath.Join(t.TempDir(), "legacy-cas-entry")
	content := []byte("already content addressed")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := ParseDigest(sha(string(content)))
	if err != nil {
		t.Fatal(err)
	}
	store := newStore(t)
	stat, err := store.ImportKnown(digest, source)
	if err != nil {
		t.Fatalf("ImportKnown: %v", err)
	}
	if stat.Size != int64(len(content)) {
		t.Fatalf("size = %d", stat.Size)
	}
	target, err := store.Path(digest)
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(sourceInfo, targetInfo) {
		t.Fatal("same-filesystem import did not preserve the source inode")
	}
	old := sourceInfo.ModTime().Add(-time.Hour)
	if err := os.Chtimes(source, old, old); err != nil {
		t.Fatal(err)
	}
	writer, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writer.CommitImported(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(old) {
		t.Fatalf("migration commit touched legacy inode: before=%s after=%s", old, after.ModTime())
	}
}

// P1-08 acceptance: no digest, however malformed, may resolve to a path outside the
// blob tree.
func FuzzDigestPathSafety(f *testing.F) {
	f.Add(sha("seed"))
	f.Add("../../../etc/passwd")
	f.Add("sha256:" + sha("seed"))
	f.Add(strings.Repeat("a", 63))
	f.Add("")

	s, err := Open(f.TempDir())
	if err != nil {
		f.Fatalf("Open: %v", err)
	}
	f.Fuzz(func(t *testing.T, in string) {
		d, err := ParseDigest(in)
		if err != nil {
			return // rejected at the boundary, which is the point
		}
		p, err := s.Path(d)
		if err != nil {
			t.Fatalf("valid digest %q has no path: %v", d, err)
		}
		clean := filepath.Clean(p)
		if !strings.HasPrefix(clean, s.blobs+string(filepath.Separator)) {
			t.Fatalf("digest %q escaped the blob tree: %s", in, clean)
		}
	})
}

func assertStagingEmpty(t *testing.T, s *Store) {
	t.Helper()
	entries, err := os.ReadDir(s.staging)
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	for _, e := range entries {
		t.Fatalf("staging not cleaned: %s", e.Name())
	}
}
