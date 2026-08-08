package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/local"
)

// bindLocalFlags wires the flags every pkgcache command accepts, so `serve` and the
// commands that auto-start it cannot disagree about what -addr or -data-dir mean.
func bindLocalFlags(fs *flag.FlagSet) func() config.LocalFlags {
	var (
		dataDir     = fs.String("data-dir", "", "cache directory (default: this user's)")
		addr        = fs.String("addr", "", "loopback address or port to serve on")
		logLevel    = fs.String("log-level", "", "debug|info|warn|error")
		offline     = fs.Bool("offline", false, "serve from cache only; never contact an upstream")
		idleTimeout = fs.Duration("idle-timeout", 0,
			"exit after this long with nothing to do (0 stays up)")
	)
	return func() config.LocalFlags {
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		out := config.LocalFlags{DataDir: *dataDir, Addr: *addr, LogLevel: *logLevel}
		if set["offline"] {
			out.Offline = offline
		}
		if set["idle-timeout"] {
			out.IdleTimeout = idleTimeout
		}
		return out
	}
}

// loadSnapshot parses the shared flags and resolves the configuration behind them.
func loadSnapshot(name string, args []string, usage string) (*config.Snapshot, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return config.LoadLocal(collect())
}

func runServe(ctx context.Context, args []string) error {
	snap, err := loadSnapshot("serve", args, `pkgcache serve — run the cache in the foreground

usage: pkgcache serve [flags]

Binds one loopback port serving pypi, npm, oci, git, files and the apt/apk forward
proxy, plus the console and the API. Refuses any address another machine can reach.

Most people never run this: `+"`pkgcache run`"+` starts a daemon on demand.

flags:
`)
	if err != nil {
		return err
	}
	return local.Run(ctx, local.RunOptions{Snapshot: snap, Notes: os.Stderr})
}
