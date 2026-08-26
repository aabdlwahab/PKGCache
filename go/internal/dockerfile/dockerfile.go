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
	"sort"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/ociname"
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

	// Indexes maps an upstream package index's origin URL to the cache's name for it,
	// so a Dockerfile that names one directly is served from here instead.
	//
	// PIP_INDEX_URL covers the default index and nothing else. A build that adds
	// `--extra-index-url https://download.pytorch.org/whl/cu130` has named a second one
	// on the command line, and that one went straight past the cache — which for a torch
	// wheel is several gigabytes, the largest single download in the build.
	//
	// Keyed on the origin as configured, matched with or without a trailing slash.
	Indexes map[string]string

	// LocalImage reports whether a reference already names an image on this machine.
	//
	// A base that exists locally is left alone: it may have no upstream at all — crate's
	// prebuilds are exactly this, a shared base built from a Dockerfile in the tree — and
	// rewriting one produces a registry reference for something never published.
	//
	// Optional. Nil means "assume nothing is local", which is the old behaviour and the
	// right default for a caller with no cheap way to ask.
	LocalImage func(ref string) bool
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

var (
	// A FROM line, with its optional flags (--platform=…) kept intact.
	fromRE = regexp.MustCompile(`(?i)^(\s*FROM\s+)((?:--[^\s]+\s+)*)(\S+)(.*)$`)
	// AS <name> at the end of a FROM.
	userRE = regexp.MustCompile(`(?i)^\s*USER\s+(\S+)`)
	// The frontend a build parses itself with. An image reference like any other,
	// written where nothing looks for one.
	syntaxRE = regexp.MustCompile(`(?i)^(\s*#\s*syntax\s*=\s*)(\S+)(.*)$`)
	// `COPY --from=` and a bind mount's `from=`, which name a stage most of the time
	// and an image the rest of it.
	fromFlagRE = regexp.MustCompile(`(?i)(--from=|,from=)([^\s,]+)`)
	// apk as a command, not as the path component in /etc/apk/repositories: the
	// character after it decides which one it is.
	apkRE = regexp.MustCompile(`\bapk([^\w/.]|$)`)
	asRE  = regexp.MustCompile(`(?i)\sAS\s+(\S+)\s*$`)
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
	// apk is only worth touching when there is a proxy for it to reach.
	proxied := options.AptProxy != ""
	inStage := false
	// Where in the current stage to put the repositories back: the line of the first
	// USER that drops privileges, or -1 for "nowhere yet, so the end of the stage".
	restoreAt := -1

	// insertLine puts a line at an earlier position without disturbing what follows.
	insertLine := func(at int, line string) {
		rewritten = append(rewritten, "")
		copy(rewritten[at+1:], rewritten[at:])
		rewritten[at] = line
	}
	// closeStage hands the repositories back at the last point the build can still
	// write them.
	closeStage := func() {
		if !inStage {
			return
		}
		if restoreAt >= 0 {
			insertLine(restoreAt, apkRestore())
		} else {
			rewritten = append(rewritten, apkRestore())
		}
		restoreAt, inStage = -1, false
	}

	items := parse(string(source))
	for index, item := range items {
		if !item.code {
			// Only ahead of the first instruction: that is where BuildKit reads a
			// parser directive, and further down the same line is a comment.
			if result.Stages == 0 {
				replaced, change := rewriteSyntax(item.text, options)
				if change != nil {
					result.Changes = append(result.Changes, *change)
				}
				rewritten = append(rewritten, replaced)
				continue
			}
			rewritten = append(rewritten, item.text)
			continue
		}
		// The restore has to run while the stage can still write /etc/apk. A stage that
		// ends with `USER node` cannot, so the spot is remembered here and the line is
		// inserted there when the stage closes.
		if inStage && restoreAt < 0 && dropsPrivileges(item.text) {
			restoreAt = len(rewritten)
		}

		switch {
		case isFrom(item.text):
			// The stage that is ending gets its repositories back before the next one
			// starts, or the change would ship in whichever stage happened to be last.
			closeStage()
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
			// Only where the stage runs apk. The pair is a shell step, and a base
			// image with no shell cannot run it at all — see stageRunsApk.
			if proxied && stageRunsApk(items, index+1) {
				rewritten = append(rewritten, apkToPlainHTTP())
				inStage = true
			}
		case options.Mode == CacheAddress && isRun(item.text):
			replaced, mounted := mountCA(item.text)
			replaced, indexed := rewriteIndexURLs(replaced, options)
			replaced, borrowed := rewriteBorrowedImages(replaced, stages, options)
			rewritten = append(rewritten, replaced)
			result.NeedsSecret = result.NeedsSecret || mounted
			result.Changes = append(result.Changes, indexed...)
			result.Changes = append(result.Changes, borrowed...)
		default:
			// An index URL is worth replacing wherever it appears: an ARG default, an
			// --extra-index-url written inline, a pip.conf written by a RUN.
			replaced, indexed := rewriteIndexURLs(item.text, options)
			replaced, borrowed := rewriteBorrowedImages(replaced, stages, options)
			rewritten = append(rewritten, replaced)
			result.Changes = append(result.Changes, indexed...)
			result.Changes = append(result.Changes, borrowed...)
		}
	}

	closeStage()

	body := strings.Join(rewritten, "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	result.Content = []byte(body)
	return result, nil
}

// Alpine's package index, which is the one thing the build proxy cannot reach on its own.
//
// pkgcache serves apt and apk through a forward proxy rather than a path, so a build has
// to fetch them over plain HTTP for the proxy to see anything at all. Debian obliges:
// deb.debian.org is http by default. Alpine stopped — every image since 3.x ships
// https://dl-cdn.alpinelinux.org in /etc/apk/repositories, and HTTPS through a proxy is a
// CONNECT tunnel this one does not offer. So apk went straight out and nothing was cached,
// while the help claimed otherwise.
//
// The file is rewritten for the build and put back before the stage ends. That second half
// matters more than it looks: leaving http in the shipped image would change what `apk add`
// does for everybody who later runs the container, on a machine that has never heard of
// this cache. It is the same rule the ARG-not-ENV note above is about — a build may bend
// things for itself and must hand back what it was given.
//
// Restored from a copy rather than by rewriting http back to https, because an image whose
// repositories were already http would otherwise be handed back something it never had.
//
// Guarded on the file existing, so this is a no-op layer on Debian, Ubuntu, distroless and
// anything else without apk.
const apkBackup = "/etc/apk/repositories.pkgcache"

// dropsPrivileges reports whether this instruction hands the stage to a user that
// cannot write /etc/apk.
//
// An unresolved build argument could name anybody, so it counts as dropping them:
// restoring a few lines early costs a stage nothing, and restoring too late fails the
// build outright — which is how this was found.
func dropsPrivileges(text string) bool {
	match := userRE.FindStringSubmatch(text)
	if match == nil {
		return false
	}
	name := match[1]
	if colon := strings.Index(name, ":"); colon >= 0 {
		name = name[:colon]
	}
	return name != "root" && name != "0"
}

// stageRunsApk reports whether the stage starting at items[from] runs apk.
//
// The repositories rewrite is a RUN, and a RUN is /bin/sh. A base image that has no
// shell cannot run it — distroless, scratch, and the images built on them, which is
// most final stages of a Go or Rust service — and the build then dies on an
// instruction its author never wrote, before reaching one they did. The file guard
// inside the step cannot help: the failure is exec'ing the shell, not reading the file.
//
// Detecting the use rather than the base image is what makes that decidable here: an
// image tag says nothing reliable about what is inside it, and `FROM builder` says
// less. A stage that never runs apk had nothing to gain from the pair anyway. The cost
// of being wrong is one uncached apk in a stage that hid its package installs inside a
// script — a slower build, not a broken one.
func stageRunsApk(items []chunk, from int) bool {
	for _, item := range items[from:] {
		if !item.code {
			continue
		}
		if isFrom(item.text) {
			return false
		}
		if isRun(item.text) && apkRE.MatchString(item.text) {
			return true
		}
	}
	return false
}

func apkToPlainHTTP() string {
	// The copy is what decides whether any of this happens: where it cannot be written
	// — an image whose default user is not root — the stage is left exactly as it was
	// and no backup exists, so the restore is a no-op too. Not caching apk is a cost;
	// failing somebody's build to bend a file this cache only borrowed is not.
	return "RUN if [ -f /etc/apk/repositories ] && cp /etc/apk/repositories " +
		apkBackup + " 2>/dev/null; then sed -i 's|^https://|http://|' /etc/apk/repositories; fi"
}

func apkRestore() string {
	return "RUN if [ -f " + apkBackup + " ]; then mv " + apkBackup +
		" /etc/apk/repositories; fi"
}

// rewriteIndexURLs points a directly named package index at the cache.
//
// The injected PIP_INDEX_URL and UV_DEFAULT_INDEX cover the default index and nothing
// else. A Dockerfile that writes an index URL itself — `--extra-index-url` on a uv or pip
// command, or an ARG holding one — has named a second index that those variables do not
// touch, and it went straight to the internet. For a CUDA torch wheel that is several
// gigabytes fetched past a cache that was sitting right there.
//
// Only indexes the cache actually serves are replaced. Rewriting an unknown one would
// point the build at a path that 404s, which is worse than letting it go upstream.
//
// Sorted longest first, so an origin that is a prefix of another cannot claim its URL.
func rewriteIndexURLs(line string, o Options) (string, []Change) {
	if len(o.Indexes) == 0 || o.Base == "" || o.Project == "" {
		return line, nil
	}
	origins := make([]string, 0, len(o.Indexes))
	for origin := range o.Indexes {
		origins = append(origins, origin)
	}
	sort.Slice(origins, func(i, j int) bool { return len(origins[i]) > len(origins[j]) })

	var changes []Change
	for _, origin := range origins {
		trimmed := strings.TrimRight(origin, "/")
		if trimmed == "" || !strings.Contains(line, trimmed) {
			continue
		}
		target := strings.TrimRight(o.Base, "/") + "/" + o.Project + "/pypi/" +
			strings.Trim(o.Indexes[origin], "/") + "/+simple"
		line = strings.ReplaceAll(line, trimmed, target)
		changes = append(changes, Change{From: trimmed, To: target})
	}
	return line, changes
}

// slowFetchSeconds is how long a client should wait for one artifact through the cache.
//
// It bounds a single request, not a build: ten minutes of no progress on one file is a
// stall worth failing, and anything less fails a genuinely slow first fetch instead.
const slowFetchSeconds = "600"

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
		// A cold cache is slower than the CDN these defaults were chosen for.
		//
		// uv gives a request 30 seconds and pip 15. That is generous against
		// files.pythonhosted.org and not against a cache that is fetching the artifact
		// upstream for the first time — a 366 MB CUDA wheel from pypi.nvidia.com took
		// 142 seconds through this cache and 0.1 seconds on the next request. At the
		// default, the first build times out, retries ten times, and reports a server
		// error for a cache that was working exactly as intended.
		//
		// Raised rather than removed: a request that hangs forever is its own failure,
		// and ten minutes is long enough for any single artifact on a slow upstream.
		"ARG UV_HTTP_TIMEOUT=" + slowFetchSeconds,
		"ARG PIP_TIMEOUT=" + slowFetchSeconds,
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
	// Neither is an image that already exists on this machine, and for the same reason
	// one stage further out: a base built locally has no upstream to be fetched from.
	//
	// This is what crate's prebuilds are — a shared base image built from a Dockerfile in
	// the tree, which every service then builds FROM. `FROM mold:latest` looks exactly
	// like a Docker Hub reference and is nothing of the kind, so it was rewritten to
	// dockerhub/library/mold and every one of those services failed on an image that has
	// never been published anywhere.
	//
	// Skipping a public image that happens to be local too is not a loss. Nothing is
	// fetched either way — Docker uses what it has — so the only thing the rewrite could
	// have added is a pull that was not going to happen.
	if o.LocalImage != nil && o.LocalImage(ref) {
		return line, nil
	}
	mapped := mapImage(ref, o.Registry)
	if mapped == "" {
		return line, nil
	}
	return prefix + flags + mapped + tail, &Change{From: ref, To: mapped}
}

