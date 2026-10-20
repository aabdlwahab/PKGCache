package oci

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/engine"
	"github.com/aabdlwahab/PKGCache/internal/ociname"
	"github.com/aabdlwahab/PKGCache/internal/router"
)

// Images pushed to this instance.
//
// A pull-through cache has nowhere to put an image somebody built: everything it holds is a
// copy of a registry's, under that registry's name. Pushed images get a namespace of their
// own, local/, named by the first segment of the image the way every registry is, and it
// reaches no upstream — on a team server it is the team's own registry, on a machine's cache
// that machine's. A cache pointed at a team pulls the team's local/ behind its own, so an
// image pushed to the team from one machine is a pull on every other.
//
// What is pushed is stored under hosted/ keys, apart from the cached copies under manifest/
// and blob/: one is the only copy anywhere and eviction must never take it (Descriptor.
// Hosted); the other can be fetched again. A layer that is both — a pushed image's base
// layers are usually cached already — costs nothing to hold twice, because both keys name
// one blob.

const (
	// HostedNamespace is the image-name segment pushed images live under:
	// <cache>/local/<repository>:<tag>, with a project before it as for any image.
	HostedNamespace = ociname.Hosted

	hostedPrefix = "hosted/"
	// maxManifestBytes bounds a pushed manifest. The distribution spec asks registries to
	// take at least 4 MiB; an index naming every platform is a few kilobytes.
	maxManifestBytes = 4 << 20
	layerMediaType   = "application/octet-stream"
)

// PushPolicy decides who may push.
type PushPolicy struct {
	// Tokens verifies a project's write token. A team server takes pushes only with one.
	Tokens TokenVerifier
	// Open takes a push from anybody who can connect, with no token: a machine's own cache,
	// whose loopback socket is the boundary its control API already relies on. The sibling
	// surface other machines reach passes GET and HEAD only, so no push arrives through it.
	Open bool
}

// TokenVerifier is the control plane's tokens as this adapter needs them.
type TokenVerifier interface {
	// HasToken reports whether any token could satisfy the scope.
	HasToken(project, eco, scope string) bool
	// VerifyToken checks one presented token.
	VerifyToken(project, eco, scope, presented string) bool
}

// WithPush sets who may push. Without it nothing can be.
func (r *Repo) WithPush(p PushPolicy) *Repo {
	r.push = p
	return r
}

func (r *Repo) pushRoutes() []eco.Route {
	return []eco.Route{
		// Both spellings: the spec's has the slash, and a client that drops it should not
		// be told the registry has no uploads.
		{Methods: []string{http.MethodPost}, Pattern: "/v2/{name...}/blobs/uploads/", Handler: r.startUpload},
		{Methods: []string{http.MethodPost}, Pattern: "/v2/{name...}/blobs/uploads", Handler: r.startUpload},
		{Methods: []string{http.MethodGet}, Pattern: "/v2/{name...}/blobs/uploads/{id}", Handler: r.uploadStatus},
		{Methods: []string{http.MethodPatch}, Pattern: "/v2/{name...}/blobs/uploads/{id}", Handler: r.appendUpload},
		{Methods: []string{http.MethodPut}, Pattern: "/v2/{name...}/blobs/uploads/{id}", Handler: r.finishUpload},
		{Methods: []string{http.MethodDelete}, Pattern: "/v2/{name...}/blobs/uploads/{id}", Handler: r.cancelUpload},
		{Methods: []string{http.MethodPut}, Pattern: "/v2/{name...}/manifests/{ref}", Handler: r.putManifest},
	}
}

// ---- uploads ----------------------------------------------------------------

