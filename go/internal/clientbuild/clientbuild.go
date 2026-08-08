// Package clientbuild runs `docker build` and `docker compose` against a Dockerfile
// the developer never has to edit.
//
// The settings a build needs are already sitting in the environment of the shell
// pkgreg-client opened. This takes them from there, rewrites the build in memory, and
// hands the result to Docker — so the file in the repository stays the file someone
// would have written with no cache at all, and still works on a machine that has never
// heard of one.
//
// Nothing is written into the project. The generated Dockerfile goes to a temporary
// directory, deliberately outside the build context: a `COPY . .` would otherwise
// sweep it into the image.
package clientbuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brightskies/pkgreg/internal/dockerfile"
)

// Options is what the caller learned from the environment or from flags.
type Options struct {
	// Session settings, normally read from the environment of a pkgreg shell.
	Bridge   string // PKGREG_BRIDGE_URL
	Registry string // PKGREG_DOCKER_REGISTRY
	Project  string // PKGREG_PROJECT
	AptProxy string // PKGREG_APT_PROXY
	CAFile   string // PKGREG_CA_FILE, only needed away from the bridge
	Server   string // PKGREG_SERVER, the cache's own origin

	// CacheAddress forces the mode used where a daemon cannot see this machine's
	// loopback: Docker Desktop, a remote daemon, a container builder, CI.
	CacheAddress bool
	// GitHosts are redirected through the cache's mirror.
	GitHosts []string
	// SkipFrom leaves image names alone, for a daemon that already resolves them
	// through the cache.
	SkipFrom bool
	// Print writes the generated file instead of building. The single most useful
	// thing a tool that rewrites your build can offer.
	Print bool

	Stdout, Stderr io.Writer
	// Runner executes the docker command. Injected so the tests never need Docker.
	Runner func(ctx context.Context, name string, args []string) error
}

// FromEnvironment fills in what a pkgreg shell already exported.
func FromEnvironment(o Options) Options {
	set := func(target *string, name string) {
		if *target == "" {
			*target = strings.TrimSpace(os.Getenv(name))
		}
	}
	set(&o.Bridge, "PKGREG_BRIDGE_URL")
	set(&o.Registry, "PKGREG_DOCKER_REGISTRY")
	set(&o.Project, "PKGREG_PROJECT")
	set(&o.AptProxy, "PKGREG_APT_PROXY")
	set(&o.CAFile, "PKGREG_CA_FILE")
	set(&o.Server, "PKGREG_SERVER")
	if o.Project == "" {
		o.Project = "global"
	}
	if len(o.GitHosts) == 0 {
		o.GitHosts = []string{"github.com"}
	}
	return o
}

// rewriteOptions turns the session into the rewriter's view of it.
func (o Options) rewriteOptions() (dockerfile.Options, error) {
	options := dockerfile.Options{
		Project: o.Project, AptProxy: o.AptProxy,
		GitHosts: o.GitHosts, SkipFrom: o.SkipFrom,
	}
	if o.CacheAddress {
		if o.Server == "" {
			return options, errors.New(
				"this build needs the cache's own address, but PKGREG_SERVER is not set;\n" +
					"run this inside a pkgreg shell, or pass -server")
		}
		if o.CAFile == "" {
			return options, errors.New(
				"this build needs the cache's CA to hand to each step, but PKGREG_CA_FILE\n" +
					"is not set; run this inside a pkgreg shell, or pass -ca-file")
		}
		options.Mode = dockerfile.CacheAddress
		options.Base = strings.TrimRight(o.Server, "/")
		options.Registry = authorityOf(o.Server)
		return options, nil
	}
	if o.Bridge == "" {
		return options, errors.New(
			"no pkgreg session found: PKGREG_BRIDGE_URL is not set.\n" +
				"Start one with pkgreg-client, or pass -cache-address to build against\n" +
				"the cache's own address instead (needed on Docker Desktop and in CI)")
	}
	options.Mode = dockerfile.Bridge
	options.Base = strings.TrimRight(o.Bridge, "/")
	options.Registry = o.Registry
	if options.Registry == "" {
		options.Registry = authorityOf(o.Bridge)
	}
	return options, nil
}

