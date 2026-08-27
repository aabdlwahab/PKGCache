package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/clientbridge"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
	"github.com/aabdlwahab/PKGCache/internal/session"
)

// defaultGitHosts is what `git clone` is redirected through the cache for.
//
// github.com alone by default, because a host that is not actually mirrored would
// simply fail, and a redirect a user did not ask for is worse than no redirect. More
// with -git-host.
var defaultGitHosts = []string{"github.com"}

// sessionFlags are the options run, shell and env share.
type sessionFlags struct {
	project  string
	gitHosts string
	noGit    bool
}

func bindSessionFlags(fs *flag.FlagSet) *sessionFlags {
	s := &sessionFlags{}
	// Empty rather than "global": the default is the current project, and that cannot
	// be read until -data-dir has been parsed. Resolved in startSession.
	fs.StringVar(&s.project, "project", "",
		"project to scope URLs to (default: the current one, from pkgcache project use)")
	fs.StringVar(&s.gitHosts, "git-host", strings.Join(defaultGitHosts, ","),
		"comma-separated hosts whose clones are served from the cache")
	fs.BoolVar(&s.noGit, "no-git", false, "leave git configuration alone")
	return s
}

// sessionOptions turns a running daemon plus flags into the environment description.
func sessionOptions(state local.State, flags *sessionFlags) session.Options {
	hosts := splitList(flags.gitHosts)
	if flags.noGit {
		hosts = nil
	}
	return session.Options{
		Prefix:  session.PkgcachePrefix,
		Kind:    "local",
		BaseURL: state.BaseURL(),
		Project: flags.project,
		// One port serves the forward proxy too, so this is the same address.
		AptProxy:       state.BaseURL(),
		DockerRegistry: state.Addr,
		GitHosts:       hosts,
	}
}

func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// startSession resolves configuration, ensures a daemon and builds the environment.
func startSession(
	ctx context.Context, name string, args []string, usage string,
) (loaded *config.Snapshot, state local.State, command, env []string, err error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	flags := bindSessionFlags(fs)
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), usage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, local.State{}, nil, nil, err
	}
	snap, err := config.LoadLocal(collect())
	if err != nil {
		return nil, local.State{}, nil, nil, err
	}
	if flags.project == "" {
		flags.project = local.CurrentProject(snap.DataDir)
	}
	state, stop, err := reachCache(ctx, snap)
	if err != nil {
		return nil, local.State{}, nil, nil, err
	}
	if stop != nil {
		// A bridge lives exactly as long as the command it serves. Registered here so
		// every caller gets it without having to remember.
		bridgeStops = append(bridgeStops, stop)
	}
	environment := session.Environment(os.Environ(), sessionOptions(state, flags))
	return snap, state, environment, fs.Args(), nil
}

// bridgeStops holds the shutdown for a bridge started by this process, closed once the
// command it exists for has finished.
var bridgeStops []func()

func closeBridges() {
	for _, stop := range bridgeStops {
		stop()
	}
	bridgeStops = nil
}

// reachCache returns something for tools to point at: a local daemon, or — when this
// machine caches nothing — a verified loopback bridge to the team cache.
//
// The second is what pkgreg-client has always done, and it is a mode of this program
// rather than a second program. Nothing here opens a store, creates a database or
// writes to the cache directory, which is the promise -no-cache makes.
func reachCache(
	ctx context.Context, snap *config.Snapshot,
) (local.State, func(), error) {
	set, err := local.ReadTeams(snap.DataDir)
	if err != nil {
		return local.State{}, nil, err
	}
	// Bridge-only is a property of the machine, not of a project: it promises that no
	// store is opened at all, which cannot hold for one project and not another.
	team, bridged := set.Bridged()
	if !bridged {
		state, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, Notes: os.Stderr})
		return state, nil, err
	}

	caPEM := []byte(team.CAPEM)
	if len(caPEM) == 0 {
		return local.State{}, nil, errors.New(
			"pkgcache: the team cache's CA is missing; run `pkgcache setup` again")
	}
	running, err := clientbridge.Start(ctx, clientbridge.SessionOptions{
		Server: team.Server, Project: team.Project, CAPEM: caPEM,
		CAFingerprint: team.Fingerprint,
	})
	if err != nil {
		return local.State{}, nil, err
	}
	return local.State{Addr: running.Addr}, func() { _ = running.Close() }, nil //nolint:contextcheck // io.Closer takes no context, by definition
}

