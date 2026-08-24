// Command pkgcache is a package cache for one machine: six pull-through ecosystems on
// one loopback port, with no certificate, no account and no privileged setup.
//
// It is the same engine as pkgreg, composed differently. pkgreg is a host other
// machines point at, and everything that makes that safe — TLS, a CA, accounts,
// tokens, projects — is absent here, paid for by one enforced invariant: pkgcache
// refuses to bind an address another machine can reach.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// commands is the subcommand table. Each returns an exit-worthy error.
var commands = map[string]struct {
	summary string
	run     func(ctx context.Context, args []string) error
}{
	"setup":              {"point this machine at a cache, once", runSetup},
	"project":            {"the projects this cache serves", runProject},
	"run":                {"run one command with its tools pointed at the cache", runRun},
	"shell":              {"open a shell whose tools use the cache", runShell},
	"env":                {"print the settings that point tools at the cache", runEnv},
	"build":              {"run docker build through the cache", runBuild},
	"pull":               {"docker pull through the cache, keeping the image's own name", runPull},
	"warmlock":           {"warm the cache from a uv.lock, and point the lock at it", runWarmlock},
	"compose":            {"run docker compose through the cache", runCompose},
	"crate":              {"run crate with its builds served from the cache", runCrate},
	"persist":            {"settings that outlive the session", runPersist},
	"docker-setup":       {"teach the Docker daemon about this cache", runDockerSetup},
	"docker-build-setup": {"cache apt and apk in every build on this machine", runDockerBuildSetup},
	"checkpoint":         {"record what this project holds right now", runCheckpoint},
	"export":             {"put what this project holds into a pack", runExport},
	"import":             {"take a pack somebody carried here", runImport},
	"snapshots":          {"the checkpoints this project has", runSnapshots},
	"rollback":           {"make a checkpoint this project's content again", runRollback},
	"widget":             {"a small window that watches this cache", runWidget},
	"tray":               {"keep the cache in your status bar", runTray},
	"console":            {"open the full console in your browser", runConsole},
	"limit":              {"set how much disk this cache may use", runLimit},
	"prune":              {"reclaim space, now, because you asked", runPrune},
	"status":             {"is the cache running, and what is in it", runStatus},
	"stop":               {"stop the cache daemon", runStop},
	"serve":              {"run the cache in the foreground", runServe},
	"version":            {"print build identity", runVersion},
}

// order fixes the help text, which a map iteration would shuffle between runs.
var order = []string{
	"setup", "project", "run", "shell", "env", "build", "compose", "crate",
	"pull", "warmlock", "persist", "docker-setup", "docker-build-setup",
	"checkpoint", "export", "import", "snapshots", "rollback",
	"widget", "tray", "console",
	"limit", "status", "prune", "stop", "serve", "version",
}

func main() {
	os.Exit(dispatch())
}

func dispatch() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		return 2
	}
	name := args[0]
	if name == "-h" || name == "--help" || name == "help" {
		usage()
		return 0
	}
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "pkgcache: unknown command %q\n\n", name)
		usage()
		return 2
	}
	if err := cmd.run(ctx, args[1:]); err != nil {
		// Asking for help is not a failure: flag has already printed the usage text.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		var exit *exitError
		if errors.As(err, &exit) {
			if exit.err != nil {
				fmt.Fprintf(os.Stderr, "pkgcache: %v\n", exit.err)
			}
			return exit.code
		}
		fmt.Fprintf(os.Stderr, "pkgcache: %v\n", err)
		return 1
	}
	return 0
}

// exitError carries an exit status a caller should see instead of 1.
//
// Two things need it. A command run through `pkgcache run` has already reported its
// own failure, and its status is the honest answer — wrapping a compiler's exit 2 in
// pkgcache's exit 1 loses information a script may branch on. And a full cache exits
// 75, distinctly, whatever else happened.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit status %d", e.code)
}

func (e *exitError) Unwrap() error { return e.err }

func usage() {
	fmt.Fprint(os.Stderr, `pkgcache — a package cache for this machine

usage: pkgcache <command> [flags]

commands:
`)
	for _, name := range order {
		fmt.Fprintf(os.Stderr, "  %-20s %s\n", name, commands[name].summary)
	}
	fmt.Fprint(os.Stderr, "\nrun `pkgcache <command> -h` for a command's flags\n")
}
