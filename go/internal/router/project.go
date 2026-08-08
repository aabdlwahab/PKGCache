package router

import (
	"encoding/base64"
	"net/http"
	"strings"
)

// Project resolution: working out which tenant a request belongs to.
//
// Projects are not ports and not hostnames. They are a URL prefix, because that is
// the only thing every client in this system can be told about. A single process
// therefore serves any number of tenants with no per-project listener, no container
// per project, and no restart to add one.
//
// Three shapes, because three client protocols leave room for different things:
//
//	/<project>/<eco>/…    npm, pypi, git, files — the uniform form
//	/v2/<project>/<repo>  docker, which always starts at /v2/ and cannot be given a
//	                      base path, so the project rides the image name
//	proxy username        apt and apk, which are forward proxies with no room in the
//	                      URL at all
//
// The uniform path form is ALSO accepted for docker and apt, so administrative URLs
// look the same for every ecosystem even where the client protocol differs.

// GlobalProject is the implicit default tenant.
const GlobalProject = "global"

// Target is a resolved request: which tenant, which ecosystem, and the path with the
// routing prefix removed.
type Target struct {
	Project string
	Eco     string
	// Path is what the ecosystem's own routes match against, with the prefix
	// stripped and still escaped.
	Path string
	// Root is the prefix that was stripped, e.g. "/global/npm". Adapters re-attach it
	// when rewriting links, or clients would follow them straight past the cache.
	Root string
}

// KnownProject reports whether a name is a registered tenant. Supplied by the
// control plane so the router needs no dependency on it.
type KnownProject func(name string) bool

// KnownEco reports whether an ID is a registered ecosystem.
type KnownEco func(id string) bool

// ResolvePath resolves the uniform /<project>/<eco>/… form.
//
// Both segments must be recognised. An unknown first segment is reported as
// unresolved rather than silently treated as the global project: a typo in a project
// name should produce a clear error, not content from someone else's tenant.
func ResolvePath(escapedPath string, knownProject KnownProject, knownEco KnownEco) (Target, bool) {
	segs, trailing := splitPath(escapedPath)
	if len(segs) < 2 {
		return Target{}, false
	}
	project, ecoID := segs[0], segs[1]
	if !knownEco(ecoID) {
		return Target{}, false
	}
	if project != GlobalProject && !knownProject(project) {
		return Target{}, false
	}
	return Target{
		Project: project,
		Eco:     ecoID,
		Path:    rebuild(segs[2:], trailing),
		Root:    "/" + project + "/" + ecoID,
	}, true
}

// ResolveOCI resolves a docker pull, where the project rides the image name.
//
// Docker cannot be given a base path — every pull starts at /v2/ — so a project pull
// looks like /v2/<project>/<dest>/<image>/…. The project segment is stripped back to
// /v2/<dest>/<image>/… for the adapter, which never needs to know.
//
// The ambiguity is real: /v2/dockerhub/alpine/manifests/x could parse as project
// "dockerhub", and a registry alias could collide with a project name. Requiring the
// first segment to be a *registered project* resolves it, and the control plane
// reserves the alias names so a project can never be created with one.
func ResolveOCI(escapedPath, ecoID string, knownProject KnownProject) (Target, bool) {
	segs, trailing := splitPath(escapedPath)
	if len(segs) == 0 || segs[0] != "v2" {
		return Target{}, false
	}
	rest := segs[1:]
	if len(rest) >= 2 && rest[0] != GlobalProject && knownProject(rest[0]) {
		return Target{
			Project: rest[0],
			Eco:     ecoID,
			Path:    "/v2/" + rebuildBare(rest[1:], trailing),
			Root:    "/v2",
		}, true
	}
	return Target{
		Project: GlobalProject,
		Eco:     ecoID,
		Path:    escapedPath,
		Root:    "",
	}, true
}

// ResolveProxy resolves a forward-proxy request, where the project rides the proxy
// username: http_proxy=http://<project>@host:3142.
//
// The password is ignored — it is a label, not a credential. Data-plane
// authentication is a separate mechanism; overloading the proxy password would make
// a project name look like a secret when it is not.
func ResolveProxy(r *http.Request, ecoID string, knownProject KnownProject) Target {
	project := GlobalProject
	if user, ok := proxyUser(r); ok && user != GlobalProject && knownProject(user) {
		project = user
	}
	return Target{
		Project: project,
		Eco:     ecoID,
		// A forward-proxy target is an absolute URL; the adapter reconstructs it from
		// the request, so the path is passed through untouched.
		Path: r.URL.EscapedPath(),
		Root: "",
	}
}

// ProxyProject returns the project label carried in Basic proxy authentication.
// The listener uses it to reject an explicitly requested unknown tenant instead of
// silently serving global content. The password is intentionally ignored.
func ProxyProject(r *http.Request) (string, bool) { return proxyUser(r) }

// proxyUser extracts the username from a Proxy-Authorization header.
func proxyUser(r *http.Request) (string, bool) {
	h := r.Header.Get("Proxy-Authorization")
	if h == "" {
		return "", false
	}
	scheme, encoded, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "basic") {
		return "", false
	}
	decoded, err := decodeBase64(strings.TrimSpace(encoded))
	if err != nil {
		return "", false
	}
	user, _, _ := strings.Cut(decoded, ":")
	return user, user != ""
}

// IsProxyRequest reports whether this arrived in absolute form, i.e. from a client
// configured to use us as a forward proxy rather than as an origin.
func IsProxyRequest(r *http.Request) bool {
	return r.URL.IsAbs() && r.URL.Host != ""
}

func rebuild(segs []string, trailing bool) string {
	if len(segs) == 0 {
		return "/"
	}
	return "/" + rebuildBare(segs, trailing)
}

func rebuildBare(segs []string, trailing bool) string {
	s := strings.Join(segs, "/")
	if trailing && s != "" {
		s += "/"
	}
	return s
}

// decodeBase64 decodes a standard-encoding base64 string.
func decodeBase64(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
