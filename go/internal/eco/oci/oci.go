// Package oci implements an OCI Distribution pull-through cache.
//
// Tags are mutable refs, while manifests, configs and layers are immutable
// sha256-addressed content. The first image-name segment selects the upstream
// registry (dockerhub, ghcr, quay); project routing is handled above this adapter.
package oci

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brightskies/pkgreg/internal/blob"
	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/eco"
	"github.com/brightskies/pkgreg/internal/engine"
	"github.com/brightskies/pkgreg/internal/router"
)

const (
	// ID is the ecosystem identifier.
	ID = "oci"

	manifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	pendingLifetime   = 10 * time.Minute
	maxPendingDigests = 4096
)

const manifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.docker.distribution.manifest.v1+json"

var defaultRegistries = map[string]string{
	"dockerhub": "https://registry-1.docker.io",
	"ghcr":      "https://ghcr.io",
	"quay":      "https://quay.io",
}

// Repo is the OCI ecosystem.
type Repo struct {
	registries map[string]string

	pendingMu sync.Mutex
	pending   map[string][]pendingImage
}

type pendingImage struct {
	name, version, arch string
	indexDigest         blob.Digest
	origin              string
	expires             time.Time
}

// New builds an OCI adapter for Docker Hub, GHCR, and Quay.
func New() *Repo { return NewWithRegistries(defaultRegistries) }

// NewWithRegistries builds an adapter with an explicit alias-to-origin map.
func NewWithRegistries(registries map[string]string) *Repo {
	copied := make(map[string]string, len(registries))
	for alias, origin := range registries {
		alias = strings.Trim(strings.ToLower(alias), "/")
		if alias != "" && origin != "" {
			copied[alias] = strings.TrimRight(origin, "/")
		}
	}
	return &Repo{registries: copied, pending: make(map[string][]pendingImage)}
}

// Descriptor implements eco.Ecosystem.
func (r *Repo) Descriptor() eco.Descriptor {
	return eco.Descriptor{
		ID:               ID,
		Display:          "OCI / Docker",
		Summary:          "OCI image manifests, configs, and layers.",
		Storage:          eco.StorageBlob,
		Listener:         eco.ListenerProtocolRooted,
		Upstreams:        eco.UpstreamNamedSet,
		DefaultUpstreams: cloneMap(r.registries),
		Freshness: func(key string) eco.Freshness {
			if strings.HasPrefix(key, "tag/") || strings.HasPrefix(key, "tags/") {
				return eco.Revalidate(0)
			}
			return eco.Immutable
		},
		Setup: setupSteps,
	}
}

// Routes implements eco.Ecosystem.
func (r *Repo) Routes() []eco.Route {
	return []eco.Route{
		{Methods: []string{http.MethodGet, http.MethodHead}, Pattern: "/v2/", Handler: r.version},
		{Methods: []string{http.MethodGet, http.MethodHead}, Pattern: "/v2/{name...}/manifests/{ref}", Handler: r.manifest},
		{Methods: []string{http.MethodGet, http.MethodHead}, Pattern: "/v2/{name...}/blobs/{digest}", Handler: r.blob},
		{Methods: []string{http.MethodGet}, Pattern: "/v2/{name...}/tags/list", Handler: r.tagsList},
	}
}

func (r *Repo) version(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	c.W.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	c.W.WriteHeader(http.StatusOK)
}

func (r *Repo) manifest(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	route, ok := resolveName(c, p.Unescape("name"))
	if !ok {
		writeOCIError(c, http.StatusNotFound, "NAME_UNKNOWN", "unknown registry prefix or repository")
		return
	}
	ref := p.Unescape("ref")
	if ref == "" || strings.Contains(ref, "/") {
		writeOCIError(c, http.StatusNotFound, "MANIFEST_UNKNOWN", "invalid manifest reference")
		return
	}
	if strings.HasPrefix(ref, "sha256:") {
		r.manifestByDigest(c, route, ref)
		return
	}
	if strings.Contains(ref, ":") {
		writeOCIError(c, http.StatusBadRequest, "DIGEST_INVALID", "unsupported manifest digest")
		return
	}
	r.manifestByTag(c, route, ref)
}

