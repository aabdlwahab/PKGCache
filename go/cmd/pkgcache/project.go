package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// `pkgcache project` — the same projects the server has, without the accounts.
//
// A project here is an upstream chain and an accounting boundary, and nothing else; see
// internal/local/project.go for why that is the whole of it. This file is the command
// line over it, and it exists because everything else already did: the control database
// is opened in local mode, the API allows everything when no accounts exist, and the
// embedded console can already create a project. Only the terminal could not.
//
// Two design choices worth stating. The verbs go through the daemon's API rather than
// opening the store, because the store has one writer and stopping a running cache to
// create a project would interrupt whatever is downloading through it. And `use` stores
// a default rather than a mode: -project keeps working everywhere, so no script depends
// on what somebody selected in a shell an hour ago.

func runProject(ctx context.Context, args []string) error {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
		args = args[1:]
	}
	switch verb {
	case "", "ls", "list":
		return projectList(ctx, args)
	case "create", "add", "new":
		return projectCreate(ctx, args)
	case "rm", "remove", "delete":
		return projectRemove(ctx, args)
	case "use", "switch":
		return projectUse(ctx, args)
	case "-h", "--help", "help":
		projectUsage(os.Stderr)
		return nil
	default:
		projectUsage(os.Stderr)
		return fmt.Errorf("project: unknown subcommand %q", verb)
	}
}

func projectUsage(out *os.File) {
	fmt.Fprint(out, `pkgcache project — the projects this cache serves

usage:
  pkgcache project ls                 list them, marking the current one
  pkgcache project create <name>      register a project
  pkgcache project rm <name>          unregister a project
  pkgcache project use <name>         work in it, until told otherwise

A project is a separate upstream chain and a separate accounting boundary: work can go
through the company's cache while a side project goes straight to the public registry,
and each one's size is a question the cache can answer. It is not an isolation
boundary — content is shared by digest, so two projects needing the same wheel store it
once.

Every command that takes -project still does. `+"`use`"+` only chooses what they default to,
and `+"`"+config.LocalEnvPrefix+`PROJECT`+"`"+` overrides it for one command without changing it.

`)
}

// projectFlags parses the shared flags for one project subcommand and returns the one
// positional argument it may take.
//
// Extra positionals are refused rather than ignored, because Go's flag package stops at
// the first non-flag: "project create work -data-dir /x" would otherwise silently use
// the wrong cache. Flags come first.
func projectFlags(name string, args []string, wantName bool) (*config.Snapshot, string, error) {
	fs := flag.NewFlagSet("project "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	fs.Usage = func() {
		projectUsage(os.Stderr)
		fmt.Fprint(fs.Output(), "flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, "", err
	}
	rest := fs.Args()
	switch {
	case wantName && len(rest) == 0:
		return nil, "", fmt.Errorf("project %s: which project? `pkgcache project %s <name>`",
			name, name)
	case wantName && len(rest) > 1, !wantName && len(rest) > 0:
		return nil, "", fmt.Errorf(
			"project %s: unexpected argument %q; flags come before the name", name, rest[len(rest)-1])
	}
	snap, err := config.LoadLocal(collect())
	if err != nil {
		return nil, "", err
	}
	projectName := ""
	if wantName {
		projectName = rest[0]
	}
	return snap, projectName, nil
}

// reachRegistry returns a daemon to ask about projects, starting one if necessary.
//
// A cache that stores nothing has no registry of its own: with -no-cache this machine is
// a verified bridge to the team's cache, and the projects that matter are the team's. It
// is refused here rather than answered with an empty list, which would be a true and
// thoroughly misleading answer.
func reachRegistry(ctx context.Context, snap *config.Snapshot) (local.State, error) {
	set, err := local.ReadTeams(snap.DataDir)
	if err != nil {
		return local.State{}, err
	}
	if team, bridged := set.Bridged(); bridged {
		return local.State{}, fmt.Errorf(
			"this cache stores nothing (-no-cache), so it has no projects of its own.\n"+
				"  The project used on the team cache is %q; change it with\n"+
				"  `pkgcache setup -server %s -project <name>`",
			team.Project, team.Server)
	}
	return local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, Notes: os.Stderr})
}

