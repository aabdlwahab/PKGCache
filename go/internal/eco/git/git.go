package git

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/engine"
	"github.com/aabdlwahab/PKGCache/internal/router"
	"github.com/aabdlwahab/PKGCache/internal/upstream"
)

const (
	// ID is the ecosystem identifier.
	ID = "git"

	defaultRefsTTL        = 60 * time.Second
	defaultMaxUploadPacks = 8
	maxNegotiationBytes   = 64 << 20
	maxLFSBatchBytes      = 1 << 20
	pendingLFSLifetime    = 10 * time.Minute
	maxPendingLFS         = 4096

	lfsMediaType = "application/vnd.git-lfs+json"
)

var serviceAdvertisement = []byte("001e# service=git-upload-pack\n0000")

// Options configures the Git adapter. Resolver hooks make real-client acceptance
// tests deterministic without weakening the production URL scheme.
type Options struct {
	GitPath         string
	RefsTTL         time.Duration
	MaxUploadPacks  int
	ResolveUpstream func(repo string) (string, error)
	LFSBatchURL     func(repo, upstream string) string
}

// Repo is the read-only Git ecosystem.
type Repo struct {
	mirrors *mirrorManager
	resolve func(string) (string, error)
	lfsURL  func(string, string) string
	refsTTL time.Duration

	lfsMu   sync.Mutex
	pending map[string]pendingLFS
}

type pendingLFS struct {
	Href    string
	Headers http.Header
	Repo    string
	Origin  string
	Expires time.Time
}

// New builds the production Git adapter.
func New() *Repo { return NewWithOptions(Options{}) }

// NewWithConfig builds the production adapter from the process configuration.
func NewWithConfig(cfg config.Git) *Repo {
	return NewWithOptions(Options{
		RefsTTL:        cfg.RefsTTL,
		MaxUploadPacks: cfg.MaxUploadPacks,
	})
}

// NewWithOptions builds a Git adapter with explicit process and origin settings.
func NewWithOptions(o Options) *Repo {
	ttl := o.RefsTTL
	if ttl == 0 {
		ttl = defaultRefsTTL
	}
	maxPacks := o.MaxUploadPacks
	if maxPacks <= 0 {
		maxPacks = defaultMaxUploadPacks
	}
	resolve := o.ResolveUpstream
	if resolve == nil {
		resolve = func(repo string) (string, error) {
			return "https://" + repo + ".git", nil
		}
	}
	lfsURL := o.LFSBatchURL
	if lfsURL == nil {
		lfsURL = func(_ string, origin string) string {
			return strings.TrimRight(origin, "/") + "/info/lfs/objects/batch"
		}
	}
	return &Repo{
		mirrors: newMirrorManager(o.GitPath, ttl, maxPacks),
		resolve: resolve,
		lfsURL:  lfsURL,
		refsTTL: ttl,
		pending: make(map[string]pendingLFS),
	}
}

// Descriptor implements eco.Ecosystem.
func (r *Repo) Descriptor() eco.Descriptor {
	return eco.Descriptor{
		ID:        ID,
		Display:   "Git",
		Summary:   "Read-only smart-HTTP mirrors and Git LFS objects.",
		Storage:   eco.StorageManagedDir,
		Listener:  eco.ListenerPathPrefixed,
		Upstreams: eco.UpstreamNone,
		Freshness: func(string) eco.Freshness { return eco.Revalidate(r.refsTTL) },
		Setup:     setupSteps,
	}
}

// Routes implements eco.Ecosystem.
func (r *Repo) Routes() []eco.Route {
	return []eco.Route{
		{Methods: []string{http.MethodPost}, Pattern: "/+maintain", Handler: r.maintain, Admin: true},
		{Methods: []string{http.MethodGet, http.MethodHead}, Pattern: "/+lfs/{oid}", Handler: r.lfsGet},
		{Methods: []string{http.MethodPost}, Pattern: "/{repo...}/info/lfs/objects/batch", Handler: r.lfsBatch},
		{Methods: []string{http.MethodGet}, Pattern: "/{repo...}/info/refs", Handler: r.infoRefs},
		{Methods: []string{http.MethodPost}, Pattern: "/{repo...}/git-upload-pack", Handler: r.uploadPack},
		{Methods: []string{http.MethodGet, http.MethodPost}, Pattern: "/{repo...}/git-receive-pack", Handler: r.receivePack},
		{Methods: []string{http.MethodGet, http.MethodHead}, Pattern: "/{path...}", Handler: r.dumb},
	}
}