func (r *Repo) manifestByTag(c *eco.Ctx, route imageRoute, tag string) {
	headers := http.Header{}
	headers.Set("Accept", requestAccept(c.R))
	refName := tagRefName(route.alias, route.repo, tag)
	upstreamURL := manifestURL(route, tag)
	doc, err := c.Document(engine.DocSpec{
		Name: refName, Key: refName, URL: upstreamURL,
		TTL: 0, Headers: headers,
	})
	if err != nil {
		writeEngineError(c, err, "MANIFEST_UNKNOWN")
		return
	}

	digest := doc.Digest
	mediaType := chooseMediaType(doc.MediaType, doc.Body)
	if _, err := c.PutBytes(manifestKey(digest), doc.Body, mediaType); err != nil {
		c.WriteError(err)
		return
	}
	if err := r.recordTag(c, route, tag, digest, mediaType, doc.Body, upstreamURL); err != nil {
		c.WriteError(err)
		return
	}
	writeManifest(c, doc.Body, mediaType, digest)
}

func (r *Repo) manifestByDigest(c *eco.Ctx, route imageRoute, rawDigest string) {
	digest, err := blob.ParseDigest(rawDigest)
	if err != nil {
		writeOCIError(c, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
		return
	}
	headers := http.Header{}
	headers.Set("Accept", requestAccept(c.R))
	upstreamURL := manifestURL(route, digest.Prefixed())
	doc, err := c.Document(engine.DocSpec{
		Name:      manifestKey(digest),
		Key:       manifestKey(digest),
		URL:       upstreamURL,
		Immutable: true,
		Expect:    engine.Expect{Digest: digest},
		Headers:   headers,
	})
	if err != nil {
		writeEngineError(c, err, "MANIFEST_UNKNOWN")
		return
	}
	mediaType := chooseMediaType(doc.MediaType, doc.Body)
	if err := r.recordDigest(c, route, digest, doc.Body, upstreamURL); err != nil {
		c.WriteError(err)
		return
	}
	writeManifest(c, doc.Body, mediaType, digest)
}

func (r *Repo) blob(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	route, ok := resolveName(c, p.Unescape("name"))
	if !ok {
		writeOCIError(c, http.StatusNotFound, "NAME_UNKNOWN", "unknown registry prefix or repository")
		return
	}
	digest, err := blob.ParseDigest(p.Unescape("digest"))
	if err != nil {
		writeOCIError(c, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
		return
	}
	headers := http.Header{}
	headers.Set("Docker-Content-Digest", digest.Prefixed())
	headers.Set("Docker-Distribution-API-Version", "registry/2.0")
	err = c.Serve(engine.Resolution{
		Key:       blobKey(digest),
		Upstream:  c.UpstreamRequest(blobURL(route, digest), nil),
		Expect:    engine.Expect{Digest: digest},
		MediaType: "application/octet-stream",
		Headers:   headers,
	})
	if err != nil {
		writeEngineError(c, err, "BLOB_UNKNOWN")
	}
}

func (r *Repo) tagsList(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	route, ok := resolveName(c, p.Unescape("name"))
	if !ok {
		writeOCIError(c, http.StatusNotFound, "NAME_UNKNOWN", "unknown registry prefix or repository")
		return
	}

	if c.Offline() {
		refs, err := c.ListRefs(tagRefPrefix(route.alias, route.repo))
		if err != nil {
			c.WriteError(err)
			return
		}
		tags := make([]string, 0, len(refs))
		prefix := tagRefPrefix(route.alias, route.repo)
		for _, ref := range refs {
			tag := strings.TrimPrefix(ref.Name, prefix)
			if tag != "" && !strings.Contains(tag, "/") {
				tags = append(tags, tag)
			}
		}
		sort.Strings(tags)
		c.W.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		_ = c.JSON(http.StatusOK, map[string]any{"name": clientName(c, route), "tags": tags})
		return
	}

	query := c.R.URL.RawQuery
	name := "tags/" + route.alias + "/" + route.repo
	if query != "" {
		name += "?" + query
	}
	upstreamURL := tagsURL(route, query)
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	doc, err := c.Document(engine.DocSpec{
		Name: name, Key: name, URL: upstreamURL, TTL: 0, Headers: headers,
	})
	if err != nil {
		writeEngineError(c, err, "NAME_UNKNOWN")
		return
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(doc.Body, &payload); err != nil {
		_ = c.Text(http.StatusBadGateway, "upstream returned an invalid tags document")
		return
	}
	payload["name"], _ = json.Marshal(clientName(c, route))
	c.W.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if err := c.JSON(http.StatusOK, payload); err != nil {
		c.WriteError(err)
	}
}

func (r *Repo) recordTag(
	c *eco.Ctx,
	route imageRoute,
	tag string,
	digest blob.Digest,
	mediaType string,
	body []byte,
	origin string,
) error {
	name := route.display
	if err := c.DeleteArtifactVersion(name, tag); err != nil {
		return err
	}
	children := indexChildren(body)
	if len(children) > 0 {
		for _, child := range children {
			if err := c.RecordArtifact(catalog.Artifact{
				Name: name, Version: tag, Arch: child.arch,
				Digest: digest, Size: int64(len(body)), Origin: origin,
				Extra: map[string]any{
					"media_type":   mediaType,
					"child_digest": child.digest.Prefixed(),
				},
			}); err != nil {
				return err
			}
			r.addPending(c.Project, child.digest, pendingImage{
				name: name, version: tag, arch: child.arch,
				indexDigest: digest, origin: origin,
			})
		}
		return nil
	}

	size, image := imageSize(body)
	if !image {
		size = int64(len(body))
	}
	if err := c.RecordArtifact(catalog.Artifact{
		Name: name, Version: tag, Digest: digest, Size: size, Origin: origin,
		Extra: map[string]any{"media_type": mediaType},
	}); err != nil {
		return err
	}
	if image {
		// Docker commonly requests the same manifest by digest immediately after the
		// tag. Remember that it belongs to the tag so this does not create a duplicate
		// digest-version inventory row.
		r.addPending(c.Project, digest, pendingImage{
			name: name, version: tag, indexDigest: digest, origin: origin,
		})
	}
	return nil
}

func (r *Repo) recordDigest(
	c *eco.Ctx,
	route imageRoute,
	digest blob.Digest,
	body []byte,
	origin string,
) error {
	size, image := imageSize(body)
	parents := r.takePending(c.Project, digest)
	if len(parents) > 0 {
		if !image {
			return nil
		}
		for _, parent := range parents {
			if err := c.RecordArtifact(catalog.Artifact{
				Name: parent.name, Version: parent.version, Arch: parent.arch,
				Digest: parent.indexDigest, Size: size, Origin: parent.origin,
			}); err != nil {
				return err
			}
		}
		return nil
	}

	children := indexChildren(body)
	if len(children) > 0 {
		version := digest.Prefixed()
		if err := c.DeleteArtifactVersion(route.display, version); err != nil {
			return err
		}
		for _, child := range children {
			if err := c.RecordArtifact(catalog.Artifact{
				Name: route.display, Version: version, Arch: child.arch,
				Digest: digest, Size: int64(len(body)), Origin: origin,
				Extra: map[string]any{"child_digest": child.digest.Prefixed()},
			}); err != nil {
				return err
			}
			r.addPending(c.Project, child.digest, pendingImage{
				name: route.display, version: version, arch: child.arch,
				indexDigest: digest, origin: origin,
			})
		}
		return nil
	}
	if image {
		return c.RecordArtifact(catalog.Artifact{
			Name: route.display, Version: digest.Prefixed(),
			Digest: digest, Size: size, Origin: origin,
		})
	}
	return nil
}

func (r *Repo) addPending(project string, digest blob.Digest, pending pendingImage) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	r.prunePendingLocked(time.Now())
	if len(r.pending) >= maxPendingDigests {
		for key := range r.pending {
			delete(r.pending, key)
			break
		}
	}
	pending.expires = time.Now().Add(pendingLifetime)
	key := project + "\x00" + digest.String()
	for _, current := range r.pending[key] {
		if current.name == pending.name && current.version == pending.version &&
			current.arch == pending.arch {
			return
		}
	}
	r.pending[key] = append(r.pending[key], pending)
}

func (r *Repo) takePending(project string, digest blob.Digest) []pendingImage {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	r.prunePendingLocked(time.Now())
	key := project + "\x00" + digest.String()
	out := r.pending[key]
	delete(r.pending, key)
	return out
}

func (r *Repo) prunePendingLocked(now time.Time) {
	for key, values := range r.pending {
		kept := values[:0]
		for _, value := range values {
			if now.Before(value.expires) {
				kept = append(kept, value)
			}
		}
		if len(kept) == 0 {
			delete(r.pending, key)
		} else {
			r.pending[key] = kept
		}
	}
}

type imageRoute struct {
	base, alias, repo, display string
}

// resolveName maps an OCI repository path onto an upstream and a repository.
//
// The first segment normally selects the upstream, because one cache fronts several
// registries. A daemon configured with registry-mirrors knows nothing of that
// namespace — it asks for /v2/library/alpine — so when the first segment is not a
// known upstream, and only when the operator has named a default, the whole path is
// treated as a repository on that default. That fallback is what turns
// "cache:8443/dockerhub/library/alpine:3.20" in a Dockerfile back into "alpine:3.20".
//
// The order matters: a real alias always wins, so enabling the mirror can never
// change the meaning of a path that already worked.
func resolveName(c *eco.Ctx, raw string) (imageRoute, bool) {
	raw = strings.Trim(raw, "/")
	parts := strings.Split(raw, "/")
	if len(parts) == 0 || raw == "" {
		return imageRoute{}, false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, '\x00') {
			return imageRoute{}, false
		}
	}
	alias := strings.ToLower(parts[0])
	base, ok := c.Upstream(alias)
	if ok && len(parts) < 2 {
		// "/v2/dockerhub/manifests/..." names an upstream and no repository.
		return imageRoute{}, false
	}
	if !ok {
		mirror := strings.ToLower(strings.TrimSpace(c.RegistryMirror()))
		if mirror == "" {
			return imageRoute{}, false
		}
		if base, ok = c.Upstream(mirror); !ok {
			return imageRoute{}, false
		}
		alias = mirror
		parts = append([]string{mirror}, parts...)
	}
	requestedRepo := strings.Join(parts[1:], "/")
	upstreamRepo := requestedRepo
	if alias == "dockerhub" && !strings.Contains(requestedRepo, "/") {
		upstreamRepo = "library/" + requestedRepo
	}
	return imageRoute{
		base: strings.TrimRight(base, "/"), alias: alias,
		repo: upstreamRepo, display: alias + "/" + requestedRepo,
	}, true
}

type childManifest struct {
	digest blob.Digest
	arch   string
}

func indexChildren(body []byte) []childManifest {
	var doc struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil
	}
	var out []childManifest
	for _, manifest := range doc.Manifests {
		if manifest.Platform.Architecture == "unknown" {
			continue
		}
		digest, err := blob.ParseDigest(manifest.Digest)
		if err != nil {
			continue
		}
		out = append(out, childManifest{digest: digest, arch: manifest.Platform.Architecture})
	}
	return out
}

