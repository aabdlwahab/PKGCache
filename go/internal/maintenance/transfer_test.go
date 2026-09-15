package maintenance

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
)

// putEco stores body under an ecosystem's key in a project.
func (h *harness) putEco(t *testing.T, project, eco, key, body string) blob.Digest {
	t.Helper()
	writer, err := h.blobs.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strings.NewReader(body).WriteTo(writer); err != nil {
		t.Fatal(err)
	}
	digest, size, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cat.CommitEntry(catalog.Entry{
		EntryKey: catalog.EntryKey{Project: project, Eco: eco, Key: key},
		Digest:   digest, Size: size, CachedAt: h.now, LastAccess: h.now,
	}, nil, catalog.Quota{}, false); err != nil {
		t.Fatal(err)
	}
	return digest
}

func (h *harness) has(t *testing.T, project, eco, key string) (blob.Digest, bool) {
	t.Helper()
	entry, err := h.cat.GetEntry(catalog.EntryKey{Project: project, Eco: eco, Key: key})
	if errors.Is(err, catalog.ErrNotFound) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return entry.Digest, true
}

// npmLike is the npm adapter's answer, so these tests exercise the mechanism without
// importing an ecosystem.
func npmLike(_, key string, _ func() ([]byte, error)) ([]string, error) {
	if name, _, found := strings.Cut(key, "/-/"); found {
		return []string{"packument/" + name}, nil
	}
	return nil, nil
}

func (h *harness) npmPackage(t *testing.T, project string) blob.Digest {
	t.Helper()
	tarball := h.putEco(t, project, "npm", "left-pad/-/left-pad-1.3.0.tgz", "the tarball")
	h.putEco(t, project, "npm", "packument/left-pad", `{"name":"left-pad"}`)
	if err := h.cat.PutArtifact(catalog.Artifact{
		Project: project, Eco: "npm", Name: "left-pad", Version: "1.3.0",
		Digest: tarball, Size: 11, CachedAt: h.now,
	}); err != nil {
		t.Fatal(err)
	}
	return tarball
}

