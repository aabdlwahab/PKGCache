package oci

import (
	"slices"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/blob"
)

const (
	configDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	layerDigest  = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	childDigest  = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestAnImageTravelsWithItsConfigAndLayers(t *testing.T) {
	image := []byte(`{"schemaVersion":2,"config":{"digest":"` + configDigest + `","size":10},
		"layers":[{"digest":"` + layerDigest + `","size":20}]}`)
	got, err := companions("tag/docker.io/library/alpine/3.20", func() ([]byte, error) { return image, nil })
	if err != nil {
		t.Fatal(err)
	}
	// Keyed exactly as the adapter keys what it stores, which is the whole requirement.
	want := []string{blobKey(mustDigest(t, configDigest)), blobKey(mustDigest(t, layerDigest))}
	if !slices.Equal(got, want) {
		t.Errorf("image companions = %q, want %q", got, want)
	}
}

func TestAnIndexTravelsWithEachPlatformsManifest(t *testing.T) {
	index := []byte(`{"schemaVersion":2,"manifests":[
		{"digest":"` + childDigest + `","platform":{"architecture":"amd64","os":"linux"}}]}`)
	got, err := companions("manifest/sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", func() ([]byte, error) { return index, nil })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{manifestKey(mustDigest(t, childDigest))}; !slices.Equal(got, want) {
		t.Errorf("index companions = %q, want %q", got, want)
	}
}

func TestALayerIsNotRead(t *testing.T) {
	got, err := companions("blob/"+layerDigest, func() ([]byte, error) {
		t.Fatal("a layer's bytes were read to find companions it cannot have")
		return nil, nil
	})
	if err != nil || got != nil {
		t.Errorf("layer companions = %q, %v", got, err)
	}
}

func mustDigest(t *testing.T, raw string) blob.Digest {
	t.Helper()
	digest, err := blob.ParseDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
