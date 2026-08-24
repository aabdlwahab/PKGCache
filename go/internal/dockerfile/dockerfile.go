// Package dockerfile rewrites a Dockerfile so a build reaches the cache, without the
// author ever editing the file.
//
// The alternative was asking every developer to paste a block of ARG lines at the top
// of every Dockerfile and remember four --build-arg flags. That block is correct, and
// it is still what the tutorial documents for anyone not using the client — but it is
// twelve lines of pkgreg in a file that should be about their application, it has to be
// repeated in every stage, and it silently rots when the cache's address changes. This
// package moves that knowledge into the tool, so the file on disk stays the file the
// author would have written with no cache at all.
//
// The rewrite is deliberately small: declare build arguments, and repoint FROM. It
// never adds a RUN, never reorders, never removes. Anything larger would make the
// generated file something a reader could not predict from the original, and a build
// nobody can predict is worse than one flag they have to remember.
package dockerfile

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
)

// Mode selects how the build reaches the cache.
type Mode int

const (
	// Bridge points tools at the client's loopback bridge, which is plain HTTP.
	// Nothing needs a certificate, but the daemon must share the loopback interface —
	// so this is the mode for a Linux daemon on the same machine.
	Bridge Mode = iota
	// CacheAddress points tools at the cache's own HTTPS address and mounts the CA
	// into each RUN that needs it. Required wherever the daemon cannot see the
	// client's loopback: Docker Desktop, a remote daemon, a container builder, CI.
	CacheAddress
	// HostGateway points tools at a cache running on this machine, reached from inside
	// the build through host.docker.internal. It is CacheAddress with every TLS part
	// removed: a local cache serves plain HTTP, so there is no CA to mount and none of
	// the six per-tool certificate variables to declare.
	//
	// It exists because Bridge is not portable. A loopback address only reaches a
	// daemon that shares this machine's network namespace, which Docker Desktop and any
	// remote daemon do not — and pkgcache has no HTTPS address to fall back to.
	//
	// The daemon must be told to accept a plain-HTTP registry at that authority, which
	// `pkgcache docker-setup` installs. Loopback is exempt by default; host.docker
	// .internal is not.
	HostGateway
)

// SecretID is the build secret the CA is passed as in CacheAddress mode. It is also
// the id the caller must use on the command line, so it lives here rather than being
// spelled out twice.
const SecretID = "pkgreg-ca"

// SecretTarget is where the CA is mounted inside a RUN. Under /run/secrets because
// that is where BuildKit's own default puts them, so it reads as ordinary to anyone
// looking at the generated file.
const SecretTarget = "/run/secrets/" + SecretID

// Options describes the instance a build should be pointed at.
type Options struct {
	Mode Mode
	// Registry is the authority images are pulled from: the bridge's host:port in
	// Bridge mode, the cache's own authority in CacheAddress mode.
	Registry string
	// Base is the scheme://authority tools address indexes through.
	Base    string
	Project string
	// AptProxy is the plain-HTTP forward proxy for apt and apk. Empty disables the
	// proxy variables rather than declaring them empty, because an empty http_proxy
	// is not the same as an unset one to every tool that reads it.
	AptProxy string
	// GitHosts are rewritten transparently through the cache's git mirror, so an
	// unmodified https://github.com/... clone in a RUN is served from the cache.
	GitHosts []string
	// SkipFrom leaves FROM lines alone. Set once the daemon resolves images through
	// the cache by itself — a registry mirror makes this rewrite redundant, and a
	// rewrite nobody needs is a parsing risk nobody needs.
	SkipFrom bool
}

// Change records one rewritten FROM, for reporting. A tool that silently alters what
// gets built is a tool people stop trusting, so every substitution is printable.
type Change struct{ From, To string }

// Result is a rewritten Dockerfile and what was done to it.
type Result struct {
	Content []byte
	Changes []Change
	// Stages is the count of build stages seen, for the caller's diagnostics.
	Stages int
	// NeedsSecret is true when at least one RUN was given the CA mount, so the caller
	// knows whether to pass --secret at all.
	NeedsSecret bool
}

