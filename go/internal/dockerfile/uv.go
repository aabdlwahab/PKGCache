package dockerfile

import (
	"regexp"
	"strings"
)

// uvProjectRE finds a RUN that runs one of uv's project commands — the ones that read or
// write uv.lock. `uv pip` is not among them: it takes its index from UV_INDEX_URL like pip
// does, and needs nothing more.
var uvProjectRE = regexp.MustCompile(
	`\buv\s[^;&|\n]*?\b(sync|run|lock|add|remove|export|tree|version|init)\b`)

// shellRE reads the program a SHELL instruction names.
var shellRE = regexp.MustCompile(`(?i)^\s*SHELL\s+\[\s*"([^"]+)"`)

// lockFileHosts are the hosts whose files a uv.lock can be pointed at the cache for: the
// ones the pypi adapter's /+files/ route fetches from. They are pypi.LockFileHosts, which
// this package does not import; a test keeps the two in step.
var lockFileHosts = []string{
	"files.pythonhosted.org",
	"download.pytorch.org",
	"download-r2.pytorch.org",
	"pypi.nvidia.com",
	"flashinfer.ai",
}

// lockSed is the sed arguments that point a uv.lock's files at the cache, and back.
//
// Files only: the `url = "…"` of an sdist or a wheel, never a `source = ` line. A lock's
// source names the index it was made against, and PyTorch's index lives on one of these
// hosts — rewriting that line is what makes `--locked` refuse the lock, the failure this
// exists to avoid. uv writes each source on a line of its own.
//
// Each file's sha256, which the lock records beside its URL, travels as ?sha256=: the cache
// then serves a file it already holds under another name without fetching it again — a
// team that installed the same wheel from an index has it — and checks what it does fetch.
// The hand-back takes the query off again, so the lock comes back byte for byte.
func lockSed(files string) (lend, handBack string) {
	var there, back []string
	for _, host := range lockFileHosts {
		from, to := `url = "https://`+sedPattern(host)+`/`, `url = "`+files+host+`/`
		there = append(there,
			`-e '/source = /!s#`+from+`\([^"]*\)", hash = "sha256:\([0-9a-f]*\)"#`+
				to+`\1?sha256=\2", hash = "sha256:\2"#g'`,
			`-e '/source = /!s#`+from+`#`+to+`#g'`)
		cached := `url = "` + sedPattern(files+host+"/")
		back = append(back,
			`-e '/source = /!s#`+cached+`\([^"?]*\)?sha256=[0-9a-f]*"#url = "https://`+host+`/\1"#g'`,
			`-e '/source = /!s#`+cached+`#url = "https://`+host+`/#g'`)
	}
	return strings.Join(there, " "), strings.Join(back, " ")
}

