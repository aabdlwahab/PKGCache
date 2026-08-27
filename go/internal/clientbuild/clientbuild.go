// Package clientbuild runs `docker build` and `docker compose` against a Dockerfile
// the developer never has to edit.
//
// The settings a build needs are already sitting in the environment of the shell
// pkgreg-client opened. This takes them from there, rewrites the build in memory, and
// hands the result to Docker — so the file in the repository stays the file someone
// would have written with no cache at all, and still works on a machine that has never
// heard of one.
//
// Nothing is written into the project, or anywhere else. The rewritten Dockerfile
// reaches Docker on standard input, so there is no generated file for a `COPY . .` to
// sweep into the image — and none for a Docker client that cannot see this process's
// filesystem to fail to open. See Build.
package clientbuild

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/dockerfile"
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
	// HostAddress is CacheAddress's pkgcache equivalent: reach a cache on this machine
	// through host.docker.internal rather than through loopback. Same reason — the
	// daemon does not share this terminal's network namespace — with no certificate
	// involved, because a local cache serves plain HTTP.
	HostAddress bool
	// HostGatewayName is the name a build resolves the host by. Overridable only so a
	// test can assert what is generated.
	HostGatewayName string
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
	// stdin is what docker reads on its standard input: the rewritten Dockerfile for a
	// build, the rendered configuration for compose, or nil for this terminal's own.
	Runner func(ctx context.Context, name string, args []string, stdin io.Reader) error

	// Indexes maps an upstream package index origin to the cache's name for it, so a
	// Dockerfile naming one directly is served from here. DiscoverIndexes asks the cache.
	Indexes map[string]string

	// LocalImage reports whether a base image already exists on this machine, so a
	// locally built base is not rewritten into a registry reference for something that
	// was never published. Nil means the check is skipped; DefaultLocalImage asks Docker.
	LocalImage func(ref string) bool
}

// FromEnvironment fills in what a pkgreg shell already exported.
func FromEnvironment(o Options) Options {
	set := func(target *string, names ...string) {
		for _, name := range names {
			if *target != "" {
				return
			}
			*target = strings.TrimSpace(os.Getenv(name))
		}
	}
	// pkgcache's namespace is consulted first, so that running `pkgcache build` inside
	// a pkgreg-client shell builds against the local cache the command names rather
	// than against a server whose variables happen to still be set.
	set(&o.Bridge, "PKGCACHE_BRIDGE_URL", "PKGREG_BRIDGE_URL")
	set(&o.Registry, "PKGCACHE_DOCKER_REGISTRY", "PKGREG_DOCKER_REGISTRY")
	set(&o.Project, "PKGCACHE_PROJECT", "PKGREG_PROJECT")
	set(&o.AptProxy, "PKGCACHE_APT_PROXY", "PKGREG_APT_PROXY")
	set(&o.CAFile, "PKGREG_CA_FILE")
	set(&o.Server, "PKGREG_SERVER")
	if o.Project == "" {
		o.Project = "global"
	}
	if len(o.GitHosts) == 0 {
		o.GitHosts = []string{"github.com"}
	}
	if o.HostGatewayName == "" {
		o.HostGatewayName = DefaultHostGateway
	}
	return o
}