func artifactsIn(t *testing.T, h *harness, project string) []catalog.Artifact {
	t.Helper()
	rows, _, err := h.cat.QueryArtifacts(catalog.ArtifactQuery{Project: project})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestCopyingAPackageTakesWhatItIsReachedThrough(t *testing.T) {
	h := newHarness(t)
	tarball := h.npmPackage(t, "work")

	result, err := h.svc.Transfer(context.Background(), TransferOptions{
		From: "work", To: "side", Digests: map[blob.Digest]struct{}{tarball: {}},
		Companions: npmLike,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Packages != 1 || result.Copied != 2 || result.Companions != 1 {
		t.Errorf("result = %+v, want one package in two entries, one a companion", result)
	}
	for _, key := range []string{"left-pad/-/left-pad-1.3.0.tgz", "packument/left-pad"} {
		if _, ok := h.has(t, "side", "npm", key); !ok {
			t.Errorf("side lacks %s", key)
		}
		if _, ok := h.has(t, "work", "npm", key); !ok {
			t.Errorf("a copy took %s out of work", key)
		}
	}
	if rows := artifactsIn(t, h, "side"); len(rows) != 1 || rows[0].Name != "left-pad" {
		t.Errorf("side's inventory = %+v, want left-pad", rows)
	}
}

// A move takes the package and leaves the packument: the same index leads to the source
// project's other versions of it.
func TestMovingAPackageLeavesTheIndexItShares(t *testing.T) {
	h := newHarness(t)
	tarball := h.npmPackage(t, "global")

	result, err := h.svc.Transfer(context.Background(), TransferOptions{
		From: "global", To: "work", Digests: map[blob.Digest]struct{}{tarball: {}},
		Move: true, Companions: npmLike,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 {
		t.Errorf("removed = %d, want the one selected entry", result.Removed)
	}
	if _, ok := h.has(t, "global", "npm", "left-pad/-/left-pad-1.3.0.tgz"); ok {
		t.Error("the moved tarball is still in global")
	}
	if _, ok := h.has(t, "global", "npm", "packument/left-pad"); !ok {
		t.Error("the move took the shared packument out of global")
	}
	if _, ok := h.has(t, "work", "npm", "left-pad/-/left-pad-1.3.0.tgz"); !ok {
		t.Error("work did not receive the tarball")
	}
	if rows := artifactsIn(t, h, "global"); len(rows) != 0 {
		t.Errorf("global still lists %+v", rows)
	}
	if rows := artifactsIn(t, h, "work"); len(rows) != 1 {
		t.Errorf("work lists %+v, want the package", rows)
	}
	if !h.blobs.Exists(tarball) {
		t.Error("a move deleted the bytes the destination now holds")
	}
}

// Under the same key, different content is somebody else's file. It is left alone, and a
// move leaves the source's copy too, because taking it would leave the package nowhere.
func TestATransferNeverOverwritesADifferentFile(t *testing.T) {
	h := newHarness(t)
	tarball := h.npmPackage(t, "work")
	theirs := h.putEco(t, "side", "npm", "left-pad/-/left-pad-1.3.0.tgz", "a different tarball")

	result, err := h.svc.Transfer(context.Background(), TransferOptions{
		From: "work", To: "side", Digests: map[blob.Digest]struct{}{tarball: {}},
		Move: true, Companions: npmLike,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Conflicts != 1 || result.Removed != 0 {
		t.Errorf("result = %+v, want one conflict and nothing removed", result)
	}
	if digest, _ := h.has(t, "side", "npm", "left-pad/-/left-pad-1.3.0.tgz"); digest != theirs {
		t.Error("side's own file was overwritten")
	}
	if _, ok := h.has(t, "work", "npm", "left-pad/-/left-pad-1.3.0.tgz"); !ok {
		t.Error("a move whose destination refused the file removed it anyway")
	}
}

// Companions are asked about what they name in turn, and read from the store: an index
// names manifests, and each manifest names its layers.
func TestCompanionsOfCompanionsTravelToo(t *testing.T) {
	h := newHarness(t)
	h.putEco(t, "global", "oci", "blob/layer-1", "layer one")
	h.putEco(t, "global", "oci", "manifest/child", "blob/layer-1\nblob/layer-not-held")
	index := h.putEco(t, "global", "oci", "manifest/index", "manifest/child")

	readsManifests := func(_, key string, read func() ([]byte, error)) ([]string, error) {
		if !strings.HasPrefix(key, "manifest/") {
			return nil, nil
		}
		body, err := read()
		if err != nil {
			return nil, err
		}
		return strings.Split(string(body), "\n"), nil
	}
	result, err := h.svc.Transfer(context.Background(), TransferOptions{
		From: "global", To: "work", Digests: map[blob.Digest]struct{}{index: {}},
		Companions: readsManifests,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"manifest/index", "manifest/child", "blob/layer-1"} {
		if _, ok := h.has(t, "work", "oci", key); !ok {
			t.Errorf("work lacks %s", key)
		}
	}
	if result.Copied != 3 || result.Companions != 2 {
		t.Errorf("result = %+v, want three entries, two of them companions", result)
	}
}

func TestTransferRefusesWhatCannotMeanAnything(t *testing.T) {
	h := newHarness(t)
	tarball := h.npmPackage(t, "work")
	if _, err := h.svc.Transfer(context.Background(), TransferOptions{
		From: "work", To: "work", Digests: map[blob.Digest]struct{}{tarball: {}},
	}); err == nil {
		t.Error("a transfer into the project it came from was accepted")
	}
	stale := blob.Digest(strings.Repeat("f", 64))
	result, err := h.svc.Transfer(context.Background(), TransferOptions{
		From: "work", To: "side", Digests: map[blob.Digest]struct{}{stale: {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Missing != 1 || result.Copied != 0 {
		t.Errorf("result = %+v, want the unknown digest reported missing", result)
	}
}
