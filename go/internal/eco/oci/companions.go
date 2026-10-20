package oci

import (
	"encoding/json"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/blob"
)

// companions keeps an image whole in the project it is copied to.
//
// What a person selects is a tag's manifest. What a pull needs beside it is the config
// and every layer that manifest lists — and for a multi-platform index, each platform's
// manifest, whose own layers are found by asking this again about those. Only manifests
// are read; a layer names nothing.
func companions(key string, read func() ([]byte, error)) ([]string, error) {
	// A pushed image's parts are pushed too, and go under the same hosted/ prefix: copying
	// one between projects must not leave its layers behind as evictable cached copies.
	hosted := hostedKey(key)
	bare := strings.TrimPrefix(key, hostedPrefix)
	if !strings.HasPrefix(bare, "manifest/") && !strings.HasPrefix(bare, "tag/") {
		return nil, nil
	}
	body, err := read()
	if err != nil {
		return nil, err
	}
	var keys []string
	if hosted {
		// Every child, attestations included: a push had to send them all to be accepted.
		for _, child := range indexDigests(body) {
			keys = append(keys, hostedManifestKey(child))
		}
		for _, digest := range imageBlobs(body) {
			keys = append(keys, hostedBlobKey(digest))
		}
		return keys, nil
	}
	for _, child := range indexChildren(body) {
		keys = append(keys, manifestKey(child.digest))
	}
	for _, digest := range imageBlobs(body) {
		keys = append(keys, blobKey(digest))
	}
	return keys, nil
}

// imageBlobs is every blob an image manifest refers to: its config and its layers.
func imageBlobs(body []byte) []blob.Digest {
	var doc struct {
		Config *struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil
	}
	var out []blob.Digest
	if doc.Config != nil {
		if digest, err := blob.ParseDigest(doc.Config.Digest); err == nil {
			out = append(out, digest)
		}
	}
	for _, layer := range doc.Layers {
		if digest, err := blob.ParseDigest(layer.Digest); err == nil {
			out = append(out, digest)
		}
	}
	return out
}