func (r *Repo) startUpload(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	route, ok := r.pushRoute(c, p.Unescape("name"))
	if !ok {
		return
	}
	query := c.R.URL.Query()
	// A layer the store already holds — pushed under another name, or a base layer this
	// machine pulled — is mounted rather than sent again. Where the client says it came from
	// does not matter: content is addressed by its digest, and nothing a client could name is
	// more trustworthy than that.
	if mount := query.Get("mount"); mount != "" {
		if digest, err := blob.ParseDigest(mount); err == nil && adoptBlob(c, digest) {
			blobCreated(c, route, digest)
			return
		}
	}
	if raw := query.Get("digest"); raw != "" {
		// A monolithic upload: the whole layer in this one request.
		digest, err := blob.ParseDigest(raw)
		if err != nil {
			writeOCIError(c, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
			return
		}
		if _, err := c.PutBlob(hostedBlobKey(digest), c.R, engine.PutOptions{
			MediaType: layerMediaType, ExpectDigest: digest, Overwrite: true,
		}); err != nil {
			writeUploadError(c, err)
			return
		}
		blobCreated(c, route, digest)
		return
	}
	id, err := c.StartUpload()
	if err != nil {
		writeUploadError(c, err)
		return
	}
	uploadAccepted(c, route, id, 0, http.StatusAccepted)
}

func (r *Repo) appendUpload(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	route, ok := r.pushRoute(c, p.Unescape("name"))
	if !ok {
		return
	}
	id := p.Unescape("id")
	// A chunk says where it starts. One that does not start where the upload stands is a
	// client that lost track of what arrived, and appending it would stage a different
	// layer from the one it is about to name.
	if header := c.R.Header.Get("Content-Range"); header != "" {
		size, known := c.UploadSize(id)
		if !known {
			writeUploadError(c, engine.ErrUploadUnknown)
			return
		}
		if start, ok := rangeStart(header); !ok || start != size {
			c.W.Header().Set("Range", byteRange(size))
			writeOCIError(c, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID",
				fmt.Sprintf("the upload holds %d bytes; a chunk must start there", size))
			return
		}
	}
	size, err := c.AppendUpload(id, c.R.Body, 0)
	if err != nil {
		writeUploadError(c, err)
		return
	}
	uploadAccepted(c, route, id, size, http.StatusAccepted)
}

func (r *Repo) finishUpload(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	route, ok := r.pushRoute(c, p.Unescape("name"))
	if !ok {
		return
	}
	digest, err := blob.ParseDigest(c.R.URL.Query().Get("digest"))
	if err != nil {
		writeOCIError(c, http.StatusBadRequest, "DIGEST_INVALID", "closing an upload needs ?digest=sha256:<hex>")
		return
	}
	if _, err := c.CommitUpload(p.Unescape("id"), hostedBlobKey(digest), c.R.Body, engine.PutOptions{
		MediaType: layerMediaType, ExpectDigest: digest, Overwrite: true,
	}); err != nil {
		writeUploadError(c, err)
		return
	}
	blobCreated(c, route, digest)
}

func (r *Repo) uploadStatus(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	route, ok := r.pushRoute(c, p.Unescape("name"))
	if !ok {
		return
	}
	id := p.Unescape("id")
	size, known := c.UploadSize(id)
	if !known {
		writeUploadError(c, engine.ErrUploadUnknown)
		return
	}
	uploadAccepted(c, route, id, size, http.StatusNoContent)
}

func (r *Repo) cancelUpload(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	if _, ok := r.pushRoute(c, p.Unescape("name")); !ok {
		return
	}
	c.CancelUpload(p.Unescape("id"))
	c.W.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	c.W.WriteHeader(http.StatusNoContent)
}

// ---- manifests --------------------------------------------------------------

func (r *Repo) putManifest(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	route, ok := r.pushRoute(c, p.Unescape("name"))
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.R.Body, maxManifestBytes+1))
	if err != nil {
		writeOCIError(c, http.StatusBadRequest, "MANIFEST_INVALID", "the manifest did not arrive whole")
		return
	}
	if len(body) > maxManifestBytes {
		writeOCIError(c, http.StatusRequestEntityTooLarge, "SIZE_INVALID",
			fmt.Sprintf("a manifest may be at most %d bytes", maxManifestBytes))
		return
	}
	if !json.Valid(body) {
		writeOCIError(c, http.StatusBadRequest, "MANIFEST_INVALID", "the manifest is not JSON")
		return
	}
	digest := blob.DigestBytes(body)
	ref := p.Unescape("ref")
	tag := ""
	if strings.HasPrefix(ref, "sha256:") {
		want, err := blob.ParseDigest(ref)
		if err != nil || want != digest {
			writeOCIError(c, http.StatusBadRequest, "DIGEST_INVALID",
				"the manifest does not match the digest it was pushed under")
			return
		}
	} else {
		if !tagRE.MatchString(ref) {
			writeOCIError(c, http.StatusBadRequest, "TAG_INVALID", "invalid tag "+strconv.Quote(ref))
			return
		}
		tag = ref
	}
	// Everything it names has to be here already. A manifest whose layers are missing is an
	// image nobody can pull, and the push that sent it would have reported success.
	if missing, code := adoptReferences(c, body); missing != "" {
		writeOCIError(c, http.StatusBadRequest, code, "the manifest names "+missing+", which was not pushed")
		return
	}
	mediaType := chooseMediaType(c.R.Header.Get("Content-Type"), body)
	if _, err := c.PutBytes(hostedManifestKey(digest), body, mediaType); err != nil {
		c.WriteError(err)
		return
	}
	if tag != "" {
		if _, err := c.PutBytes(hostedTagKey(route.repo, tag), body, mediaType); err != nil {
			c.WriteError(err)
			return
		}
		if err := r.recordTag(c, route, tag, digest, mediaType, body, "pushed"); err != nil {
			c.WriteError(err)
			return
		}
	}
	h := c.W.Header()
	h.Set("Location", repositoryPath(c, route)+"/manifests/"+digest.Prefixed())
	h.Set("Docker-Content-Digest", digest.Prefixed())
	h.Set("Docker-Distribution-API-Version", "registry/2.0")
	h.Set("Content-Length", "0")
	c.W.WriteHeader(http.StatusCreated)
}