type repoRoute struct {
	repo     string
	mirror   string
	upstream string
}

func (r *Repo) resolveRepo(c *eco.Ctx, raw string) (repoRoute, error) {
	if raw == "" || strings.HasPrefix(raw, "/") || strings.HasSuffix(raw, "/") {
		return repoRoute{}, fmt.Errorf("empty repository path")
	}
	raw = strings.TrimSuffix(raw, ".git")
	parts := strings.Split(raw, "/")
	if len(parts) < 2 || !validDNSHost(parts[0]) {
		return repoRoute{}, fmt.Errorf("repository path must begin with a DNS host")
	}
	parts[0] = strings.ToLower(parts[0])
	for _, part := range parts {
		if !safeRepoSegment(part) {
			return repoRoute{}, fmt.Errorf("unsafe repository path")
		}
	}
	repo := strings.Join(parts, "/")
	root, err := c.ManagedDir()
	if err != nil {
		return repoRoute{}, err
	}
	components := append([]string{root}, parts...)
	components[len(components)-1] += ".git"
	mirror := filepath.Join(components...)
	rel, err := filepath.Rel(root, mirror)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return repoRoute{}, fmt.Errorf("repository path escapes managed storage")
	}
	origin, err := r.resolve(repo)
	if err != nil || origin == "" {
		if err == nil {
			err = fmt.Errorf("empty upstream")
		}
		return repoRoute{}, fmt.Errorf("resolve upstream: %w", err)
	}
	return repoRoute{repo: repo, mirror: mirror, upstream: origin}, nil
}

