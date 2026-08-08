// Package session builds the environment that points package tools at a cache, and
// picks the shell a temporary session runs in.
//
// It is shared by the two programs that configure tools without installing anything:
// pkgreg-client, which points them at a verified loopback bridge, and pkgcache, which
// points them at a cache running on this machine. The difference between those two is
// a base URL and a variable namespace; everything else — which variables a wheel, a
// packument, a clone and a .deb are actually influenced by — is the same knowledge,
// and it was worth having in one place before there were two callers rather than
// after.
package session

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// Prefixes namespace each program's own variables. A session always clears both, so a
// pkgcache shell opened inside a pkgreg-client shell cannot inherit a stale server
// address from it.
const (
	PkgregPrefix   = "PKGREG_"
	PkgcachePrefix = "PKGCACHE_"
)

// Options describes the cache tools should be pointed at.
type Options struct {
	// Prefix namespaces this program's own variables: PkgregPrefix or PkgcachePrefix.
	Prefix string
	// Kind is the value of <Prefix>SESSION, and exists so a shell prompt or a bug
	// report can say which kind of session it is in.
	Kind string
	// BaseURL is the origin, without a trailing slash: http://127.0.0.1:41780.
	BaseURL string
	// Project scopes every path-prefixed URL.
	Project string
	// AptProxy is published for apt and apk to be pointed at deliberately. It is not
	// exported as http_proxy: that would send curl, wget and every HTTPS client through
	// a forward proxy that only relays http://, turning one working tool into several
	// broken ones.
	AptProxy string
	// DockerRegistry is the authority images are pulled from. Empty omits it.
	DockerRegistry string
	// GitHosts are redirected through the cache's mirror with url.<...>.insteadOf, so
	// an unmodified `git clone https://github.com/...` is served from the cache.
	// Empty leaves git alone.
	GitHosts []string
	// Extra carries variables only one caller has, such as a CA fingerprint. They are
	// written after the managed set and are not otherwise interpreted.
	Extra [][2]string
}

// Environment returns base with this session's variables applied.
//
// Every variable it manages is removed first, from both namespaces, so entering a
// session twice does not leave the first one's settings behind — and so a variable
// this session deliberately does not set, such as a CA path that no longer applies,
// is gone rather than stale.
func Environment(base []string, o Options) []string {
	if o.Project == "" {
		o.Project = "global"
	}
	prefix := o.Prefix
	if prefix == "" {
		prefix = PkgcachePrefix
	}
	baseURL := strings.TrimRight(o.BaseURL, "/")
	projectBase := baseURL + "/" + o.Project

	managed := managedNames(o)
	out := make([]string, 0, len(base)+16)
	noProxy := ""
	for _, entry := range base {
		key, value, found := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if upper == "NO_PROXY" && noProxy == "" {
			noProxy = value
		}
		if found && managed[upper] {
			continue
		}
		out = append(out, entry)
	}
	noProxy = appendNoProxy(noProxy, "127.0.0.1", "localhost")

	values := [][2]string{
		{prefix + "SESSION", o.Kind},
		{prefix + "PROJECT", o.Project},
		{prefix + "BRIDGE_URL", baseURL},
		{prefix + "DOCKER_REGISTRY", o.DockerRegistry},
		{prefix + "GIT_URL", projectBase + "/git"},
		{prefix + "FILES_URL", projectBase + "/files/"},
		{prefix + "APT_PROXY", o.AptProxy},
		{"PIP_INDEX_URL", projectBase + "/pypi/root/pypi/+simple/"},
		{"UV_DEFAULT_INDEX", projectBase + "/pypi/root/pypi/+simple/"},
		{"NPM_CONFIG_REGISTRY", projectBase + "/npm/"},
		{"NO_PROXY", noProxy},
		{"no_proxy", noProxy},
	}
	values = append(values, gitConfig(projectBase+"/git", o.GitHosts)...)
	values = append(values, o.Extra...)

	for _, value := range values {
		if value[1] != "" {
			out = append(out, value[0]+"="+value[1])
		}
	}
	return out
}

