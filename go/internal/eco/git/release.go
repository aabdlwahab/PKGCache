package git

import (
	"net/http"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/engine"
	"github.com/aabdlwahab/PKGCache/internal/router"
)

// releasePrefix is where release assets live in the cache, beside the mirrors and LFS
// objects. It is also what makes them immutable in the descriptor.
const releasePrefix = "release/"

// release serves a forge's release asset: /<host>/<owner>/<repo>/releases/download/<tag>/<file>.
//
// These are how a lot of software ships what no package index carries — a patched vLLM
// wheel for one GPU generation, a static binary — and a build downloads them by URL,
// straight past every index the cache fronts. Same forge, same path as the repository
// they belong to, so they sit here beside its mirror.
//
// Immutable: a published asset is replaced only by deleting the release, and a build
// that pinned the URL wants the bytes it pinned. The forge's redirect to its object
// store is followed upstream; the client only ever sees this URL.
func (r *Repo) release(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	route, err := r.resolveRepo(c, p.Unescape("repo"))
	if err != nil {
		_ = c.NotFound(err.Error())
		return
	}
	tag, file := p.Unescape("tag"), p.Unescape("file")
	if !safeRepoSegment(tag) || !safeRepoSegment(file) {
		_ = c.NotFound("invalid release asset path")
		return
	}

	asset := route.repo + "/releases/download/" + tag + "/" + file
	upstreamURL := strings.TrimSuffix(route.upstream, ".git") + "/releases/download/" +
		tag + "/" + file
	err = c.Serve(engine.Resolution{
		Key:       releasePrefix + route.repo + "/" + tag + "/" + file,
		Upstream:  c.CachesFirst(c.UpstreamRequest(upstreamURL, nil), asset),
		MediaType: "application/octet-stream",
		Artifact: &catalog.Artifact{
			Name: route.repo, Version: tag, Origin: upstreamURL,
			Extra: map[string]any{"file": file},
		},
		AccessName: route.repo,
	})
	if err != nil {
		c.WriteError(err)
	}
}

// parseArtifactKey is the inventory identity of a release asset: the repository and the
// tag. Mirrors and LFS objects are cached content but not artifacts of their own.
func parseArtifactKey(key string) (name, version, arch string, ok bool) {
	rest, isRelease := strings.CutPrefix(key, releasePrefix)
	if !isRelease {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 5 {
		return "", "", "", false
	}
	tag := parts[len(parts)-2]
	return strings.Join(parts[:len(parts)-2], "/"), tag, "", true
}
