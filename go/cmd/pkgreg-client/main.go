// Command pkgreg-client opens a temporary configured shell or persists machine setup.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/brightskies/pkgreg/internal/buildinfo"
	"github.com/brightskies/pkgreg/internal/clientbuild"
	"github.com/brightskies/pkgreg/internal/clientinstaller"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "pkgreg-client: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Subcommands are dispatched before flag parsing because everything after them
	// belongs to docker, not to us. `pkgreg-client build -t app .` must be able to
	// carry any docker flag, including ones this program has never heard of.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "build":
			return buildCommand(os.Args[2:], false)
		case "compose":
			return buildCommand(os.Args[2:], true)
		}
	}
	fs := flag.NewFlagSet("pkgreg-client", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	options := clientinstaller.Options{}
	version := fs.Bool("version", false, "print build identity and exit")
	fs.StringVar(&options.Server, "server", "",
		"pkgreg HTTPS origin, for example https://pkgcache.internal:8443")
	fs.StringVar(&options.Project, "project", "global", "project to configure")
	fs.StringVar(&options.ExpectedSHA256, "ca-sha256", "",
		"out-of-band CA SHA-256 fingerprint (colons optional)")
	fs.StringVar(&options.CAFile, "ca-file", "",
		"trusted CA file; also supplies the expected fingerprint")
	fs.StringVar(&options.CookieFile, "cookie-file", "",
		"file containing one raw Cookie header for an authenticated control plane")
	fs.StringVar(&options.TokenFile, "token-file", os.Getenv("PKGREG_TOKEN_FILE"),
		"file containing a project access token for a temporary session")
	fs.StringVar(&options.Host, "host", "", "override the generated cache hostname")
	fs.StringVar(&options.CacheIP, "cache-ip", "", "add a managed hostname mapping")
	fs.StringVar(&options.Shell, "shell", "", "shell to launch for the temporary session")
	fs.BoolVar(&options.Persist, "persist", false,
		"install machine-wide trust and settings instead of opening a temporary shell")
	fs.BoolVar(&options.DockerTrust, "docker-trust", false,
		"install this cache's CA for the Docker daemon only, so docker can pull from it")
	fs.BoolVar(&options.DockerBuildTrust, "docker-build-trust", false,
		"point docker builds on this machine at the cache's apt/apk proxy, so OS packages "+
			"are cached with no Dockerfile change")
	fs.BoolVar(&options.DryRun, "dry-run", false,
		"with -persist or -docker-trust, print every change without applying it")
	fs.BoolVar(&options.Uninstall, "uninstall", false,
		"with -persist or -docker-trust, reverse a previous installation")
	fs.BoolVar(&options.Print, "print", false,
		"with -persist, print the verified setup script instead of executing it")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), `pkgreg-client — open a verified, temporary pkgreg shell

usage:
  pkgreg-client -server https://cache:8443 -ca-sha256 FINGERPRINT
  pkgreg-client -docker-trust -server https://cache:8443 -ca-sha256 FINGERPRINT
  pkgreg-client --persist -server https://cache:8443 -ca-sha256 FINGERPRINT
  pkgreg-client build [docker build flags] PATH
  pkgreg-client compose [docker compose flags] COMMAND

The default starts a child shell configured through a verified localhost bridge.
Type exit to restore the terminal's previous environment. Nothing is installed.
For a token-gated project, save its token in a protected file and pass -token-file.

Docker is the exception to that shell: its daemon is a separate process that never
sees the shell's environment, and on macOS and Windows it runs in a virtual machine
whose loopback is not yours, so the bridge address is unreachable. -docker-trust
installs this cache's CA for the daemon alone — one file, no administrator access on
Docker Desktop — after which docker pulls from the cache's own address.

Use --persist for a shared or CI host that needs machine-wide CA and tool settings.

build and compose run docker for you against a Dockerfile they rewrite in memory, so
the file in your repository never mentions this cache and still works on a machine
that has never heard of one. Run them from inside a pkgreg shell; add -print to see
exactly what would be built, and -cache-address on Docker Desktop or in CI.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *version {
		fmt.Println(buildinfo.Get().String())
		return nil
	}
	// -docker-build-trust can take its address from a running session, so it is the
	// one mode that does not always need -server.
	if options.Server == "" && !(options.DockerBuildTrust && os.Getenv("PKGREG_APT_PROXY") != "") {
		fs.Usage()
		return fmt.Errorf("-server is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return clientinstaller.Run(ctx, options)
}

// buildCommand runs `pkgreg-client build` or `pkgreg-client compose`.
//
// Its own flags are parsed only up to the first argument it does not recognise;
// everything from there is docker's. That ordering is what lets the command stay a
// wrapper rather than a second, permanently incomplete docker CLI.
func buildCommand(argv []string, compose bool) error {
	name := "build"
	if compose {
		name = "compose"
	}
	fs := flag.NewFlagSet("pkgreg-client "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	options := clientbuild.Options{}
	fs.BoolVar(&options.Print, "print", false,
		"write the rewritten Dockerfile or Compose file and build nothing")
	fs.BoolVar(&options.CacheAddress, "cache-address", false,
		"build against the cache's own address instead of the loopback bridge, for a "+
			"daemon that cannot see it: Docker Desktop, a remote daemon, CI")
	fs.BoolVar(&options.SkipFrom, "keep-images", false,
		"leave image names alone, for a daemon that already resolves them through the cache")
	fs.StringVar(&options.Server, "server", "", "override PKGREG_SERVER")
	fs.StringVar(&options.CAFile, "ca-file", "", "override PKGREG_CA_FILE")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `pkgreg-client %s — run docker %s through the cache

usage:
  pkgreg-client %s [flags] [docker %s arguments]

Everything this does not recognise is passed to docker untouched. Your Dockerfile and
Compose file are never modified: the rewrite happens in memory, and the generated
Dockerfile is written outside the build context so a COPY cannot pick it up.

flags:
`, name, name, name, name)
		fs.PrintDefaults()
	}

	// Split our flags from docker's at the first argument we do not own.
	ours, theirs := splitFlags(fs, argv)
	if err := fs.Parse(ours); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	options = clientbuild.FromEnvironment(options)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if compose {
		return clientbuild.Compose(ctx, options, append(fs.Args(), theirs...))
	}
	return clientbuild.Build(ctx, options, append(fs.Args(), theirs...))
}

// splitFlags divides argv into the leading flags this command defines and everything
// else. Go's flag package stops at the first non-flag, which would silently hand
// `-t app:dev` to us as a positional argument and then complain about it.
func splitFlags(fs *flag.FlagSet, argv []string) (ours, theirs []string) {
	known := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { known[f.Name] = true })
	for i := 0; i < len(argv); i++ {
		argument := argv[i]
		trimmed := strings.TrimLeft(argument, "-")
		base, _, _ := strings.Cut(trimmed, "=")
		if strings.HasPrefix(argument, "-") && known[base] {
			ours = append(ours, argument)
			continue
		}
		return ours, argv[i:]
	}
	return ours, nil
}
