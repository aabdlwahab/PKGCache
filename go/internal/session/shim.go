package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// lookupReal opens every shim: it finds the program the shim stands in for on PATH,
// skipping the shim's own directory, or says it is missing the way a shell would.
func lookupReal(dir, program string) string {
	return `shims='` + dir + `'
real=
saved_ifs=$IFS
IFS=:
set -f
for dir in $PATH; do
  if [ -z "$dir" ] || [ "$dir" = "$shims" ]; then continue; fi
  if [ -f "$dir/` + program + `" ] && [ -x "$dir/` + program + `" ]; then real=$dir/` + program + `; break; fi
done
set +f
IFS=$saved_ifs
if [ -z "$real" ]; then
  echo "` + program + `: command not found" >&2
  exit 127
fi
`
}

// UVShim is the uv a session puts first on PATH.
//
// A session points uv at the cache with UV_INDEX_URL, which is what `uv pip` reads. uv's
// project commands read it too, and cannot live with it: `uv sync --locked` refuses a lock
// made against PyPI because the index no longer matches it, and `uv lock`, `uv add` and a
// plain `uv sync` write the cache's loopback address into uv.lock — a file that gets
// committed, and then fails for everyone without this cache. uv has no setting that
// reaches `uv pip` alone except a config file, and naming one hides the user's own.
//
// So this stands in front of uv and withholds the session's index from the project
// commands; everything else passes straight through. Those commands then fetch from PyPI
// directly, which is slower than the cache and never wrong. Nothing here touches a lock:
// a build lends its lock to the cache for the length of a sync (see dockerfile.UVWrapper),
// and a developer's working tree is not a thing to borrow.
//
// Only the session's own index is withheld, recognised from the session's variables, so
// an index somebody set for themselves is left alone — and outside a session the shim is
// uv. dir is where the shim lives, skipped when looking for the real one.
func UVShim(dir string) string {
	return `#!/bin/sh
# Written by pkgcache for its sessions: withholds the session's UV_INDEX_URL from uv's
# project commands, which record it in uv.lock. Everything else is uv itself.
` + lookupReal(dir, "uv") + `sub= next=
for arg in "$@"; do
  if [ -n "$next" ]; then next=; continue; fi
  case $arg in
    --project|--directory|--cache-dir|--config-file|--color|--allow-insecure-host) next=1 ;;
    -*) ;;
    *) sub=$arg; break ;;
  esac
done
case $sub in
  sync|run|lock|add|remove|export|tree|version|init)
    session_index="${PKGCACHE_BRIDGE_URL:-}/${PKGCACHE_PROJECT:-global}/pypi/root/pypi/+simple/"
    if [ -n "${PKGCACHE_BRIDGE_URL:-}" ] && [ "${UV_INDEX_URL:-}" = "$session_index" ]; then
      unset UV_INDEX_URL
    fi
    ;;
esac
exec "$real" "$@"
`
}

// The options that carry a credential meant for the forge, or read one from somewhere the
// shim cannot see. A download given any of them is left exactly as written: the cache
// fetches a release asset with its own credentials, never the caller's, so a private
// asset has to go the way its caller sent it. A short option counts wherever it sits in
// a cluster (-fsSLu), which errs towards leaving a download alone.
const (
	curlCredentials = `--user|--user=*|--netrc|--netrc-optional|--netrc-file|--netrc-file=*|` +
		`--oauth2-bearer|--oauth2-bearer=*|--header|--header=*|--config|--config=*|` +
		`--cert|--cert=*|--key|--key=*|--aws-sigv4|--aws-sigv4=*|` +
		`--negotiate|--ntlm|--digest|--basic|--anyauth|-[unKEH]*|-[!-]*[unKEH]*`
	wgetCredentials = `--user|--user=*|--password|--password=*|--http-user|--http-user=*|` +
		`--http-password|--http-password=*|--ask-password|--use-askpass|--use-askpass=*|` +
		`--header|--header=*|--load-cookies|--load-cookies=*|--certificate|--certificate=*|` +
		`--private-key|--private-key=*|--execute|--execute=*|-e*|-[!-]*e*`
)