// DiscoverIndexes asks a running cache which package indexes it serves, as origin URL to
// index name.
//
// From the cache rather than from a table here, because an operator can add one — a
// pytorch build for a CUDA version this project has never heard of, a private wheelhouse
// — and a build should pick that up without a new release of this program.
//
// Best effort: a cache that cannot be asked is not a reason to fail a build, it only
// means the directly named indexes go upstream as they always did.
func DiscoverIndexes(ctx context.Context, base, project string) map[string]string {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(base, "/")+"/api/v1/projects/"+project+"/upstreams", nil)
	if err != nil {
		return nil
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Upstreams []struct {
			Eco     string `json:"eco"`
			Name    string `json:"name"`
			URL     string `json:"url"`
			Enabled bool   `json:"enabled"`
		} `json:"upstreams"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return nil
	}
	out := make(map[string]string)
	for _, upstream := range body.Upstreams {
		// pypi only: this exists for index URLs, and an npm registry or an OCI origin
		// written into a Dockerfile is a different rewrite with a different path shape.
		if upstream.Eco != "pypi" || !upstream.Enabled || upstream.URL == "" {
			continue
		}
		out[upstream.URL] = upstream.Name
	}
	return out
}

// DefaultLocalImage asks the container command whether a reference is already an image
// here, for Options.LocalImage.
//
// `image inspect` and not `images`: it takes the reference as written — tag, digest,
// registry prefix and all — and answers with an exit status, which is the whole question.
// Output is discarded; a missing image is not an error worth printing.
func DefaultLocalImage(docker string) func(string) bool {
	if docker == "" {
		docker = "docker"
	}
	return func(ref string) bool {
		// #nosec G204 -- the reference comes from a Dockerfile the caller is building.
		cmd := exec.Command(docker, "image", "inspect", ref)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		return cmd.Run() == nil
	}
}

// DefaultHostGateway is the name a build resolves this machine by. Docker Desktop
// provides it; on native Linux `--add-host` supplies it, which Build adds.
const DefaultHostGateway = "host.docker.internal"

// rewriteOptions turns the session into the rewriter's view of it.
func (o Options) rewriteOptions() (dockerfile.Options, error) {
	options := dockerfile.Options{
		Project: o.Project, AptProxy: o.AptProxy,
		GitHosts: o.GitHosts, SkipFrom: o.SkipFrom,
		LocalImage: o.LocalImage, Indexes: o.Indexes,
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
	if o.HostAddress {
		if o.Bridge == "" {
			return options, errors.New(
				"no session found: run this inside `pkgcache shell`, or through\n" +
					"`pkgcache build`, which starts the cache for you")
		}
		gateway := o.HostGatewayName
		if gateway == "" {
			gateway = DefaultHostGateway
		}
		options.Mode = dockerfile.HostGateway
		options.Base = viaHost(strings.TrimRight(o.Bridge, "/"), gateway)
		options.Registry = authorityOf(options.Base)
		if o.AptProxy != "" {
			options.AptProxy = viaHost(strings.TrimRight(o.AptProxy, "/"), gateway)
		}
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

// viaHost swaps a loopback authority for the name a build container resolves this
// machine by, keeping the scheme, the port and everything after it.
func viaHost(rawURL, gateway string) string {
	scheme := ""
	rest := rawURL
	for _, candidate := range []string{"https://", "http://"} {
		if strings.HasPrefix(rest, candidate) {
			scheme, rest = candidate, strings.TrimPrefix(rest, candidate)
			break
		}
	}
	authority, path, hadPath := strings.Cut(rest, "/")
	port := ""
	if _, p, err := net.SplitHostPort(authority); err == nil {
		port = p
	}
	rebuilt := gateway
	if port != "" {
		rebuilt = net.JoinHostPort(gateway, port)
	}
	if hadPath {
		rebuilt += "/" + path
	}
	return scheme + rebuilt
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

	report(o.Stderr, result.Changes)
	// `-f -` rather than a generated file, because not every Docker client can read
	// this process's filesystem: the snap has a private /tmp, and a rootless daemon
	// has its own mount namespace. Such a client sends an empty dockerfile and the
	// build dies on "failed to read dockerfile ... no such file or directory".
	// Standard input is the one channel every client shares with whoever invoked it,
	// and it keeps the generated file out of the build context for free.
	command := []string{"build", "-f", "-"}
	switch rewrite.Mode {
	case dockerfile.Bridge:
		// The RUN steps talk to a loopback address, which only exists in the build
		// container if it shares this machine's network namespace.
		command = append(command, "--network=host")
	case dockerfile.HostGateway:
		// Docker Desktop resolves host.docker.internal already; native Linux does not,
		// and this is the documented way to ask for it. Harmless where it is redundant.
		command = append(command,
			"--add-host="+o.HostGatewayName+":host-gateway")
	case dockerfile.CacheAddress:
	}
	if result.NeedsSecret {
		command = append(command,
			"--secret", "id="+dockerfile.SecretID+",src="+o.CAFile)
	}
	return o.Runner(ctx, "docker", append(command, rest...), bytes.NewReader(result.Content))
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
	needsSecret := false
	for _, build := range result.Dockerfiles {
		needsSecret = needsSecret || build.NeedsSecret
	}
	if o.Print {
		_, err := o.Stdout.Write(document)
		return err
	}
	report(o.Stderr, result.Changes)
	if needsSecret {
		_, _ = fmt.Fprintf(o.Stderr,
			"pkgreg: this build needs the cache CA; add to each service's build:\n"+
				"  secrets: [%s]\nwith a top-level secrets entry pointing at %s\n",
			dockerfile.SecretID, o.CAFile)
	}
	// On stdin rather than a temporary file on purpose: a rendered configuration
	// carries interpolated environment values, which can include credentials, and
	// none of this needs to touch the disk. The rewritten Dockerfiles ride along
	// inside it as `dockerfile_inline`, for that reason and for Build's.
	return o.Runner(ctx, "docker",
		append([]string{"compose", "-f", "-"}, args...), bytes.NewReader(document))
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

func withDefaults(o Options) Options {
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Runner == nil {
		o.Runner = func(
			ctx context.Context, name string, args []string, stdin io.Reader,
		) error {
			command := exec.CommandContext(ctx, name, args...)
			if stdin == nil {
				stdin = os.Stdin
			}
			command.Stdin, command.Stdout, command.Stderr = stdin, o.Stdout, o.Stderr
			return command.Run()
		}
	}
	return o
}

// report prints every substitution. A tool that silently changes what gets built is a
// tool people stop trusting the first time a build surprises them.
func report(w io.Writer, changes []dockerfile.Change) {
	for _, change := range changes {
		_, _ = fmt.Fprintf(w, "pkgreg: %s -> %s\n", change.From, change.To)
	}
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