// upstreamAlias maps a registry host to the cache's upstream name. Anything absent is
// left alone: a registry this cache does not front is not ours to redirect.
var upstreamAlias = map[string]string{
	"docker.io":            "dockerhub",
	"index.docker.io":      "dockerhub",
	"registry-1.docker.io": "dockerhub",
	"ghcr.io":              "ghcr",
	"quay.io":              "quay",
}

var (
	// A FROM line, with its optional flags (--platform=…) kept intact.
	fromRE = regexp.MustCompile(`(?i)^(\s*FROM\s+)((?:--[^\s]+\s+)*)(\S+)(.*)$`)
	// AS <name> at the end of a FROM.
	asRE = regexp.MustCompile(`(?i)\sAS\s+(\S+)\s*$`)
	// A RUN line, with any flags it already carries.
	runRE = regexp.MustCompile(`(?i)^(\s*RUN\s+)(.*)$`)
	// A heredoc opener, e.g. RUN <<EOF or COPY <<-'EOF'. Everything up to the
	// terminator is data, not instructions.
	heredocRE = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)
)

// chunk is one logical line plus whether it is an instruction at all. Heredoc bodies
// and comments are carried through untouched, which is the whole point of tracking
// them: a line of shell inside a heredoc can begin with the word FROM.
type chunk struct {
	text string
	code bool
}

// parse folds a Dockerfile into logical lines.
//
// Two things make this more than strings.Split: a trailing backslash continues the
// instruction onto the next line, and a heredoc turns every line up to its terminator
// into data. Both were found by rewriting real files — the heredoc case rewrote a
// line of shell script as though it were a FROM.
func parse(source string) []chunk {
	var (
		chunks   []chunk
		pending  string
		heredocs []string
	)
	flush := func(text string) {
		chunks = append(chunks, chunk{text: text, code: true})
		if terminator := heredocOpener(text); terminator != "" {
			heredocs = append(heredocs, terminator)
		}
	}
	for _, raw := range splitLines(source) {
		if len(heredocs) > 0 {
			chunks = append(chunks, chunk{text: raw})
			if strings.TrimSpace(raw) == heredocs[len(heredocs)-1] {
				heredocs = heredocs[:len(heredocs)-1]
			}
			continue
		}
		if pending != "" {
			pending += "\n" + raw
			if !continues(raw) {
				flush(pending)
				pending = ""
			}
			continue
		}
		if isComment(raw) || strings.TrimSpace(raw) == "" {
			chunks = append(chunks, chunk{text: raw})
			continue
		}
		if continues(raw) {
			pending = raw
			continue
		}
		flush(raw)
	}
	if pending != "" {
		flush(pending)
	}
	return chunks
}