// adoptReferences files under hosted/ everything a manifest names, and reports the first
// thing it names that this instance does not hold, with the error code that says which.
func adoptReferences(c *eco.Ctx, body []byte) (missing, code string) {
	for _, child := range indexDigests(body) {
		if !adoptManifest(c, child) {
			return child.Prefixed(), "MANIFEST_UNKNOWN"
		}
	}
	for _, digest := range imageBlobs(body) {
		if !adoptBlob(c, digest) {
			return digest.Prefixed(), "MANIFEST_BLOB_UNKNOWN"
		}
	}
	return "", ""
}

// adoptBlob makes a layer part of what this instance hosts. One already in the store is
// linked, not copied; one that is not cannot be.
func adoptBlob(c *eco.Ctx, digest blob.Digest) bool {
	if _, held := c.Entry(hostedBlobKey(digest)); held {
		return true
	}
	return c.BlobExists(digest) && c.LinkBlob(hostedBlobKey(digest), digest, layerMediaType) == nil
}

// adoptManifest is adoptBlob for an index's child manifest, which may be one pushed a moment
// ago or one this instance cached from a registry.
func adoptManifest(c *eco.Ctx, digest blob.Digest) bool {
	if _, held := c.Entry(hostedManifestKey(digest)); held {
		return true
	}
	cached, found := c.Entry(manifestKey(digest))
	return found && c.LinkBlob(hostedManifestKey(digest), cached.Digest, cached.MediaType) == nil
}

// ---- serving what was pushed ------------------------------------------------

// serveHostedManifest answers a manifest this instance holds, reporting whether it did.
func serveHostedManifest(c *eco.Ctx, route imageRoute, ref string) bool {
	key := hostedTagKey(route.repo, ref)
	if strings.HasPrefix(ref, "sha256:") {
		digest, err := blob.ParseDigest(ref)
		if err != nil {
			return false
		}
		key = hostedManifestKey(digest)
	}
	body, entry, err := c.ReadEntry(key, maxManifestBytes)
	if err != nil {
		return false
	}
	writeManifest(c, body, chooseMediaType(entry.MediaType, body), entry.Digest)
	return true
}

