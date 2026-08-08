// Command pkgreg is the package cache: six pull-through ecosystems, an operator
// console, and air-gap transfer, in one static binary with no containers.
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
	"serve":          {"run the cache and the control plane", runServe},
	"init":           {"create the data directory, mint TLS material, write a config", runInit},
	"publish-client": {"offer pkgreg-client binaries for download from this instance", runPublishClient},
	"doctor":         {"check configuration, storage and TLS, and report what is wrong", runDoctor},
	"audit":          {"print the immutable control-plane audit log", runAudit},
	"checkpoint":     {"create a content-addressed cache checkpoint", runCheckpoint},
	"snapshot":       {"alias for checkpoint", runCheckpoint},
	"rollback":       {"restore a project checkpoint", runRollback},
	"export":         {"write a full or delta air-gap pack", runExport},
	"import":         {"verify and apply an air-gap pack", runImport},
	"lockwarm":       {"warm and rewrite a uv.lock", runLockwarm},
	"gc":             {"collect unreferenced blobs", runGC},
	"evict":          {"apply LRU, TTL and free-space eviction policy", runEvict},
	"migrate":        {"import a live Python cache into the Go store", runMigrate},
	"systemd":        {"install and start the production service", runSystemd},
	"version":        {"print build identity", runVersion},
}

// main does nothing but translate dispatch's result into an exit status. Every exit
// path runs through one return so the signal handler's release is never skipped —
// os.Exit does not run deferred functions.
func main() {
	os.Exit(dispatch())
}

func dispatch() int {
	// One signal handler for the whole process; every subcommand honours the context.
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
		fmt.Fprintf(os.Stderr, "pkgreg: unknown command %q\n\n", name)
		usage()
		return 2
	}
	if err := cmd.run(ctx, args[1:]); err != nil {
		// Asking for help is not a failure. flag returns ErrHelp after it has already
		// printed the usage text, so there is nothing left to say and nothing to
		// report: printing "flag: help requested" as an error and exiting non-zero
		// made every `pkgreg <cmd> -h` look like a broken command to a shell probe.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "pkgreg: %v\n", err)
		return exitCode(err)
	}
	return 0
}

// exitCode maps a command's error to a status a script can branch on. Anything without
// a dedicated meaning is 1; only doctor's readiness classes have their own, and they
// are documented in `pkgreg doctor -h`.
func exitCode(err error) int {
	switch {
	case errors.Is(err, errNotInitialized):
		return 3
	case errors.Is(err, errUnsafePosture):
		return 4
	default:
		return 1
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `pkgreg — a versioned, air-gap-portable package cache

usage: pkgreg <command> [flags]

commands:
`)
	// Fixed order: a map iteration would shuffle the help text between runs.
	for _, name := range []string{
		"serve", "init", "publish-client", "doctor", "audit", "checkpoint", "rollback",
		"export", "import", "lockwarm", "migrate", "systemd", "version",
		"gc", "evict",
	} {
		fmt.Fprintf(os.Stderr, "  %-14s %s\n", name, commands[name].summary)
	}
	fmt.Fprint(os.Stderr, "\nrun `pkgreg <command> -h` for a command's flags\n")
}