func projectList(ctx context.Context, args []string) error {
	snap, _, err := projectFlags("ls", args, false)
	if err != nil {
		return err
	}
	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	projects, err := local.ListProjects(ctx, state)
	if err != nil {
		return err
	}
	// Global first, then by name: a list whose order depends on creation time cannot be
	// compared between two machines, and comparing them is most of why somebody looks.
	sort.SliceStable(projects, func(i, j int) bool {
		if (projects[i].Name == config.GlobalProject) != (projects[j].Name == config.GlobalProject) {
			return projects[i].Name == config.GlobalProject
		}
		return projects[i].Name < projects[j].Name
	})
	// Where each project's misses go is the reason to have more than one, so it is
	// listed rather than left to `pkgcache setup` to recite.
	set, err := local.ReadTeams(snap.DataDir)
	if err != nil {
		return err
	}
	current := local.CurrentProject(snap.DataDir)
	// Column widths from the content, because a team cache's hostname is as long as
	// somebody's internal DNS makes it and a fixed width would tear the table.
	nameWidth, viaWidth := len(config.GlobalProject), 6
	for _, project := range projects {
		nameWidth = max(nameWidth, len(project.Name))
		viaWidth = max(viaWidth, width(via(set, project.Name)))
	}
	for _, project := range projects {
		marker := " "
		if project.Name == current {
			marker = "*"
		}
		tier := via(set, project.Name)
		line := fmt.Sprintf("%s %-*s  %s%s  %s", marker, nameWidth, project.Name,
			tier, strings.Repeat(" ", viaWidth-width(tier)), created(project.CreatedAt))
		if project.Offline {
			line += "   offline"
		}
		fmt.Println(strings.TrimRight(line, " "))
	}
	if inheriting(set, projects) {
		fmt.Printf("\n(↑ inherited from %s; pkgcache setup -project <name> gives one its own)\n",
			config.GlobalProject)
	}
	if !containsProject(projects, current) {
		fmt.Printf("\npkgcache: the current project %q does not exist here.\n"+
			"  `pkgcache project create %s`, or `pkgcache project use %s`\n",
			current, current, config.GlobalProject)
	}
	return nil
}

// width counts runes, not bytes: the inherited marker is one character that encodes as
// three, and %-*s pads by bytes.
func width(value string) int { return len([]rune(value)) }

// inheriting reports whether any listed project is following the global project's
// chain, which is the only reason to print the legend for it.
func inheriting(set local.TeamSet, projects []local.Project) bool {
	for _, project := range projects {
		if project.Name == config.GlobalProject {
			continue
		}
		if _, has := set.For(project.Name); !has {
			continue
		}
		if _, own := set.Own(project.Name); !own {
			return true
		}
	}
	return false
}

// via names the tier a project's misses go to next, short enough for a column.
func via(set local.TeamSet, project string) string {
	team, has := set.For(project)
	if !has {
		return "direct"
	}
	host := strings.TrimPrefix(strings.TrimPrefix(team.Server, "https://"), "http://")
	if _, own := set.Own(project); !own {
		// Marked, because an inherited chain is the one somebody is surprised by: they
		// configured the global project and this one followed. Not with a star, which
		// the first column already spends on the current project.
		return host + " ↑"
	}
	return host
}

// created renders a project's age. The global project predates the record — it exists
// from the first time the store is opened — so it has no timestamp to show.
func created(at time.Time) string {
	if at.IsZero() {
		return "since this cache existed"
	}
	return "created " + at.Local().Format("2006-01-02 15:04")
}

func containsProject(projects []local.Project, name string) bool {
	for _, project := range projects {
		if project.Name == name {
			return true
		}
	}
	return false
}