// serveHostedBlob answers a layer under local/, reporting whether it did.
//
// What this instance holds comes first: a pushed layer, or one the store has for any other
// reason, which is what lets a push skip uploading a base layer it already holds. A HEAD
// stops there. It is how a pusher asks whether to send a layer, and the answer must not
// depend on whether the team behind this cache is reachable at that moment: a 404 sends the
// layer, and a failed lookup would fail the push. A GET goes on to the team, if there is one.
func serveHostedBlob(c *eco.Ctx, route imageRoute, digest blob.Digest, headers http.Header) bool {
	res := engine.Resolution{
		Key: hostedBlobKey(digest), Expect: engine.Expect{Digest: digest},
		MediaType: layerMediaType, Headers: headers,
	}
	if _, held := c.Entry(res.Key); held {
		if err := c.Serve(res); err != nil {
			c.WriteError(err)
		}
		return true
	}
	if c.BlobExists(digest) {
		res.Key = blobKey(digest)
		if err := c.Serve(res); err != nil {
			c.WriteError(err)
		}
		return true
	}
	if route.base == "" || c.R.Method == http.MethodHead {
		writeOCIError(c, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to this registry")
		return true
	}
	return false
}

// serveHostedTags lists the tags pushed to a repository here, reporting whether it did.
// One this instance holds nothing for is the team's to answer, when there is a team.
func serveHostedTags(c *eco.Ctx, route imageRoute) bool {
	entries, err := c.ListEntries(hostedTagPrefix(route.repo))
	if err != nil {
		c.WriteError(err)
		return true
	}
	tags := make([]string, 0, len(entries))
	for _, entry := range entries {
		if tag := strings.TrimPrefix(entry.Key, hostedTagPrefix(route.repo)); tag != "" && !strings.Contains(tag, "/") {
			tags = append(tags, tag)
		}
	}
	if len(tags) == 0 && route.base != "" {
		return false
	}
	sort.Strings(tags)
	c.W.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	_ = c.JSON(http.StatusOK, map[string]any{"name": clientName(c, route), "tags": tags})
	return true
}

// hostedUpstream is where local/ is pulled from when this instance does not hold an image:
// the team cache's own local/, when this one is chained to a team, and nowhere otherwise.
// The chain's wildcard entry is the team's /v2 root (see discoveredOrigin), so its local/
// is one more segment on it, as a discovered registry is.
func hostedUpstream(c *eco.Ctx) string {
	if chain := c.UpstreamChain(ociname.AnyRegistry); len(chain) > 0 {
		return strings.TrimRight(chain[0].URL, "/") + "/" + HostedNamespace
	}
	return ""
}

// ---- who may push -----------------------------------------------------------

// pushRoute resolves a push's image name and decides whether it may go ahead, answering the
// client itself when it may not.
func (r *Repo) pushRoute(c *eco.Ctx, raw string) (imageRoute, bool) {
	if !r.authorizePush(c) {
		return imageRoute{}, false
	}
	route, ok := resolveName(c, raw)
	if !ok || !route.hosted {
		writeOCIError(c, http.StatusForbidden, "DENIED",
			"this cache keeps pushed images under local/: push to <cache>/local/<name>:<tag>")
		return imageRoute{}, false
	}
	if !repositoryRE.MatchString(route.repo) {
		writeOCIError(c, http.StatusBadRequest, "NAME_INVALID", "invalid repository name "+strconv.Quote(route.repo))
		return imageRoute{}, false
	}
	return route, true
}

func (r *Repo) authorizePush(c *eco.Ctx) bool {
	if c.Offline() {
		writeOCIError(c, http.StatusForbidden, "DENIED",
			"read-only: this project is offline, and an image pushed to it would be lost at the next import")
		return false
	}
	if r.push.Open {
		return true
	}
	if r.push.Tokens == nil || !r.push.Tokens.HasToken(c.Project, ID, "write") {
		writeOCIError(c, http.StatusForbidden, "DENIED",
			"this project has no write token: create one in the console (Connect, Access tokens, scope write)")
		return false
	}
	if !r.push.Tokens.VerifyToken(c.Project, ID, "write", pushCredential(c.R)) {
		c.W.Header().Set("WWW-Authenticate", `Basic realm="pkgreg"`)
		writeOCIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "pushing needs a write token for this project")
		return false
	}
	return true
}

