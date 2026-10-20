// Command pkgreg-client opens a verified session against a pkgreg cache.
//
// It is now a shim. Everything it did lives in pkgcache, which is the same program
// with a local cache it does not have to use: `pkgcache setup -no-cache` is exactly
// this client's behaviour, and every other mode is a superset of it.
//
// The shim exists because instructions outlive binaries. A tutorial, a Makefile and a
// CI job that say `pkgreg-client` should keep working across the release that merges
// the two, and be told once, on stderr, where they should be pointed instead.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	steps, err := translate(args, os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pkgreg-client: %v\n", err)
		return 2
	}
	announce(os.Stderr, steps)

	target, err := locate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pkgreg-client: %v\n", err)
		return 1
	}
	for _, step := range steps {
		if code := runStep(target, step); code != 0 {
			return code
		}
	}
	return 0
}

// translate turns one pkgreg-client command line into the pkgcache commands that do the
// same thing, run in order.
//
// Forwarding argv unchanged was right for build and compose, which kept their names, and
// wrong for the invocation every tutorial actually printed: `pkgreg-client -server …`
// became `pkgcache -server …`, which pkgcache rejects as an unknown command. So a first
// argument that is a flag is the old session form and is translated; anything else is a
// pkgcache subcommand and goes through as it is.
//
// The modes that installed things are not translated, and say what replaced them. Each
// replacement decides differently from the flag it replaces, and running it on somebody's
// behalf would make that decision for them.
func translate(args []string, getenv func(string) string, output io.Writer) ([][]string, error) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return [][]string{args}, nil
	}

	fs := flag.NewFlagSet("pkgreg-client", flag.ContinueOnError)
	fs.SetOutput(output)
	server := fs.String("server", "", "pkgreg HTTPS origin, for example https://pkgcache.internal:8443")
	project := fs.String("project", "", "project on the server to use (pkgcache setup -team-project)")
	fingerprint := fs.String("ca-sha256", "", "out-of-band CA SHA-256 fingerprint (colons optional)")
	caFile := fs.String("ca-file", "", "trusted CA file; also supplies the expected fingerprint")
	tokenFile := fs.String("token-file", getenv("PKGREG_TOKEN_FILE"),
		"file containing a project access token")
	shell := fs.String("shell", "", "shell to launch for the session")
	version := fs.Bool("version", false, "print build identity and exit")
	// Accepted so they can be answered rather than rejected as unknown: each is a
	// question somebody's instructions still ask, and "flag provided but not defined" is
	// not an answer to it.
	persist := fs.Bool("persist", false, "replaced; see below")
	dockerTrust := fs.Bool("docker-trust", false, "replaced by pkgcache docker-setup")
	dockerBuildTrust := fs.Bool("docker-build-trust", false, "replaced by pkgcache docker-build-setup")
	dryRun := fs.Bool("dry-run", false, "replaced; see below")
	uninstall := fs.Bool("uninstall", false, "replaced; see below")
	printScript := fs.Bool("print", false, "replaced; see below")
	host := fs.String("host", "", "replaced; see below")
	cacheIP := fs.String("cache-ip", "", "replaced; see below")
	cookieFile := fs.String("cookie-file", "", "not supported; use -token-file")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), `pkgreg-client — now a shim for pkgcache

usage:
  pkgreg-client -server https://cache:8443 -ca-sha256 FINGERPRINT [-project P] [-token-file T]
  pkgreg-client build|compose|<any pkgcache command> [arguments]

The first form runs, in order:
  pkgcache setup -no-cache -server … -ca-sha256 … [-team-project P] [-token-file T]
  pkgcache shell
which is what this client used to do: a verified loopback bridge to the team cache,
nothing cached locally, and a shell that exit leaves. The difference is that setup is
remembered — the server, its CA and any token stay until pkgcache setup -uninstall.
Anything else is passed to pkgcache unchanged.

Replaced modes:
  --persist             pkgcache persist, for this user's tool settings; machine-wide
                        trust on a managed or CI host is the project's setup.sh
  -docker-trust         pkgcache docker-setup
  -docker-build-trust   pkgcache docker-build-setup

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	switch {
	case *dockerBuildTrust:
		return nil, errors.New("-docker-build-trust is now `pkgcache docker-build-setup`, " +
			"which sends apt in every build on this machine through the cache; apk, pip and " +
			"npm in a build need `pkgcache build`")
	case *dockerTrust:
		return nil, errors.New("-docker-trust is now `pkgcache docker-setup`, " +
			"which teaches the Docker daemon about the cache")
	case *persist || *dryRun || *uninstall || *printScript || *host != "" || *cacheIP != "":
		// All of these changed persistent setup and nothing else, so they share an answer.
		return nil, errors.New("persistent setup (--persist, and -dry-run, -uninstall, -print, " +
			"-host and -cache-ip with it) is now `pkgcache persist` for this user's tool " +
			"settings. Machine-wide trust for a managed or CI host is the project's setup.sh, " +
			"from Console → Connect on the instance, run as root")
	case *cookieFile != "":
		return nil, errors.New("-cookie-file has no pkgcache equivalent; " +
			"save a project token in a protected file and pass -token-file")
	case *version:
		return [][]string{{"version"}}, nil
	case fs.NArg() > 0:
		return nil, fmt.Errorf("unexpected argument %q; the session form takes flags only", fs.Arg(0))
	case *server == "":
		fs.Usage()
		return nil, errors.New("-server is required")
	}

	setup := []string{"setup", "-no-cache", "-server", *server}
	if *project != "" {
		setup = append(setup, "-team-project", *project)
	}
	if *fingerprint != "" {
		setup = append(setup, "-ca-sha256", *fingerprint)
	}
	if *caFile != "" {
		setup = append(setup, "-ca-file", *caFile)
	}
	if *tokenFile != "" {
		setup = append(setup, "-token-file", *tokenFile)
	}
	session := []string{"shell"}
	if *shell != "" {
		// pkgcache shell takes the shell to run as its argument rather than a flag.
		session = append(session, *shell)
	}
	return [][]string{setup, session}, nil
}

// announce says, once, what this invocation has become, in the words that would do the
// same thing without the shim: the point is that the next copy of the instruction says
// pkgcache.
func announce(w io.Writer, steps [][]string) {
	lines := make([]string, 0, len(steps))
	for _, step := range steps {
		lines = append(lines, "  pkgcache "+strings.Join(step, " "))
	}
	_, _ = fmt.Fprintf(w, "pkgreg-client: this is now pkgcache, which does the same thing and "+
		"can also cache locally.\n  Running:\n%s\n", strings.Join(lines, "\n"))
}

// runStep runs one pkgcache command and returns the status this process should exit with
// if it failed, or 0.
//
// Wrapped rather than exec'd, because syscall.Exec does not exist on Windows and a shim
// that behaves differently per platform is a shim that gets debugged. The cost is one
// extra process for the release or two this exists.
//
// Standard streams are handed over directly, so the terminal, the child's prompt and its
// interactive shell all work; Ctrl-C reaches both through the process group.
func runStep(target string, step []string) int {
	// #nosec G204 -- target is pkgcache, resolved beside this binary or on PATH.
	cmd := exec.Command(target, step...) //nolint:noctx // Ctrl-C reaches the child through the process group; see the comment above
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			// pkgcache has already said whatever it wanted to; its status is the answer.
			return exit.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "pkgreg-client: run %s: %v\n", target, err)
		return 1
	}
	return 0
}

// locate prefers the pkgcache sitting beside this binary, because that is what a
// release ships and what an air-gapped host will have copied.
func locate() (string, error) {
	if self, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(self), executableName(runtime.GOOS))
		if info, statErr := os.Stat(beside); statErr == nil && !info.IsDir() {
			return beside, nil
		}
	}
	found, err := exec.LookPath("pkgcache")
	if err != nil {
		return "", fmt.Errorf(
			"pkgcache is not installed beside this binary or on PATH; install it from the "+
				"same place this came from: %w", err)
	}
	return found, nil
}

// executableName is pkgcache's file name on goos. LookPath adds .exe on Windows by
// itself; a path built by hand does not, so without this the pkgcache sitting right
// beside the shim was never found there.
func executableName(goos string) string {
	if goos == "windows" {
		return "pkgcache.exe"
	}
	return "pkgcache"
}