// rewriteSyntax points a `# syntax=` parser directive at the cache.
//
// The directive names the frontend image BuildKit parses the Dockerfile with, and it is
// fetched before a single instruction is read — so a build whose every FROM was pointed
// at the cache still went to Docker Hub for this one, first, and on a machine with no
// route there died on a line nobody thinks of as a dependency:
//
//	ERROR: failed to solve: failed to fetch anonymous token: ...
//	> resolve image config for docker-image://docker.io/docker/dockerfile:1
//
// It is skipped on the same terms as a FROM: -keep-images leaves it, and so does an
// image this machine already has, since nothing would be fetched either way.
func rewriteSyntax(line string, o Options) (string, *Change) {
	match := syntaxRE.FindStringSubmatch(line)
	if match == nil || o.SkipFrom {
		return line, nil
	}
	prefix, ref, tail := match[1], match[2], match[3]
	if o.LocalImage != nil && o.LocalImage(ref) {
		return line, nil
	}
	mapped := mapImage(ref, o.Registry)
	if mapped == "" {
		return line, nil
	}
	return prefix + mapped + tail, &Change{From: ref, To: mapped}
}

// rewriteBorrowedImages points a `COPY --from=` or a mount's `from=` at the cache.
//
// Both usually name an earlier stage, which is why an image reference hiding in one is
// easy to miss — `COPY --from=builder` and `COPY --from=ghcr.io/astral-sh/uv:0.10.8`
// are the same instruction, and only the second one fetches anything. A build that
// borrows a single binary from a published image this way went to that registry
// directly, past a cache that had been pointed at every FROM in the file.
//
// A stage name is left alone, and so is a stage index — `COPY --from=0` is the first
// stage, not an image called 0 on Docker Hub. Beyond that the terms are a FROM's:
// -keep-images leaves it, and so does an image this machine already has.
func rewriteBorrowedImages(line string, stages map[string]bool, o Options) (string, []Change) {
	if o.SkipFrom || !(isCopy(line) || isRun(line)) {
		return line, nil
	}
	var changes []Change
	replaced := fromFlagRE.ReplaceAllStringFunc(line, func(match string) string {
		parts := fromFlagRE.FindStringSubmatch(match)
		prefix, ref := parts[1], parts[2]
		if stages[strings.ToLower(ref)] || isStageIndex(ref) {
			return match
		}
		if o.LocalImage != nil && o.LocalImage(ref) {
			return match
		}
		mapped := mapImage(ref, o.Registry)
		if mapped == "" {
			return match
		}
		changes = append(changes, Change{From: ref, To: mapped})
		return prefix + mapped
	})
	return replaced, changes
}