func imageSize(body []byte) (int64, bool) {
	var doc struct {
		Config *struct {
			Size int64 `json:"size"`
		} `json:"config"`
		Layers []struct {
			Size int64 `json:"size"`
		} `json:"layers"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Layers == nil {
		return 0, false
	}
	var total int64
	if doc.Config != nil && doc.Config.Size > 0 {
		total = doc.Config.Size
	}
	for _, layer := range doc.Layers {
		if layer.Size < 0 || total > math.MaxInt64-layer.Size {
			return 0, false
		}
		total += layer.Size
	}
	return total, true
}

func requestAccept(r *http.Request) string {
	if value := r.Header.Get("Accept"); value != "" && value != "*/*" {
		return value
	}
	return manifestAccept
}

func chooseMediaType(header string, body []byte) string {
	if value := strings.TrimSpace(strings.Split(header, ";")[0]); value != "" {
		return value
	}
	var doc struct {
		MediaType string `json:"mediaType"`
	}
	if json.Unmarshal(body, &doc) == nil && doc.MediaType != "" {
		return doc.MediaType
	}
	return manifestMediaType
}

func writeManifest(c *eco.Ctx, body []byte, mediaType string, digest blob.Digest) {
	c.W.Header().Set("Docker-Content-Digest", digest.Prefixed())
	c.W.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	c.W.Header().Set("Content-Length", fmt.Sprint(len(body)))
	c.W.Header().Set("ETag", `"`+digest.Prefixed()+`"`)
	if err := c.ServeBytes(http.StatusOK, mediaType, body); err != nil {
		c.WriteError(err)
	}
}

func writeOCIError(c *eco.Ctx, status int, code, message string) {
	c.W.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	_ = c.JSON(status, map[string]any{
		"errors": []map[string]string{{"code": code, "message": message}},
	})
}

func writeEngineError(c *eco.Ctx, err error, notFoundCode string) {
	if status, ok := engine.UpstreamStatus(err); ok {
		code := "UNKNOWN"
		if status == http.StatusNotFound {
			code = notFoundCode
		}
		writeOCIError(c, status, code, http.StatusText(status))
		return
	}
	c.WriteError(err)
}

func manifestURL(route imageRoute, ref string) string {
	return eco.JoinURL(route.base,
		"v2/"+escapeRepo(route.repo)+"/manifests/"+url.PathEscape(ref))
}

func blobURL(route imageRoute, digest blob.Digest) string {
	return eco.JoinURL(route.base,
		"v2/"+escapeRepo(route.repo)+"/blobs/"+url.PathEscape(digest.Prefixed()))
}

func tagsURL(route imageRoute, query string) string {
	raw := eco.JoinURL(route.base, "v2/"+escapeRepo(route.repo)+"/tags/list")
	if query != "" {
		raw += "?" + query
	}
	return raw
}

func escapeRepo(repo string) string {
	parts := strings.Split(repo, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func tagRefName(alias, repo, tag string) string {
	return tagRefPrefix(alias, repo) + tag
}

func tagRefPrefix(alias, repo string) string { return "tag/" + alias + "/" + repo + "/" }
func manifestKey(digest blob.Digest) string  { return "manifest/" + digest.String() }
func blobKey(digest blob.Digest) string      { return "blob/" + digest.String() }

func clientName(c *eco.Ctx, route imageRoute) string {
	if c.Project == config.GlobalProject {
		return route.display
	}
	return c.Project + "/" + route.display
}

func setupSteps(ctx eco.SetupContext) []eco.SetupStep {
	prefix := "dockerhub/library/alpine:3.20"
	if !ctx.IsGlobal {
		prefix = ctx.Project + "/" + prefix
	}
	// Two commands, because Docker has two situations and only one of them looks like
	// every other ecosystem. The daemon does not read the shell the client configured,
	// so the first form works only when that daemon shares this machine's loopback
	// interface. Naming the second address here rather than only saying "use persistent
	// setup" is the difference between guidance and an instruction: the reader on Docker
	// Desktop needs a command, and it is one we can compute.
	return []eco.SetupStep{
		{Comment: "Docker runs as a separate daemon. From the pkgreg shell, with Docker on this same machine:"},
		{Command: `docker pull "$PKGREG_DOCKER_REGISTRY/` + prefix + `"`},
		{Comment: "Docker Desktop, a remote builder and CI cannot reach that loopback bridge. On a host where the cache's CA is installed (persistent setup), pull from the cache directly — this address is stable, so it can go in a Dockerfile:"},
		{Command: "docker pull " + eco.ClientAuthority(ctx.Host, ctx.Port) + "/" + prefix},
	}
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
