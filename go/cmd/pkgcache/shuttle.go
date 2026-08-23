package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// The air-gap shuttle from the client: carry what this cache holds to a machine with no
// network, and take what somebody else carried.
//
// The engine needed nothing for this — the job runner, the staging directories and the
// control API are in every instance — so what is here is a command line over jobs the
// daemon already knows how to run, plus the two things a person on a laptop needs that a
// server operator does not:
//
//   - packs at a path they choose, because the pack is going onto a USB stick and not
//     into ~/.local/share/pkgcache/shuttle/out;
//   - `export` meaning "what my cache holds now", which is a checkpoint and then a pack,
//     rather than a checkpoint they have to know to take first.
//
// An import needs no counterpart: it applies the pack itself, and it is refused unless
// the pack's base matches this project's checkpoint — so it can be pointed at a project
// that already holds work without risking it.

// shuttleCommon parses the flags every shuttle verb shares and resolves the project.
func shuttleCommon(
	name string, args []string, usage string, bind func(*flag.FlagSet),
) (*config.Snapshot, string, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	project := fs.String("project", "",
		"project to act on (default: the current one, from pkgcache project use)")
	if bind != nil {
		bind(fs)
	}
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, "", nil, err
	}
	snap, err := config.LoadLocal(collect())
	if err != nil {
		return nil, "", nil, err
	}
	scope := *project
	if scope == "" {
		scope = local.CurrentProject(snap.DataDir)
	}
	return snap, scope, fs.Args(), nil
}

func runCheckpoint(ctx context.Context, args []string) error {
	var message string
	snap, project, _, err := shuttleCommon("checkpoint", args,
		`pkgcache checkpoint — record what this project holds right now

usage: pkgcache checkpoint -m "before the trip"

A checkpoint is a manifest: the digests this project currently resolves to, named so it
can be exported, carried, and rolled back to. It copies no bytes and takes no space
worth counting.

flags:
`, func(fs *flag.FlagSet) {
			fs.StringVar(&message, "m", "", "what this checkpoint is for")
			fs.StringVar(&message, "message", "", "alias for -m")
		})
	if err != nil {
		return err
	}
	if strings.TrimSpace(message) == "" {
		return errors.New("checkpoint: -m is required; a checkpoint nobody labelled is one " +
			"nobody can choose later")
	}
	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	id, err := checkpoint(ctx, state, project, message)
	if err != nil {
		return err
	}
	fmt.Printf("pkgcache: checkpoint %s\n", short(id))
	return nil
}

// checkpoint records the project's current content and returns the new head.
func checkpoint(ctx context.Context, state local.State, project, message string) (string, error) {
	job, err := local.SubmitJob(ctx, state, project, "snapshots",
		map[string]any{"message": message})
	if err != nil {
		return "", err
	}
	if _, err := local.FollowJob(ctx, state, job.ID, os.Stderr); err != nil {
		return "", err
	}
	head, _, err := local.Snapshots(ctx, state, project)
	return head, err
}