// pushCredential is the token a push presents: as a bearer token, which is how pkgcache
// sends a machine's, or as the password of Basic auth, which is how `docker login` stores
// one. The username is not checked; a token names its own project and scope.
func pushCredential(req *http.Request) string {
	scheme, value, _ := strings.Cut(req.Header.Get("Authorization"), " ")
	if strings.EqualFold(scheme, "bearer") {
		return strings.TrimSpace(value)
	}
	if _, password, ok := req.BasicAuth(); ok {
		return password
	}
	return ""
}

// ---- answers ----------------------------------------------------------------

func uploadAccepted(c *eco.Ctx, route imageRoute, id string, size int64, status int) {
	h := c.W.Header()
	h.Set("Location", repositoryPath(c, route)+"/blobs/uploads/"+id)
	h.Set("Docker-Upload-UUID", id)
	h.Set("Range", byteRange(size))
	h.Set("Docker-Distribution-API-Version", "registry/2.0")
	h.Set("Content-Length", "0")
	c.W.WriteHeader(status)
}

func blobCreated(c *eco.Ctx, route imageRoute, digest blob.Digest) {
	h := c.W.Header()
	h.Set("Location", repositoryPath(c, route)+"/blobs/"+digest.Prefixed())
	h.Set("Docker-Content-Digest", digest.Prefixed())
	h.Set("Docker-Distribution-API-Version", "registry/2.0")
	h.Set("Content-Length", "0")
	c.W.WriteHeader(http.StatusCreated)
}

func writeUploadError(c *eco.Ctx, err error) {
	switch {
	case errors.Is(err, engine.ErrUploadUnknown):
		writeOCIError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "no such upload; start the layer again")
	case errors.Is(err, engine.ErrChecksum):
		writeOCIError(c, http.StatusBadRequest, "DIGEST_INVALID", "the layer does not match its digest")
	case errors.Is(err, engine.ErrReadOnly):
		writeOCIError(c, http.StatusForbidden, "DENIED", "read-only: this project is offline")
	case errors.Is(err, engine.ErrTooLarge):
		writeOCIError(c, http.StatusRequestEntityTooLarge, "SIZE_INVALID", err.Error())
	default:
		c.WriteError(err)
	}
}

// repositoryPath is a repository's API path as the client names it, project included, for
// the Location headers that tell a client where to go next.
func repositoryPath(c *eco.Ctx, route imageRoute) string {
	return "/v2/" + router.OCIProjectPrefix(c.Project) + route.display
}

// byteRange is the Range header of an upload holding size bytes. The distribution spec
// writes an empty one as 0-0.
func byteRange(size int64) string {
	if size <= 0 {
		return "0-0"
	}
	return "0-" + strconv.FormatInt(size-1, 10)
}

// rangeStart reads where a chunk's Content-Range says it starts: "0-1023", as the spec
// writes it, or "bytes 0-1023/2048", as HTTP does.
func rangeStart(header string) (int64, bool) {
	header = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "bytes"))
	start, _, ok := strings.Cut(header, "-")
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(start), 10, 64)
	return value, err == nil && value >= 0
}

// ---- keys and names ---------------------------------------------------------

func hostedKey(key string) bool              { return strings.HasPrefix(key, hostedPrefix) }
func hostedBlobKey(d blob.Digest) string     { return hostedPrefix + blobKey(d) }
func hostedManifestKey(d blob.Digest) string { return hostedPrefix + manifestKey(d) }
func hostedTagPrefix(repo string) string     { return hostedPrefix + "tag/" + repo + "/" }
func hostedTagKey(repo, tag string) string   { return hostedTagPrefix(repo) + tag }

// The distribution spec's grammar for a repository path and a tag.
var (
	repositoryRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)
	tagRE        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// indexDigests is every manifest an index names, whatever its platform. Inventory skips the
// attestation entries buildx files as unknown/unknown; a push still has to have sent them.
func indexDigests(body []byte) []blob.Digest {
	var doc struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil
	}
	out := make([]blob.Digest, 0, len(doc.Manifests))
	for _, manifest := range doc.Manifests {
		if digest, err := blob.ParseDigest(manifest.Digest); err == nil {
			out = append(out, digest)
		}
	}
	return out
}