func safeRepoSegment(s string) bool {
	return s != "" && s != "." && s != ".." &&
		!strings.ContainsAny(s, `/\`) && !strings.ContainsRune(s, 0)
}

func validDNSHost(host string) bool {
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') &&
				(ch < '0' || ch > '9') && ch != '-' {
				return false
			}
		}
	}
	return true
}

func (r *Repo) ensure(c *eco.Ctx, route repoRoute) (MirrorOutcome, error) {
	return r.mirrors.ensure(c.Context(), route.repo, route.mirror, route.upstream,
		c.Offline(), func() error { return r.syncCatalog(c, route) })
}

func (r *Repo) syncCatalog(c *eco.Ctx, route repoRoute) error {
	refs, head, err := r.mirrors.inspect(c.Context(), route.mirror)
	if err != nil {
		return err
	}
	prefix := route.repo + "/"
	current := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		name := prefix + ref.Name
		current[name] = struct{}{}
		if err := c.SetRef(name, ref.Object, eco.Revalidate(r.refsTTL)); err != nil {
			return err
		}
	}
	old, err := c.ListRefs(prefix)
	if err != nil {
		return err
	}
	for _, ref := range old {
		if _, ok := current[ref.Name]; !ok {
			if err := c.DeleteRef(ref.Name); err != nil {
				return err
			}
		}
	}

	if err := c.DeleteArtifacts(route.repo); err != nil {
		return err
	}
	sizeRef := head
	if sizeRef == "" && len(refs) > 0 {
		sizeRef = refs[0].Name
	}
	size := dirSize(route.mirror)
	for _, ref := range refs {
		refSize := int64(0)
		if ref.Name == sizeRef {
			refSize = size
		}
		if err := c.RecordArtifact(catalog.Artifact{
			Name:    route.repo,
			Version: strings.TrimPrefix(ref.Name, "refs/"),
			Size:    refSize,
			Origin:  route.upstream,
			Extra: map[string]any{
				"commit": ref.Object,
				"ref":    ref.Name,
				"head":   ref.Name == head,
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repo) infoRefs(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	noCache(c.W.Header())
	switch req.URL.Query().Get("service") {
	case "git-receive-pack":
		r.writePushRefusal(c)
		return
	case "git-upload-pack":
	default:
		_ = c.Text(http.StatusNotFound, "dumb HTTP Git protocol is not supported")
		return
	}
	route, err := r.resolveRepo(c, p.Unescape("repo"))
	if err != nil {
		_ = c.NotFound("not a valid repository path")
		return
	}
	if _, err := r.ensure(c, route); err != nil {
		r.writeMirrorError(c, err)
		return
	}

	c.W.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	c.W.WriteHeader(http.StatusOK)
	if _, err := c.W.Write(serviceAdvertisement); err != nil {
		return
	}
	_ = r.mirrors.advertise(c.Context(), route.mirror, req.Header.Get("Git-Protocol"), c.W)
}

func (r *Repo) uploadPack(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	noCache(c.W.Header())
	route, err := r.resolveRepo(c, p.Unescape("repo"))
	if err != nil {
		_ = c.NotFound("not a valid repository path")
		return
	}
	body, status, err := readNegotiation(c.W, req)
	if err != nil {
		_ = c.Text(status, err.Error())
		return
	}
	if !mirrorExists(route.mirror) {
		if _, err := r.ensure(c, route); err != nil {
			r.writeMirrorError(c, err)
			return
		}
	}

	c.W.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	c.W.WriteHeader(http.StatusOK)
	_ = r.mirrors.uploadPack(c.Context(), route.mirror,
		req.Header.Get("Git-Protocol"), body, c.W)
}

func readNegotiation(w http.ResponseWriter, req *http.Request) (body []byte, status int, err error) {
	if req.ContentLength > maxNegotiationBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("negotiation body too large")
	}
	raw := http.MaxBytesReader(w, req.Body, maxNegotiationBytes)
	var reader io.Reader = raw
	if encoding := strings.TrimSpace(strings.ToLower(req.Header.Get("Content-Encoding"))); encoding != "" {
		if encoding != "gzip" {
			return nil, http.StatusUnsupportedMediaType,
				fmt.Errorf("unsupported content encoding")
		}
		compressed, err := gzip.NewReader(raw)
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("invalid gzip body")
		}
		defer func() { _ = compressed.Close() }()
		reader = compressed
	}
	body, err = io.ReadAll(io.LimitReader(reader, maxNegotiationBytes+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("negotiation body too large")
		}
		return nil, http.StatusBadRequest, fmt.Errorf("invalid negotiation body")
	}
	if len(body) > maxNegotiationBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("negotiation body too large")
	}
	return body, 0, nil
}

func (r *Repo) receivePack(w http.ResponseWriter, req *http.Request, p router.Params) {
	r.writePushRefusal(eco.CtxFrom(w, req, p))
}

func (r *Repo) dumb(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	_ = c.Text(http.StatusNotFound,
		"dumb HTTP Git protocol is not supported; this is a smart-HTTP mirror")
}

func (r *Repo) writePushRefusal(c *eco.Ctx) {
	noCache(c.W.Header())
	c.W.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	body := pktLine([]byte("ERR read-only mirror: push (git-receive-pack) is not supported\n"))
	c.W.WriteHeader(http.StatusForbidden)
	_, _ = c.W.Write(body)
}

func (r *Repo) writeMirrorError(c *eco.Ctx, err error) {
	switch {
	case errors.Is(err, ErrNotCached):
		_ = c.NotFound("repository not cached (offline)")
	default:
		var mirrorErr *MirrorError
		if errors.As(err, &mirrorErr) {
			_ = c.Text(http.StatusBadGateway, "upstream clone/fetch failed: "+mirrorErr.Error())
			return
		}
		c.WriteError(err)
	}
}

func (r *Repo) maintain(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	root, err := c.ManagedDir()
	if err != nil {
		c.WriteError(err)
		return
	}
	var mirrors []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".git") {
			return nil
		}
		if mirrorExists(path) {
			mirrors = append(mirrors, path)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		c.WriteError(err)
		return
	}
	sort.Strings(mirrors)
	failures := make([]string, 0)
	maintained := 0
	for _, mirror := range mirrors {
		rel, relErr := filepath.Rel(root, mirror)
		if relErr != nil {
			failures = append(failures, mirror+": "+relErr.Error())
			continue
		}
		repo := strings.TrimSuffix(filepath.ToSlash(rel), ".git")
		origin, resolveErr := r.resolve(repo)
		if resolveErr != nil {
			failures = append(failures, repo+": "+resolveErr.Error())
			continue
		}
		route := repoRoute{repo: repo, mirror: mirror, upstream: origin}
		if maintainErr := r.mirrors.maintain(c.Context(), repo, mirror,
			func() error { return r.syncCatalog(c, route) }); maintainErr != nil {
			failures = append(failures, repo+": "+maintainErr.Error())
			continue
		}
		maintained++
	}
	_ = c.JSON(http.StatusOK, map[string]any{
		"maintained": maintained,
		"errors":     failures,
	})
}

func pktLine(data []byte) []byte {
	return append([]byte(fmt.Sprintf("%04x", len(data)+4)), data...)
}

func noCache(header http.Header) {
	header.Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	header.Set("Pragma", "no-cache")
	header.Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
}

func setupSteps(_ eco.SetupContext) []eco.SetupStep {
	return []eco.SetupStep{
		{Comment: "Use the temporary Git base from the pkgreg shell. Keep the original Git host in the path:"},
		{Command: `git clone "$PKGREG_GIT_URL/github.com/<owner>/<repo>.git"`},
	}
}

type lfsObjectRequest struct {
	OID  any `json:"oid"`
	Size any `json:"size"`
}

type lfsBatchRequest struct {
	Operation string             `json:"operation"`
	Transfers []string           `json:"transfers,omitempty"`
	Objects   []lfsObjectRequest `json:"objects"`
}

type lfsUpstreamBatch struct {
	Objects []struct {
		OID     string `json:"oid"`
		Actions struct {
			Download struct {
				Href   string            `json:"href"`
				Header map[string]string `json:"header"`
			} `json:"download"`
		} `json:"actions"`
	} `json:"objects"`
}

func (r *Repo) lfsBatch(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	c.W.Header().Set("Content-Type", lfsMediaType)
	route, err := r.resolveRepo(c, p.Unescape("repo"))
	if err != nil {
		_ = lfsJSON(c, http.StatusNotFound, map[string]any{"message": "not a valid repository path"})
		return
	}
	var payload lfsBatchRequest
	decoder := json.NewDecoder(http.MaxBytesReader(c.W, req.Body, maxLFSBatchBytes))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		_ = lfsJSON(c, http.StatusBadRequest, map[string]any{"message": "invalid JSON"})
		return
	}
	if payload.Operation != "download" {
		_ = lfsJSON(c, http.StatusForbidden,
			map[string]any{"message": "read-only mirror: LFS upload is not supported"})
		return
	}

	results := make([]map[string]any, 0, len(payload.Objects))
	missing := make([]map[string]any, 0)
	for _, object := range payload.Objects {
		oid, ok := object.OID.(string)
		if !ok {
			results = append(results, lfsError(object.OID, object.Size,
				http.StatusUnprocessableEntity, "invalid oid"))
			continue
		}
		digest, digestErr := blob.ParseDigest(oid)
		if digestErr != nil || oid != strings.ToLower(oid) {
			results = append(results, lfsError(oid, object.Size,
				http.StatusUnprocessableEntity, "invalid oid"))
			continue
		}
		if c.BlobExists(digest) {
			results = append(results, lfsAction(oid, object.Size, c.ExternalBase()))
			continue
		}
		if c.Offline() {
			results = append(results, lfsError(oid, object.Size,
				http.StatusNotFound, "not cached (offline)"))
			continue
		}
		missing = append(missing, map[string]any{"oid": oid, "size": object.Size})
	}

	if len(missing) > 0 {
		hrefs := r.forwardLFSBatch(c, route, missing)
		for _, object := range missing {
			oid := object["oid"].(string)
			if info, ok := hrefs[oid]; ok {
				r.rememberLFS(c.Project, oid, info)
				results = append(results, lfsAction(oid, object["size"], c.ExternalBase()))
			} else {
				results = append(results, lfsError(oid, object["size"],
					http.StatusNotFound, "no such object upstream"))
			}
		}
	}
	_ = lfsJSON(c, http.StatusOK, map[string]any{"transfer": "basic", "objects": results})
}

func (r *Repo) forwardLFSBatch(
	c *eco.Ctx, route repoRoute, objects []map[string]any,
) map[string]pendingLFS {
	body, err := json.Marshal(map[string]any{
		"operation": "download",
		"transfers": []string{"basic"},
		"objects":   objects,
	})
	if err != nil {
		return nil
	}
	headers := http.Header{}
	headers.Set("Accept", lfsMediaType)
	headers.Set("Content-Type", lfsMediaType)
	request := upstream.Request{
		URL:     r.lfsURL(route.repo, route.upstream),
		Method:  http.MethodPost,
		Headers: headers,
		Body:    body,
		Eco:     ID,
	}
	_, _, response, err := c.Exchange(request, maxLFSBatchBytes)
	if err != nil {
		return nil
	}
	var parsed lfsUpstreamBatch
	if json.Unmarshal(response, &parsed) != nil {
		return nil
	}
	out := make(map[string]pendingLFS)
	for _, object := range parsed.Objects {
		download := object.Actions.Download
		if download.Href == "" {
			continue
		}
		objectHeaders := http.Header{}
		for key, value := range download.Header {
			objectHeaders.Set(key, value)
		}
		out[object.OID] = pendingLFS{
			Href: download.Href, Headers: objectHeaders,
			Repo: route.repo, Origin: route.upstream,
			Expires: time.Now().Add(pendingLFSLifetime),
		}
	}
	return out
}

func (r *Repo) rememberLFS(project, oid string, info pendingLFS) {
	r.lfsMu.Lock()
	defer r.lfsMu.Unlock()
	now := time.Now()
	for key, candidate := range r.pending {
		if now.After(candidate.Expires) {
			delete(r.pending, key)
		}
	}
	if len(r.pending) >= maxPendingLFS {
		for key := range r.pending {
			delete(r.pending, key)
			break
		}
	}
	r.pending[project+"\x00"+oid] = info
}

func (r *Repo) pendingLFS(project, oid string) (pendingLFS, bool) {
	r.lfsMu.Lock()
	defer r.lfsMu.Unlock()
	key := project + "\x00" + oid
	info, ok := r.pending[key]
	if ok && time.Now().After(info.Expires) {
		delete(r.pending, key)
		return pendingLFS{}, false
	}
	return info, ok
}

func (r *Repo) lfsGet(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	rawOID := p.Unescape("oid")
	digest, err := blob.ParseDigest(rawOID)
	if err != nil || rawOID != strings.ToLower(rawOID) {
		_ = c.NotFound("invalid oid")
		return
	}
	info, havePending := r.pendingLFS(c.Project, rawOID)
	if !c.BlobExists(digest) && !havePending {
		if c.Offline() {
			_ = c.NotFound("not cached (offline)")
		} else {
			_ = c.NotFound("unknown LFS object (request a batch first)")
		}
		return
	}
	origin := upstream.Request{}
	name := "(lfs)"
	source := ""
	if havePending {
		origin = upstream.Request{
			URL: info.Href, Headers: info.Headers.Clone(), Eco: ID,
		}
		name = info.Repo + " (LFS)"
		source = info.Origin
	}
	if err := c.Serve(engine.Resolution{
		Key:       "lfs/" + rawOID,
		Upstream:  origin,
		Expect:    engine.Expect{Digest: digest},
		MediaType: "application/octet-stream",
		Artifact: &catalog.Artifact{
			Name: name, Version: rawOID[:12], Origin: source,
			Extra: map[string]any{"lfs": true},
		},
		AccessName: name,
	}); err != nil {
		c.WriteError(err)
	}
}

func lfsAction(oid string, size any, externalBase string) map[string]any {
	return map[string]any{
		"oid": oid, "size": size,
		"actions": map[string]any{
			"download": map[string]any{
				"href": strings.TrimRight(externalBase, "/") + "/+lfs/" + oid,
			},
		},
	}
}

func lfsError(oid, size any, code int, message string) map[string]any {
	return map[string]any{
		"oid": oid, "size": size,
		"error": map[string]any{"code": code, "message": message},
	}
}

func lfsJSON(c *eco.Ctx, status int, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.ServeBytes(status, lfsMediaType, body)
}