// Build rewrites the Dockerfile named in args (or ./Dockerfile) and runs docker build.
//
// Everything else in args is passed through untouched. This is a wrapper, not a
// reimplementation: any flag Docker grows tomorrow keeps working without a release
// here, and anything this does not understand is Docker's business.
func Build(ctx context.Context, o Options, args []string) error {
	o = withDefaults(o)
	rewrite, err := o.rewriteOptions()
	if err != nil {
		return err
	}

	path, rest := extractDockerfileFlag(args)
	if path == "" {
		path = filepath.Join(contextDir(rest), "Dockerfile")
	}
	source, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	result, err := dockerfile.Rewrite(source, rewrite)
	if err != nil {
		return err
	}
	if o.Print {
		_, err := o.Stdout.Write(result.Content)
		return err
	}

	generated, cleanup, err := writeTemp(result.Content)
	if err != nil {
		return err
	}
	defer cleanup()

	report(o.Stderr, result.Changes)
	command := []string{"build", "-f", generated}
	if rewrite.Mode == dockerfile.Bridge {
		// The RUN steps talk to a loopback address, which only exists in the build
		// container if it shares this machine's network namespace.
		command = append(command, "--network=host")
	}
	if result.NeedsSecret {
		command = append(command,
			"--secret", "id="+dockerfile.SecretID+",src="+o.CAFile)
	}
	return o.Runner(ctx, "docker", append(command, rest...))
}

// Compose rewrites a rendered Compose configuration and feeds it back on stdin.
func Compose(ctx context.Context, o Options, args []string) error {
	o = withDefaults(o)
	rewrite, err := o.rewriteOptions()
	if err != nil {
		return err
	}

	// Flags that change which services and values Compose renders have to reach the
	// `config` call too, or the rewrite is computed against a different project than
	// the one about to be built.
	rendered, err := o.render(ctx, selectionFlags(args))
	if err != nil {
		return err
	}
	result, err := dockerfile.RewriteCompose(rendered, rewrite, os.ReadFile)
	if err != nil {
		return err
	}

	document := result.Content
	var cleanups []func()
	defer func() {
		for _, done := range cleanups {
			done()
		}
	}()
	needsSecret := false
	for service, build := range result.Dockerfiles {
		generated, cleanup, err := writeTemp(build.Content)
		if err != nil {
			return err
		}
		cleanups = append(cleanups, cleanup)
		if document, err = dockerfile.SetComposeDockerfile(document, service, generated); err != nil {
			return err
		}
		needsSecret = needsSecret || build.NeedsSecret
	}
	if o.Print {
		_, err := o.Stdout.Write(document)
		return err
	}
	report(o.Stderr, result.Changes)
	if needsSecret {
		fmt.Fprintf(o.Stderr,
			"pkgreg: this build needs the cache CA; add to each service's build:\n"+
				"  secrets: [%s]\nwith a top-level secrets entry pointing at %s\n",
			dockerfile.SecretID, o.CAFile)
	}
	// On stdin rather than a temporary file on purpose: a rendered configuration
	// carries interpolated environment values, which can include credentials, and
	// none of this needs to touch the disk.
	return o.runStdin(ctx, document, append([]string{"compose", "-f", "-"}, args...))
}

func (o Options) render(ctx context.Context, selection []string) ([]byte, error) {
	command := exec.CommandContext(ctx, "docker",
		append(append([]string{"compose"}, selection...), "config")...)
	command.Stderr = o.Stderr
	out, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("docker compose config: %w", err)
	}
	return out, nil
}

func (o Options) runStdin(ctx context.Context, input []byte, args []string) error {
	command := exec.CommandContext(ctx, "docker", args...)
	command.Stdin = strings.NewReader(string(input))
	command.Stdout, command.Stderr = o.Stdout, o.Stderr
	return command.Run()
}