func projectCreate(ctx context.Context, args []string) error {
	snap, name, err := projectFlags("create", args, true)
	if err != nil {
		return err
	}
	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	if _, err := local.CreateProject(ctx, state, name); err != nil {
		return err
	}
	// The chain is materialised by the daemon as it creates the project, not here.
	// Upstream rows are per project, so a project created after `setup` inherits nothing
	// on its own and would resolve straight to the public registry — silently, because a
	// chain missing its first row is still a valid chain. That was once done in this
	// function, which meant it happened for this command and for nothing else: the same
	// project created from the widget or over the API got no chain at all. It belongs
	// where every caller passes through it.
	//
	// Read here only to say what happened.
	set, err := local.ReadTeams(snap.DataDir)
	if err != nil {
		return err
	}
	team, chained := set.For(name)
	fmt.Printf("pkgcache: created %s\n", name)
	if chained {
		fmt.Printf("  lookups go through %s (project %s), then %s\n",
			team.Server, team.Project, directly(team))
		fmt.Printf("  its own team:    pkgcache setup -project %s -server …\n", name)
	}
	fmt.Printf("  work in it:      pkgcache project use %s\n", name)
	fmt.Printf("  or once:         pkgcache run -project %s -- npm ci\n", name)
	return nil
}

func projectRemove(ctx context.Context, args []string) error {
	snap, name, err := projectFlags("rm", args, true)
	if err != nil {
		return err
	}
	// Refused here as well as in local.DeleteProject, because this is the message a
	// person reads and the one below is defence in depth for a caller that skipped it.
	if name == config.GlobalProject {
		return fmt.Errorf("%s is where everything is served from and cannot be removed",
			config.GlobalProject)
	}
	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	if err := local.DeleteProject(ctx, state, name); err != nil {
		return err
	}
	// Its team configuration goes with it. Leaving the entry behind would silently
	// reapply itself to a project of the same name created later, which is a surprise
	// rather than a convenience.
	set, err := local.ReadTeams(snap.DataDir)
	if err != nil {
		return err
	}
	if _, own := set.Own(name); own {
		set.Remove(name)
		if err := local.WriteTeams(snap.DataDir, set); err != nil {
			return err
		}
	}
	// The current project must never name something that is gone: the next `run` would
	// fail with a 404 from the router, which is correct and unhelpful.
	if local.CurrentProject(snap.DataDir) == name {
		local.ClearCurrentProject(snap.DataDir)
		fmt.Printf("pkgcache: removed %s; now working in %s\n", name, config.GlobalProject)
	} else {
		fmt.Printf("pkgcache: removed %s\n", name)
	}
	// Said every time, because "I deleted a project and the disk did not shrink" is the
	// predictable next question, and the answer is a design decision rather than a bug.
	fmt.Println("  its content is shared by digest and stays until nothing references it")
	return nil
}

func projectUse(ctx context.Context, args []string) error {
	snap, name, err := projectFlags("use", args, true)
	if err != nil {
		return err
	}
	// Validated against the registry, because the whole value of choosing a default is
	// catching the typo here rather than in a 404 from the next `npm ci`.
	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	projects, err := local.ListProjects(ctx, state)
	if err != nil {
		return err
	}
	if !containsProject(projects, name) {
		return fmt.Errorf("no such project here: %s\n  `pkgcache project create %s` first",
			name, name)
	}
	if err := local.SetCurrentProject(snap.DataDir, name); err != nil {
		return err
	}
	fmt.Printf("pkgcache: working in %s\n", name)
	if name == config.GlobalProject {
		fmt.Println("  which is the default, so nothing is stored to remember it")
	}
	return nil
}

// directly names the last tier, for a line that has to say where a miss ends up.
func directly(team local.Team) string {
	if team.Direct {
		return "the public registry"
	}
	return "nothing — this project never reaches a registry itself"
}