// isStageIndex reports whether a --from names a stage by position rather than by name.
func isStageIndex(ref string) bool {
	if ref == "" {
		return false
	}
	for _, digit := range ref {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
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
	}
	// A variable is only disqualifying where it lands.
	//
	// The rule used to be "any $ means the author is choosing the base themselves", which
	// is true of `FROM ${BASE_IMAGE}` and false of `FROM nvidia/cuda:${CUDA_VER}-runtime`
	// — an extremely common shape, and in one real manifest the shape of every CUDA image
	// in the build. Those are multi-gigabyte pulls that went straight past the cache while
	// everything around them was served from it.
	//
	// The registry prefix is prepended to the *name*, and Docker expands the tag after
	// that, so a variable in the tag is carried through untouched and still resolves. A
	// variable in the name or the registry is a different matter: there is no way to know
	// which repository it will become, so those are still left alone.
	name, tag := splitTag(ref)
	if strings.Contains(name, "$") {
		return ""
	}
	head, rest, hasSlash := strings.Cut(name, "/")
	// The first component is a registry only if it looks like a host AND something
	// follows it. Without the second test the tag colon in "python:3.12-alpine" reads
	// as a port, and the most common base image in the world is silently left
	// pointing at Docker Hub.
	if hasSlash && (strings.Contains(head, ".") || strings.Contains(head, ":") || head == "localhost") {
		reg, ok := ociname.Lookup(head)
		if !ok || !reg.Public {
			// A registry only this builder can route to — localhost, an IP literal, a
			// host:port on the build network — is not ours to redirect: the same string
			// means something different on the machine the cache runs on. Everything
			// else is rewritten whether or not anybody configured it, because the cache
			// discovers a registry from the path the same way this reads it from a FROM.
			return ""
		}
		return registry + "/" + reg.Segment + "/" + rest + tag
	}
	if !hasSlash {
		// Docker Hub's official images live under library/.
		name = "library/" + name
	}
	return registry + "/dockerhub/" + name + tag
}

// splitTag separates a reference from its tag or digest.
//
// The last colon is only a tag separator when it comes after the last slash: in
// "registry:5000/image" it is a port, and cutting there would produce a repository nobody
// asked for. A digest is unambiguous and takes precedence.
func splitTag(ref string) (name, tag string) {
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		return ref[:at], ref[at:]
	}
	if colon := strings.LastIndex(ref, ":"); colon > strings.LastIndex(ref, "/") {
		return ref[:colon], ref[colon:]
	}
	return ref, ""
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

func isCopy(line string) bool {
	return hasInstruction(line, "COPY")
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