func withDefaults(o Options) Options {
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Runner == nil {
		o.Runner = func(ctx context.Context, name string, args []string) error {
			command := exec.CommandContext(ctx, name, args...)
			command.Stdin, command.Stdout, command.Stderr = os.Stdin, o.Stdout, o.Stderr
			return command.Run()
		}
	}
	return o
}

// report prints every substitution. A tool that silently changes what gets built is a
// tool people stop trusting the first time a build surprises them.
func report(w io.Writer, changes []dockerfile.Change) {
	for _, change := range changes {
		fmt.Fprintf(w, "pkgreg: %s -> %s\n", change.From, change.To)
	}
}

// writeTemp puts the generated Dockerfile outside the build context, where a
// `COPY . .` cannot pick it up.
func writeTemp(content []byte) (string, func(), error) {
	file, err := os.CreateTemp("", "pkgreg-*.Dockerfile")
	if err != nil {
		return "", nil, fmt.Errorf("create generated Dockerfile: %w", err)
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return "", nil, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())
		return "", nil, err
	}
	return file.Name(), func() { _ = os.Remove(file.Name()) }, nil
}

// extractDockerfileFlag removes -f/--file from args and returns its value, because
// the generated file replaces it.
func extractDockerfileFlag(args []string) (string, []string) {
	rest := make([]string, 0, len(args))
	path := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-f" || args[i] == "--file":
			if i+1 < len(args) {
				path = args[i+1]
				i++
			}
		case strings.HasPrefix(args[i], "--file="):
			path = strings.TrimPrefix(args[i], "--file=")
		default:
			rest = append(rest, args[i])
		}
	}
	return path, rest
}

// contextDir finds the build context in a docker build invocation: the last bare
// argument. Only used to locate a default Dockerfile beside it.
func contextDir(args []string) string {
	directory := "."
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if strings.HasPrefix(argument, "-") {
			if takesValue(argument) && i+1 < len(args) {
				i++
			}
			continue
		}
		directory = argument
	}
	if strings.Contains(directory, "://") {
		// A URL context has no local Dockerfile to read; let Docker report that.
		return "."
	}
	return directory
}

// takesValue reports whether a docker build flag consumes the next argument. Only the
// separated form matters — `--tag=x` carries its own value.
func takesValue(flag string) bool {
	if strings.Contains(flag, "=") {
		return false
	}
	switch flag {
	case "-t", "--tag", "--build-arg", "--secret", "--platform", "--target",
		"--cache-from", "--cache-to", "--output", "-o", "--label", "--network",
		"--add-host", "--ssh", "--allow", "--builder", "--iidfile", "--progress",
		"--metadata-file", "--attest", "--annotation", "--build-context":
		return true
	}
	return false
}

// selectionFlags are the Compose flags that change what `config` renders. Passing the
// build flags too would make `config` reject them, and omitting these would render a
// different project than the one being built.
func selectionFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		argument := args[i]
		switch {
		case argument == "-f" || argument == "--file" ||
			argument == "-p" || argument == "--project-name" ||
			argument == "--profile" || argument == "--env-file" ||
			argument == "--project-directory":
			if i+1 < len(args) {
				out = append(out, argument, args[i+1])
				i++
			}
		case strings.HasPrefix(argument, "--file=") ||
			strings.HasPrefix(argument, "--project-name=") ||
			strings.HasPrefix(argument, "--profile=") ||
			strings.HasPrefix(argument, "--env-file=") ||
			strings.HasPrefix(argument, "--project-directory="):
			out = append(out, argument)
		}
	}
	return out
}

func authorityOf(raw string) string {
	trimmed := strings.TrimSuffix(raw, "/")
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(trimmed, scheme) {
			return strings.TrimPrefix(trimmed, scheme)
		}
	}
	return trimmed
}