// DownloadShim is the curl or wget a session puts first on PATH.
//
// A forge's release asset is fetched by URL, so no index variable reaches it: one
// project's release build curls a 40 MB CPython from GitHub's releases, and under `pkgcache run` it
// went to the internet while everything around it came from the cache. A Dockerfile's
// release URLs are rewritten for the same reason (see dockerfile.rewriteReleaseURLs); a
// host script has no Dockerfile to rewrite, but its curl can be stood in front of.
//
// The mapping is the session's own: the insteadOf pairs it exports as GIT_CONFIG_* so
// clones go through the cache, recognised as the session's by pointing at its bridge, and
// applied to a release download alone — https://<host>/<owner>/<repo>/releases/download/…
// with a literal owner and repository, the form the git adapter serves. Any other URL, a
// download that carries credentials, and everything outside a session go to the real
// program unchanged, arguments byte for byte.
func DownloadShim(dir, program string) string {
	credentials := curlCredentials
	if program == "wget" {
		credentials = wgetCredentials
	}
	return `#!/bin/sh
# Written by pkgcache for its sessions: sends a forge's release downloads through the
# cache, as a Docker build's are. Everything else is ` + program + ` itself.
` + lookupReal(dir, program) + `case ${GIT_CONFIG_COUNT:-0} in
  ''|*[!0-9]*) count=0 ;;
  *) count=${GIT_CONFIG_COUNT:-0} ;;
esac
if [ -z "${PKGCACHE_BRIDGE_URL:-}" ] || [ "$count" -eq 0 ]; then exec "$real" "$@"; fi
for arg in "$@"; do
  case $arg in
    ` + credentials + `) exec "$real" "$@" ;;
  esac
done
rewrite() {
  out=$1
  case $1 in https://*/releases/download/?*) ;; *) return 0 ;; esac
  i=0
  while [ "$i" -lt "$count" ]; do
    eval "key=\${GIT_CONFIG_KEY_$i:-} value=\${GIT_CONFIG_VALUE_$i:-}"
    i=$((i + 1))
    case $key in "url.$PKGCACHE_BRIDGE_URL/"*/.insteadOf) ;; *) continue ;; esac
    case $value in https://?*/) ;; *) continue ;; esac
    case $1 in "$value"?*) ;; *) continue ;; esac
    rest=${1#"$value"}
    owner=${rest%%/*}
    rest=${rest#*/}
    repo=${rest%%/*}
    rest=${rest#*/}
    case $owner in ''|*[!A-Za-z0-9._-]*) return 0 ;; esac
    case $repo in ''|*[!A-Za-z0-9._-]*) return 0 ;; esac
    case $rest in releases/download/?*) ;; *) return 0 ;; esac
    base=${key#url.}
    out=${base%.insteadOf}$owner/$repo/$rest
    return 0
  done
}
for arg do
  shift
  rewrite "$arg"
  set -- "$@" "$out"
done
exec "$real" "$@"
`
}

// shimmed is every program a session can stand in front of, and its shim.
var shimmed = []struct {
	name string
	shim func(dir string) string
}{
	{"uv", UVShim},
	{"curl", func(dir string) string { return DownloadShim(dir, "curl") }},
	{"wget", func(dir string) string { return DownloadShim(dir, "wget") }},
}

// ShimDir is where the session shims are written: under the user's cache directory, not
// the cache's own data directory, which a bridge-only session promises not to write to.
func ShimDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "pkgcache", "shims"), nil
}

// WriteShims writes the session shims for the programs found on path, the PATH the
// session starts from, and returns the directory to put first on it, or "" where there
// are none. Windows has none: it runs no sh, and a batch file cannot pass arguments
// through faithfully enough to stand in for a program.
//
// A program that is not installed gets no shim, and loses one an earlier session wrote:
// a script that asks `command -v uv` before choosing between uv and pip would otherwise
// find the shim, choose uv, and fail on a machine that has none.
func WriteShims(dir, path string) (string, error) {
	if runtime.GOOS == "windows" {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- on PATH: others need not write, but may read
		return "", fmt.Errorf("session: shim directory: %w", err)
	}
	for _, program := range shimmed {
		target := filepath.Join(dir, program.name)
		if !installed(program.name, path, dir) {
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("session: remove shim: %w", err)
			}
			continue
		}
		if err := writeShim(target, []byte(program.shim(dir))); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// installed reports whether program is an executable file in a directory of path other
// than the shims' own — the same search the shim makes when it runs.
func installed(program, path, shims string) bool {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || dir == shims {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, program))
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return true
		}
	}
	return false
}

func writeShim(path string, content []byte) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, content) { // #nosec G304 -- the shim directory's own file
		return nil
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, content, 0o755); err != nil { // #nosec G306 -- an executable on PATH
		return fmt.Errorf("session: write shim: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("session: install shim: %w", err)
	}
	return nil
}