// Rewrite returns the Dockerfile a build should actually run.
func Rewrite(source []byte, options Options) (Result, error) {
	if options.Project == "" {
		return Result{}, fmt.Errorf("dockerfile: project is required")
	}
	if options.Base == "" {
		return Result{}, fmt.Errorf("dockerfile: base URL is required")
	}
	args := buildArgs(options)

	var (
		result    Result
		stages    = map[string]bool{}
		rewritten []string
	)
	for _, item := range parse(string(source)) {
		if !item.code {
			rewritten = append(rewritten, item.text)
			continue
		}
		switch {
		case isFrom(item.text):
			replaced, change := rewriteFrom(item.text, stages, options)
			rewritten = append(rewritten, replaced)
			if change != nil {
				result.Changes = append(result.Changes, *change)
			}
			if name := asRE.FindStringSubmatch(item.text); name != nil {
				stages[strings.ToLower(name[1])] = true
			}
			result.Stages++
			// ARG is scoped to its stage, so the block is repeated after every FROM.
			// Declaring it once at the top would work in the first stage and quietly
			// do nothing in the rest — the failure people report as "works locally".
			rewritten = append(rewritten, args...)
		case options.Mode == CacheAddress && isRun(item.text):
			replaced, mounted := mountCA(item.text)
			rewritten = append(rewritten, replaced)
			result.NeedsSecret = result.NeedsSecret || mounted
		default:
			rewritten = append(rewritten, item.text)
		}
	}

	body := strings.Join(rewritten, "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	result.Content = []byte(body)
	return result, nil
}

// buildArgs is the block declared after every FROM.
//
// ARG rather than ENV throughout: a RUN reads an ARG exactly as it reads an ENV, but
// an ENV is written into the shipped image. An image whose PIP_INDEX_URL points at a
// bridge port on the machine that built it is a trap for whoever pulls it later.
func buildArgs(o Options) []string {
	index := strings.TrimRight(o.Base, "/") + "/" + o.Project + "/pypi/root/pypi/+simple/"
	npm := strings.TrimRight(o.Base, "/") + "/" + o.Project + "/npm/"
	git := strings.TrimRight(o.Base, "/") + "/" + o.Project + "/git"

	args := []string{
		"ARG PIP_INDEX_URL=" + index,
		"ARG UV_DEFAULT_INDEX=" + index,
		"ARG NPM_CONFIG_REGISTRY=" + npm,
	}
	// pip refuses a plain-HTTP index unless the host is loopback, and it refuses it by
	// *ignoring the index* — the error is "no matching distribution", which reads as a
	// missing package rather than as a rejected repository.
	//
	// That is exactly the HostGateway case this mode exists for: host.docker.internal is
	// not loopback, so every `pip install` in a Docker Desktop or CI build failed while the
	// base image and apt went through the cache perfectly. Loopback needs none of this and
	// CacheAddress mode is HTTPS, so this is the one shape that does.
	if host := insecureHost(o); host != "" {
		args = append(args,
			"ARG PIP_TRUSTED_HOST="+host,
			// uv's equivalent. It fails the same way for the same reason.
			"ARG UV_INSECURE_HOST="+host)
	}
	// Git has no index variable, but it reads configuration from the environment, so
	// insteadOf can be expressed as ARGs — which redirects an unmodified
	// https://github.com/... clone, and submodules and pip's git+https with it.
	for i, host := range o.GitHosts {
		args = append(args,
			fmt.Sprintf("ARG GIT_CONFIG_KEY_%d=url.%s/%s/.insteadOf", i, git, host),
			fmt.Sprintf("ARG GIT_CONFIG_VALUE_%d=https://%s/", i, host))
	}
	if len(o.GitHosts) > 0 {
		args = append(args, fmt.Sprintf("ARG GIT_CONFIG_COUNT=%d", len(o.GitHosts)))
	}
	if o.AptProxy != "" {
		// apt and apk read no index variable but both honour a proxy, so this is what
		// makes `apt-get install` work with no flags. no_proxy is what stops pip and
		// npm being sent through that proxy on their way to the cache — and it has to
		// name the authority they actually use. It named only loopback, which is right
		// for Bridge and wrong for HostGateway: pip and npm would have been sent to the
		// cache through the cache's own apt proxy, which relays http:// and is not what
		// either of them is talking to.
		args = append(args,
			"ARG http_proxy="+o.AptProxy,
			"ARG no_proxy="+noProxyFor(o))
	}
	if o.Mode == CacheAddress {
		// These tools read their own trust store rather than the OS one — verified:
		// node ignores update-ca-certificates entirely — so each is pointed at the
		// mounted CA explicitly.
		args = append(args,
			"ARG PIP_CERT="+SecretTarget,
			"ARG SSL_CERT_FILE="+SecretTarget,
			"ARG NODE_EXTRA_CA_CERTS="+SecretTarget,
			"ARG NPM_CONFIG_CAFILE="+SecretTarget,
			"ARG GIT_SSL_CAINFO="+SecretTarget,
			"ARG UV_NATIVE_TLS=true")
	}
	return args
}

// noProxyFor lists the hosts that must not go through the apt proxy: loopback, and
// whatever authority this build actually reaches the cache on.
func noProxyFor(o Options) string {
	hosts := []string{"127.0.0.1", "localhost"}
	for _, candidate := range []string{hostOf(o.Registry), hostOf(o.Base)} {
		if candidate == "" {
			continue
		}
		if !slices.Contains(hosts, candidate) {
			hosts = append(hosts, candidate)
		}
	}
	return strings.Join(hosts, ",")
}

// insecureHost is the host tools have to be told to trust, or "" when none is.
//
// Only for a plain-HTTP base that is not loopback: pip and uv already trust 127.0.0.1 and
// localhost, and CacheAddress mode is HTTPS with a CA they are pointed at explicitly.
func insecureHost(o Options) string {
	if !strings.HasPrefix(strings.TrimSpace(o.Base), "http://") {
		return ""
	}
	host := hostOf(o.Base)
	switch host {
	case "", "127.0.0.1", "::1", "localhost":
		return ""
	}
	return host
}

// hostOf extracts the host from an authority or a URL, without a port.
func hostOf(value string) string {
	trimmed := strings.TrimSpace(value)
	for _, scheme := range []string{"https://", "http://"} {
		trimmed = strings.TrimPrefix(trimmed, scheme)
	}
	trimmed, _, _ = strings.Cut(trimmed, "/")
	if host, _, err := net.SplitHostPort(trimmed); err == nil {
		return host
	}
	return trimmed
}

func rewriteFrom(line string, stages map[string]bool, o Options) (string, *Change) {
	match := fromRE.FindStringSubmatch(line)
	if match == nil {
		return line, nil
	}
	prefix, flags, ref, tail := match[1], match[2], match[3], match[4]
	if o.SkipFrom {
		return line, nil
	}
	// A FROM naming an earlier stage is not an image. Rewriting it produces a
	// reference to something that has never existed, and the build fails with a
	// registry error for an image the author never mentioned.
	if stages[strings.ToLower(ref)] {
		return line, nil
	}
	mapped := mapImage(ref, o.Registry)
	if mapped == "" {
		return line, nil
	}
	return prefix + flags + mapped + tail, &Change{From: ref, To: mapped}
}

// mapImage returns the cache path for a Docker image reference, or "" to leave it.
// MapImage rewrites one image reference to fetch through a cache at registry, or returns
// "" for a reference that should be left alone.
//
// Exported for `pkgcache pull`, which needs exactly the rewrite a FROM line gets — an
// image pulled by hand and an image pulled by a build should resolve to the same bytes,
// and two implementations of this would eventually disagree about which.
func MapImage(ref, registry string) string { return mapImage(ref, registry) }

func mapImage(ref, registry string) string {
	switch {
	case registry == "", ref == "", ref == "scratch":
		return ""
	case strings.ContainsAny(ref, "$"):
		// Already parameterised: the author is choosing the base themselves.
		return ""
	}
	head, rest, hasSlash := strings.Cut(ref, "/")
	// The first component is a registry only if it looks like a host AND something
	// follows it. Without the second test the tag colon in "python:3.12-alpine" reads
	// as a port, and the most common base image in the world is silently left
	// pointing at Docker Hub.
	if hasSlash && (strings.Contains(head, ".") || strings.Contains(head, ":") || head == "localhost") {
		alias, known := upstreamAlias[strings.ToLower(head)]
		if !known {
			return ""
		}
		return registry + "/" + alias + "/" + rest
	}
	name := ref
	if !hasSlash {
		// Docker Hub's official images live under library/.
		name = "library/" + ref
	}
	return registry + "/dockerhub/" + name
}

// mountCA adds the CA secret to a RUN, unless it already carries one.
func mountCA(line string) (string, bool) {
	match := runRE.FindStringSubmatch(line)
	if match == nil {
		return line, false
	}
	if strings.Contains(line, "type=secret") && strings.Contains(line, SecretID) {
		return line, true
	}
	mount := fmt.Sprintf("--mount=type=secret,id=%s,target=%s ", SecretID, SecretTarget)
	return match[1] + mount + match[2], true
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	// A trailing newline produces a final empty element that would become a spurious
	// blank line on every rewrite.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func continues(line string) bool {
	trimmed := strings.TrimRight(line, " \t")
	return strings.HasSuffix(trimmed, `\`)
}

func isComment(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "#")
}

func isFrom(line string) bool {
	return hasInstruction(line, "FROM")
}

func isRun(line string) bool {
	return hasInstruction(line, "RUN")
}

func hasInstruction(line, name string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) <= len(name) || isComment(line) {
		return false
	}
	if !strings.EqualFold(trimmed[:len(name)], name) {
		return false
	}
	next := trimmed[len(name)]
	return next == ' ' || next == '\t'
}

// heredocOpener reports the terminator of a heredoc started on this line, or "".
func heredocOpener(line string) string {
	if isComment(line) {
		return ""
	}
	if match := heredocRE.FindStringSubmatch(line); match != nil {
		return match[1]
	}
	return ""
}