func runExport(ctx context.Context, args []string) error {
	var (
		file   string
		since  string
		target string
	)
	snap, project, _, err := shuttleCommon("export", args,
		`pkgcache export — put what this project holds into a pack

usage:
  pkgcache export -file /media/usb/work.tar
  pkgcache export -file work.tar -since 7b69cf29      # only what is new since then

Checkpoints the project first, so the pack is what the cache holds now rather than what
it held the last time somebody thought about it. -target exports an existing checkpoint
instead and takes no new one.

-since builds a delta, which is what a second trip should carry — but only the machine
receiving it can say which checkpoint it is on, so there is no default. A pack whose base
the far side does not have is refused there, not merely slower.

flags:
`, func(fs *flag.FlagSet) {
			fs.StringVar(&file, "file", "",
				"where to write the pack; a path, or a .tar name to leave it in shuttle/out")
			fs.StringVar(&since, "since", "", "base checkpoint, for a delta")
			fs.StringVar(&target, "target", "", "export this checkpoint instead of a new one")
		})
	if err != nil {
		return err
	}
	// Validated before anything is submitted. An export is minutes and gigabytes; a path
	// that was never going to work should cost neither.
	plan, err := local.PlanExport(snap.DataDir, file)
	if err != nil {
		return err
	}
	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	// Both are checkpoint prefixes, because `snapshots` prints twelve characters and
	// nobody retypes sixty-four. The engine compares ids literally, so an unresolved
	// prefix reaches it as "base … is not an ancestor of target …" — accurate, and
	// impossible to act on.
	for _, ref := range []*string{&since, &target} {
		if *ref == "" {
			continue
		}
		resolved, err := resolveSnapshot(ctx, state, project, *ref)
		if err != nil {
			return err
		}
		*ref = resolved
	}
	if target == "" {
		// "Export this project" means what is in it now, so the checkpoint is taken here
		// rather than demanded of the user. It also gives whoever receives this pack the
		// base they need to ask for a delta next time.
		id, err := checkpoint(ctx, state, project,
			"export "+time.Now().Format("2006-01-02 15:04"))
		if err != nil {
			return err
		}
		target = id
	}
	job, err := local.SubmitJob(ctx, state, project, "export", map[string]any{
		"base": since, "target": target, "file": plan.Name,
	})
	if err != nil {
		return err
	}
	if _, err := local.FollowJob(ctx, state, job.ID, os.Stderr); err != nil {
		return err
	}
	if plan.Name == "" {
		// The job named the pack after the checkpoint, and has already said where it is.
		return nil
	}
	path, err := plan.FinishExport(snap.DataDir)
	if err != nil {
		return err
	}
	fmt.Printf("pkgcache: %s\n", path)
	if info, statErr := os.Stat(path); statErr == nil {
		fmt.Printf("  %s, checkpoint %s\n", local.FormatSize(info.Size()), short(target))
	}
	fmt.Printf("  import it with: pkgcache import -project %s -file %s\n", project, path)
	return nil
}

func runImport(ctx context.Context, args []string) error {
	var file string
	snap, project, _, err := shuttleCommon("import", args,
		`pkgcache import — take a pack somebody carried here

usage: pkgcache import -file /media/usb/work.tar

The pack is applied as it is read: there is no second command, and when the project it
names does not exist here it is created. Nothing is written unless the pack's starting
point matches this project's checkpoint, so pointing it at a project that already holds
work cannot lose that work — it is refused instead.

flags:
`, func(fs *flag.FlagSet) {
			fs.StringVar(&file, "file", "",
				"the pack; a path, or a .tar name already in shuttle/in")
		})
	if err != nil {
		return err
	}
	source, err := local.StageImport(snap.DataDir, file)
	if err != nil {
		return err
	}
	state, err := reachRegistry(ctx, snap)
	if err != nil {
		// Nothing was submitted, so the staged copy is this function's to remove.
		source.Cleanup(snap.DataDir)
		return err
	}
	defer source.Cleanup(snap.DataDir)

	before, _, err := local.Snapshots(ctx, state, project)
	if err != nil {
		return err
	}
	job, err := local.SubmitJob(ctx, state, project, "import",
		map[string]any{"file": source.Name})
	if err != nil {
		return err
	}
	if _, err := local.FollowJob(ctx, state, job.ID, os.Stderr); err != nil {
		return explainImport(err, project, before)
	}
	head, _, err := local.Snapshots(ctx, state, project)
	if err != nil {
		return err
	}
	fmt.Printf("pkgcache: %s is now at checkpoint %s\n", project, short(head))
	if full := reportFull(snap.DataDir); full != nil {
		return full
	}
	return nil
}

