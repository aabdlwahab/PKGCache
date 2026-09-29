package pypi

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/engine"
	"github.com/aabdlwahab/PKGCache/internal/router"
)

// LockFileHosts are the hosts a uv.lock can name files on that this cache fetches by
// address: where the indexes it fronts keep their files. PyPI's index links to
// files.pythonhosted.org; PyTorch's to download.pytorch.org and its download-r2 mirror;
// NVIDIA's and FlashInfer's to themselves.
//
// A fixed list rather than any host a lock names: the route fetches whatever path it is
// given, so the set of hosts is the whole of what it can be pointed at.
var LockFileHosts = []string{
	"files.pythonhosted.org",
	"download.pytorch.org",
	"download-r2.pytorch.org",
	"pypi.nvidia.com",
	"flashinfer.ai",
}

// lockedFilesKey is the cache-key prefix for files served by their address.
const lockedFilesKey = "files/"

// lockedFile serves a file by the address its index keeps it at: /+files/<host>/<path>
// stands for https://<host>/<path>.
//
// A uv.lock records the address of every file it pins, and `uv sync --frozen` and
// `--locked` fetch exactly those addresses. No index setting reaches them — uv documents
// none, and pointing UV_INDEX_URL at the cache makes `--locked` refuse the lock outright —
// so every locked uv install in a build went straight to PyPI however the cache was set
// up. A build's uv.lock is pointed here for the length of the sync instead (see
// dockerfile.UVWrapper), and the addresses it names are answered from the cache. uv checks
// each file against the hash the lock records, so this changes where a file comes from and
// never what is installed.
//
// These hosts name a file by path for good — PyPI's paths are content-addressed and no
// index here reissues a filename — which is why the path can be a cache key, immutable.
func (r *Repo) lockedFile(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	host := strings.ToLower(p.Unescape("host"))
	path := p.Get("path")
	base, known := r.fileHosts[host]
	if !known || !validLockedPath(path) || !c.OriginAllowed(host) {
		_ = c.NotFound("not a package file on a host this cache fetches from")
		return
	}
	upstreamURL := base + path
	filename, _ := url.PathUnescape(path[strings.LastIndex(path, "/")+1:])
	// The lock's own hash, where the build passed it on: a file already held under any
	// other name is then served without being fetched again, and one that is fetched is
	// checked against it before it is kept.
	var expect engine.Expect
	relayed := "+files/" + host + "/" + path
	if raw := req.URL.Query().Get("sha256"); raw != "" {
		digest, err := blob.ParseDigest(raw)
		if err != nil {
			_ = c.NotFound("not a sha256: " + raw)
			return
		}
		expect.Digest = digest
		relayed += "?sha256=" + raw
	}

	var artifact *catalog.Artifact
	accessName := ""
	if name, version, arch, ok := ParseDistributionFilename(filename); ok {
		artifact = &catalog.Artifact{
			Name: name, Version: version, Arch: arch, Origin: upstreamURL,
			Extra: map[string]any{"host": host},
		}
		accessName = name
	}
	err := c.Serve(engine.Resolution{
		Key: lockedFilesKey + host + "/" + path,
		// A laptop's team cache has the same route, so the file is asked of each cache in
		// front of this one at the same path, hash included, before its host.
		Upstream:   c.CachesFirst(c.UpstreamRequest(upstreamURL, nil), relayed),
		Expect:     expect,
		MediaType:  "application/octet-stream",
		Artifact:   artifact,
		AccessName: accessName,
	})
	if err != nil {
		c.WriteError(err)
	}
}

// validLockedPath accepts the path of a distribution file and nothing else: no empty,
// relative or encoded-separator segments, and a name an index would serve. The route
// fetches from a fixed set of hosts, so this is what stops it reaching anything there
// but a package file.
func validLockedPath(path string) bool {
	if path == "" || strings.ContainsAny(path, "?#\\") {
		return false
	}
	lower := strings.ToLower(path)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return false
	}
	segments := strings.Split(path, "/")
	for _, segment := range segments {
		unescaped, err := url.PathUnescape(segment)
		if err != nil || unescaped == "" || unescaped == "." || unescaped == ".." {
			return false
		}
	}
	// A wheel or an sdist: what an index serves, and nothing else on those hosts.
	name := strings.ToLower(segments[len(segments)-1])
	return slices.ContainsFunc(append([]string{".whl"}, distributionSuffixes...), func(suffix string) bool {
		return strings.HasSuffix(name, suffix) && len(name) > len(suffix)
	})
}