func runRun(ctx context.Context, args []string) error {
	// Everything after `--` is the command, and must not be parsed as ours: `pkgcache
	// run -- npm ci --no-audit` has to reach npm with its flags intact.
	ours, theirs := splitAtDoubleDash(args)
	snap, _, environment, rest, err := startSession(ctx, "run", ours,
		`pkgcache run — run one command with its package tools pointed at the cache

usage: pkgcache run [flags] -- <command> [arguments]

Starts the cache if it is not running, then runs the command with pip, uv, npm, yarn,
pnpm and git configured to use it. Nothing is installed and nothing outlives the
command.

flags:
`)
	if err != nil {
		return err
	}
	command := slices.Concat(rest, theirs)
	if len(command) == 0 {
		return errors.New("run: no command given; use `pkgcache run -- npm ci`")
	}

	// #nosec G204 -- running the command the user typed is the entire purpose.
	child := exec.CommandContext(ctx, command[0], command[1:]...)
	child.Env = environment
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The child gets the terminal's signals directly from the terminal; killing it
	// here as well would race the shutdown it is already performing.
	child.Cancel = func() error { return nil }

	err = child.Run()
	closeBridges()
	// A full cache is reported whatever happened, and the child's own failure still
	// wins the exit status: `npm ci` exiting 1 must surface as 1, never masked by
	// pkgcache's 75, because the build failing is the more urgent of the two facts.
	full := reportFull(snap.DataDir)
	if err == nil {
		if full != nil {
			return full
		}
		return nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// The child has already said whatever it wanted to say. Adding "pkgcache: exit
		// status 1" on top of a compiler's error output helps nobody.
		return &exitError{code: exit.ExitCode()}
	}
	return fmt.Errorf("run %s: %w", command[0], err)
}

func runShell(ctx context.Context, args []string) error {
	snap, state, environment, rest, err := startSession(ctx, "shell", args,
		`pkgcache shell — a shell whose package tools use the cache

usage: pkgcache shell [flags]

Starts the cache if it is not running, then opens your shell with pip, uv, npm and git
pointed at it. Type exit to return to your previous environment; nothing is installed
and nothing is left behind.

flags:
`)
	if err != nil {
		return err
	}
	preferred := ""
	if len(rest) > 0 {
		preferred = rest[0]
	}
	program, shellArgs, err := session.Shell(preferred, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, `pkgcache: session ready
  cache:   %s
  project: %s

Type exit to return to your previous environment.

`, state.BaseURL(), valueOf(environment, session.PkgcachePrefix+"PROJECT"))

	// #nosec G204 -- the shell is the user's own, from -shell or $SHELL.
	child := exec.CommandContext(ctx, program, shellArgs...)
	child.Env = environment
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.Cancel = func() error { return nil }

	err = child.Run()
	closeBridges()
	full := reportFull(snap.DataDir)
	if err == nil {
		fmt.Fprintln(os.Stderr, "pkgcache: session ended; previous settings restored")
		if full != nil {
			return full
		}
		return nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return &exitError{code: exit.ExitCode()}
	}
	return fmt.Errorf("shell: %w", err)
}

func runEnv(ctx context.Context, args []string) error {
	_, _, environment, _, err := startSession(ctx, "env", args,
		`pkgcache env — print the settings that point tools at the cache

usage: eval "$(pkgcache env)"

Starts the cache if it is not running and prints shell exports for the settings
`+"`pkgcache run`"+` would apply. Use it when a shell has to keep them, rather than one
command or one session.

flags:
`)
	if err != nil {
		return err
	}
	// env prints settings for a shell that outlives this process, so a bridge that dies
	// with it would be worse than useless. It is refused rather than silently printed.
	if len(bridgeStops) > 0 {
		closeBridges()
		return errors.New(
			"env cannot be used with -no-cache: the bridge it names lives only as long as\n" +
				"  this command. Use `pkgcache run` or `pkgcache shell`, which keep it open")
	}
	for _, entry := range changedBy(os.Environ(), environment) {
		name, value, _ := strings.Cut(entry, "=")
		fmt.Printf("export %s=%s\n", name, shellQuote(value))
	}
	return nil
}

// changedBy returns the entries a session added or altered, so `pkgcache env` prints
// its own settings rather than a copy of the caller's entire environment.
func changedBy(before, after []string) []string {
	existing := make(map[string]string, len(before))
	for _, entry := range before {
		key, value, _ := strings.Cut(entry, "=")
		existing[key] = value
	}
	var changed []string
	for _, entry := range after {
		key, value, _ := strings.Cut(entry, "=")
		if old, had := existing[key]; !had || old != value {
			changed = append(changed, entry)
		}
	}
	sort.Strings(changed)
	return changed
}

func valueOf(environment []string, name string) string {
	for _, entry := range environment {
		if key, value, _ := strings.Cut(entry, "="); key == name {
			return value
		}
	}
	return ""
}

// shellQuote wraps a value so that eval sees exactly the bytes we meant. Every URL here
// is machine-generated, but a project name reaches this from a flag.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// splitAtDoubleDash divides our flags from the command to run. Go's flag package stops
// at the first non-flag argument, which would hand `npm` to us as a positional and then
// reject `--no-audit` as an unknown flag.
func splitAtDoubleDash(args []string) (ours, theirs []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}