// explainImport rewrites the one refusal a person will actually hit.
//
// The engine says: non-fast-forward import: local HEAD is "7b69…", pack requires base "".
// That is precise and useless — it names two hashes and no way forward. There are exactly
// two, and which one is right depends on whether this project's content is worth keeping,
// which only the person can answer.
//
// The engine's own line is deliberately not wrapped into this. The job log has already
// put it on stderr, and repeating it doubles the noise on the one error path somebody is
// most likely to meet.
func explainImport(err error, project, head string) error {
	if err == nil || !strings.Contains(err.Error(), "non-fast-forward") {
		return err
	}
	at := "no checkpoint"
	if head != "" {
		at = "checkpoint " + short(head)
	}
	return fmt.Errorf(`this pack does not continue from where %s is (%s), so nothing was changed.

  The pack was built from a different starting point. Either:
    - ask whoever made it to export a delta from here:
        pkgcache export -since %s
      run on their machine, against the checkpoint they gave you; or
    - import it into a project of its own, leaving this one alone:
        pkgcache project create %s-incoming
        pkgcache import -project %s-incoming -file …

  Nothing has been written, and %s is unchanged.`,
		project, at, short(head), project, project, project)
}

func runSnapshots(ctx context.Context, args []string) error {
	snap, project, _, err := shuttleCommon("snapshots", args,
		`pkgcache snapshots — the checkpoints this project has, and which one it is on

usage: pkgcache snapshots

The one marked * is where the project is. It is the fact both ends of a transfer need:
a pack is accepted only where its starting point is the receiver's checkpoint.

flags:
`, nil)
	if err != nil {
		return err
	}
	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	head, snapshots, err := local.Snapshots(ctx, state, project)
	if err != nil {
		return err
	}
	if len(snapshots) == 0 {
		fmt.Printf("pkgcache: %s has no checkpoints yet\n", project)
		fmt.Println("  pkgcache checkpoint -m \"what this is for\"")
		return nil
	}
	for _, entry := range snapshots {
		marker := " "
		if entry.ID == head {
			marker = "*"
		}
		fmt.Printf("%s %s  %-19s  %5d entries  %9s  %s\n", marker, short(entry.ID),
			entry.CreatedAt[:min(19, len(entry.CreatedAt))], entry.Entries,
			local.FormatSize(entry.Bytes), entry.Subject)
	}
	return nil
}

func runRollback(ctx context.Context, args []string) error {
	snap, project, rest, err := shuttleCommon("rollback", args,
		`pkgcache rollback — make a checkpoint this project's content again

usage: pkgcache rollback <checkpoint>

This DISCARDS what the project resolves to now and replaces it with the checkpoint's
content. The bytes survive — everything is stored by digest and shared — but anything
cached since that checkpoint stops being served until it is fetched again.

Nothing else in pkgcache does this. An import cannot: a pack that does not continue from
here is refused rather than applied.

flags:
`, nil)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("rollback: which checkpoint? `pkgcache snapshots` lists them")
	}
	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	// Resolved from a prefix, because nobody types 64 hex characters and `snapshots`
	// prints 12.
	id, err := resolveSnapshot(ctx, state, project, rest[0])
	if err != nil {
		return err
	}
	job, err := local.Rollback(ctx, state, project, id)
	if err != nil {
		return err
	}
	if _, err := local.FollowJob(ctx, state, job.ID, os.Stderr); err != nil {
		return err
	}
	fmt.Printf("pkgcache: %s is now at checkpoint %s\n", project, short(id))
	return nil
}

// resolveSnapshot expands a checkpoint prefix, and refuses an ambiguous one rather than
// picking: rolling back to the wrong checkpoint is the one mistake here that costs
// something.
func resolveSnapshot(
	ctx context.Context, state local.State, project, prefix string,
) (string, error) {
	_, snapshots, err := local.Snapshots(ctx, state, project)
	if err != nil {
		return "", err
	}
	var found []string
	for _, entry := range snapshots {
		if strings.HasPrefix(entry.ID, prefix) {
			found = append(found, entry.ID)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("no checkpoint here starts with %q; `pkgcache snapshots` lists them",
			prefix)
	default:
		return "", fmt.Errorf("%q matches %d checkpoints; give more of it", prefix, len(found))
	}
}

// short is the prefix people actually exchange.
func short(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