// sedPattern escapes what a basic regular expression would read as more than itself.
func sedPattern(literal string) string {
	var b strings.Builder
	for _, r := range literal {
		if strings.ContainsRune(`.[]*^$\`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// UVWrapper is the shell function put in front of a RUN that runs a uv project command.
//
// uv's project commands and the index settings a build is given do not mix. `uv sync
// --frozen` and `--locked` fetch the exact addresses uv.lock records — files.pythonhosted.org
// — whatever UV_INDEX_URL says, so every locked install in a build went past the cache. And
// UV_INDEX_URL, which `uv pip` needs, makes `uv sync --locked` refuse the lock outright,
// because the index it names is not the one the lock was made against: a Dockerfile that
// builds as it is failed under the cache. Measured with uv 0.9.30; uv documents no mirror
// setting that would do this instead.
//
// So for the length of a `uv sync` or `uv run`, the PyPI addresses in the nearest uv.lock
// are pointed at the cache's copy of the same paths (pypi's /+files/), and then pointed
// back, the way apt's repositories are handed back. The lock's index is untouched, so
// `--locked` still accepts it; uv checks each file against the hash the lock records, so
// what is installed cannot change, only where it comes from. The index this build was
// given is withheld from every project command, so none of them fails on it or writes
// the cache's address into a lock. Anything else — `uv pip`, `uv tool` — is run as it was.
//
// The lock is rewritten in place — through a temporary copy and `cat >`, not `sed -i` —
// because uv's own Docker guide bind-mounts it into the step, and a bind-mounted file can
// be written but not replaced: sed -i's rename fails on it. The mount itself is made
// writable by wrapUV; see writableLockMounts.
//
// POSIX sh on one line, since it is prepended to the RUN's own. It is defined whether or
// not uv is installed yet: `pip install uv && uv sync --locked` is a common line, and
// leaving that one unwrapped is the failure this exists to prevent. The price is a RUN
// that probes `command -v uv` in the same line finding the function.
func UVWrapper(o Options) string {
	base := strings.TrimRight(o.Base, "/") + "/" + o.Project + "/pypi/"
	index := base + "root/pypi/+simple/"
	lend, handBack := lockSed(base + "+files/")
	return `uv() { ` +
		`__pkgcache_sub=; __pkgcache_dir=; __pkgcache_next=; ` +
		`for __pkgcache_a in "$@"; do ` +
		`case $__pkgcache_next in dir) __pkgcache_dir=$__pkgcache_a; __pkgcache_next=; continue;; ` +
		`skip) __pkgcache_next=; continue;; esac; ` +
		`case $__pkgcache_a in --project|--directory) __pkgcache_next=dir;; ` +
		`--project=*|--directory=*) __pkgcache_dir=${__pkgcache_a#*=};; ` +
		`--cache-dir|--config-file|--color|--allow-insecure-host) __pkgcache_next=skip;; ` +
		`-*) ;; *) if [ -z "$__pkgcache_sub" ]; then __pkgcache_sub=$__pkgcache_a; fi;; esac; done; ` +
		`case $__pkgcache_sub in sync|run|lock|add|remove|export|tree|version|init) ;; ` +
		`*) command uv "$@"; return;; esac; ` +
		`__pkgcache_lock=; ` +
		`case $__pkgcache_sub in sync|run) ` +
		`__pkgcache_d=$(CDPATH= cd -- "${__pkgcache_dir:-.}" 2>/dev/null && pwd) || __pkgcache_d=; ` +
		`while [ -n "$__pkgcache_d" ]; do ` +
		`if [ -f "$__pkgcache_d/uv.lock" ]; then __pkgcache_lock=$__pkgcache_d/uv.lock; break; fi; ` +
		`if [ "$__pkgcache_d" = / ]; then break; fi; ` +
		`__pkgcache_d=${__pkgcache_d%/*}; [ -n "$__pkgcache_d" ] || __pkgcache_d=/; done; ` +
		`__pkgcache_tmp=${TMPDIR:-/tmp}/.pkgcache-uv-lock.$$; ` +
		`if [ -n "$__pkgcache_lock" ] && [ -w "$__pkgcache_lock" ] && ` +
		`sed ` + lend + ` "$__pkgcache_lock" > "$__pkgcache_tmp" 2>/dev/null && ` +
		`cat "$__pkgcache_tmp" > "$__pkgcache_lock" 2>/dev/null; ` +
		`then :; else __pkgcache_lock=; fi; rm -f "$__pkgcache_tmp";; esac; ` +
		`__pkgcache_status=0; ` +
		`( if [ "${UV_INDEX_URL:-}" = '` + index + `' ]; then unset UV_INDEX_URL; fi; exec uv "$@" ) ` +
		`|| __pkgcache_status=$?; ` +
		`if [ -n "$__pkgcache_lock" ]; then ` +
		`sed ` + handBack + ` "$__pkgcache_lock" > "$__pkgcache_tmp" && ` +
		`cat "$__pkgcache_tmp" > "$__pkgcache_lock"; rm -f "$__pkgcache_tmp"; fi; ` +
		`return $__pkgcache_status; };`
}

// wrapUV puts UVWrapper in front of a shell-form RUN that runs a uv project command, after
// the RUN's own flags. An exec-form RUN has no shell to define it in, and a heredoc's
// script is its own; both are left as they are.
func wrapUV(line string, o Options) (string, bool) {
	match := runRE.FindStringSubmatch(line)
	if match == nil || !uvProjectRE.MatchString(line) {
		return line, false
	}
	flags, command := splitRunFlags(match[2])
	if strings.HasPrefix(command, "[") || strings.HasPrefix(command, "<<") ||
		strings.Contains(command, UVWrapper(o)) {
		return line, false
	}
	return match[1] + writableLockMounts(flags) + UVWrapper(o) + " " + command, true
}

// mountFlagRE is one --mount flag of a RUN.
var mountFlagRE = regexp.MustCompile(`--mount=\S+`)

// writableLockMounts lets the wrapper lend a bind-mounted uv.lock to the cache.
//
// uv's documentation builds images with `--mount=type=bind,source=uv.lock,target=uv.lock`,
// and a bind mount is read-only unless it says otherwise, so the wrapper found the lock
// unwritable, left it alone, and the whole sync went to PyPI. rw on a bind mount lets the
// step write it and BuildKit throws the writes away when the step ends, so neither the
// build context nor the image can see the change. A mount written read-only on purpose
// (ro, readonly) is left as it is.
func writableLockMounts(flags string) string {
	return mountFlagRE.ReplaceAllStringFunc(flags, func(flag string) string {
		bind, lock, fixed := true, false, false
		for _, option := range strings.Split(strings.TrimPrefix(flag, "--mount="), ",") {
			key, value, _ := strings.Cut(option, "=")
			switch strings.ToLower(key) {
			case "type":
				bind = value == "bind"
			case "target", "dst", "destination":
				lock = value == "uv.lock" || strings.HasSuffix(value, "/uv.lock")
			case "rw", "readwrite", "ro", "readonly":
				fixed = true
			}
		}
		if bind && lock && !fixed {
			return flag + ",rw"
		}
		return flag
	})
}

// splitRunFlags separates a RUN's own flags (--mount, --network, --security) from its
// command, keeping every character of both, line continuations included.
func splitRunFlags(rest string) (flags, command string) {
	at := 0
	for {
		start := at
		for start < len(rest) {
			switch {
			case rest[start] == ' ' || rest[start] == '\t':
				start++
				continue
			case rest[start] == '\\' && start+1 < len(rest) && rest[start+1] == '\n':
				start += 2
				continue
			}
			break
		}
		if !strings.HasPrefix(rest[start:], "--") {
			return rest[:start], rest[start:]
		}
		end := start
		for end < len(rest) && rest[end] != ' ' && rest[end] != '\t' && rest[end] != '\n' {
			end++
		}
		at = end
	}
}

// posixShell reports whether a SHELL instruction leaves the stage on a shell the wrapper
// is written for. Anything else — PowerShell, cmd — keeps its RUNs as written.
func posixShell(line string, current bool) bool {
	match := shellRE.FindStringSubmatch(line)
	if match == nil {
		return current
	}
	program := match[1][strings.LastIndexAny(match[1], `/\`)+1:]
	switch program {
	case "sh", "bash", "dash", "ash", "busybox":
		return true
	}
	return false
}
