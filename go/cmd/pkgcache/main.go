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
	"status":  {"is the cache running, and what is in it", runStatus},
	"stop":    {"stop the cache daemon", runStop},
	"serve":   {"run the cache in the foreground", runServe},
	"version": {"print build identity", runVersion},
}

// order fixes the help text, which a map iteration would shuffle between runs.
var order = []string{"status", "stop", "serve", "version"}

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
		fmt.Fprintf(os.Stderr, "pkgcache: %v\n", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprint(os.Stderr, `pkgcache — a package cache for this machine

usage: pkgcache <command> [flags]

commands:
`)
	for _, name := range order {
		fmt.Fprintf(os.Stderr, "  %-14s %s\n", name, commands[name].summary)
	}
	fmt.Fprint(os.Stderr, "\nrun `pkgcache <command> -h` for a command's flags\n")
}