// gitConfig redirects clones through the cache's mirror.
//
// git has no index variable, but it reads configuration from the environment, so
// insteadOf can be expressed as GIT_CONFIG_* — which redirects an unmodified
// https://github.com/... clone, and submodules and pip's git+https with it. The same
// trick the Dockerfile rewriter uses for a build, applied to a shell.
func gitConfig(gitBase string, hosts []string) [][2]string {
	if len(hosts) == 0 {
		return nil
	}
	out := make([][2]string, 0, len(hosts)*2+1)
	for i, host := range hosts {
		out = append(out,
			[2]string{fmt.Sprintf("GIT_CONFIG_KEY_%d", i),
				fmt.Sprintf("url.%s/%s/.insteadOf", gitBase, host)},
			[2]string{fmt.Sprintf("GIT_CONFIG_VALUE_%d", i),
				fmt.Sprintf("https://%s/", host)})
	}
	return append(out, [2]string{"GIT_CONFIG_COUNT", fmt.Sprint(len(hosts))})
}

// managedNames is every variable a session owns, and therefore every variable it must
// clear before it writes its own.
func managedNames(o Options) map[string]bool {
	names := map[string]bool{
		// Tool settings, including the CA paths a loopback session does not need: an
		// inherited PIP_CERT pointing at a certificate that is no longer in play is a
		// confusing failure, not a harmless leftover.
		"PIP_CERT": true, "PIP_INDEX_URL": true,
		"UV_NATIVE_TLS": true, "UV_DEFAULT_INDEX": true,
		"NODE_EXTRA_CA_CERTS": true, "NPM_CONFIG_CAFILE": true,
		"NPM_CONFIG_REGISTRY": true, "GIT_SSL_CAINFO": true,
		"NO_PROXY": true, "no_proxy": true,
		"GIT_CONFIG_COUNT": true,
	}
	for _, prefix := range []string{PkgregPrefix, PkgcachePrefix} {
		for _, suffix := range []string{
			"SESSION", "SERVER", "PROJECT", "CA_FILE", "CA_SHA256", "BRIDGE_URL",
			"GIT_URL", "APT_PROXY", "FILES_URL", "DOCKER_REGISTRY",
		} {
			names[prefix+suffix] = true
		}
	}
	// GIT_CONFIG_KEY_n/VALUE_n are numbered, and a session that set three of them and
	// then set two would otherwise leave the third pointing at a cache that is no
	// longer there. Clearing a generous fixed range covers every session either program
	// writes; anything beyond it was not ours to begin with.
	for i := range 16 {
		names[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)] = true
		names[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)] = true
	}
	return names
}

func appendNoProxy(value string, hosts ...string) string {
	parts := strings.Split(value, ",")
	seen := make(map[string]bool, len(parts)+len(hosts))
	clean := make([]string, 0, len(parts)+len(hosts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		clean = append(clean, part)
	}
	for _, host := range hosts {
		if !seen[host] {
			seen[host] = true
			clean = append(clean, host)
		}
	}
	return strings.Join(clean, ",")
}

// Shell picks the interactive shell a session runs in.
//
// preferred wins, then $SHELL, then the platform default. Interactive flags are passed
// so the user gets their prompt and their aliases: a session shell that behaves
// differently from their usual one is a session they will not trust.
func Shell(preferred, operatingSystem string) (program string, args []string, err error) {
	goos := operatingSystem
	if goos == "" {
		goos = runtime.GOOS
	}
	switch goos {
	case "linux", "darwin":
		shell := preferred
		if shell == "" {
			shell = strings.TrimSpace(os.Getenv("SHELL"))
		}
		if shell == "" {
			shell = "/bin/sh"
		}
		return shell, []string{"-i"}, nil
	case "windows":
		shell := preferred
		if shell == "" {
			shell = "powershell.exe"
		}
		return shell, []string{"-NoLogo"}, nil
	default:
		return "", nil, fmt.Errorf("session: unsupported operating system %q", goos)
	}
}
