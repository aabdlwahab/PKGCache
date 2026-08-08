package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/brightskies/pkgreg/internal/app"
	"github.com/brightskies/pkgreg/internal/config"
)

func runCheckpoint(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("checkpoint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	project := fs.String("project", config.GlobalProject, "project to checkpoint")
	message := fs.String("message", "", "checkpoint message")
	fs.StringVar(message, "m", "", "checkpoint message")
	asJSON := fs.Bool("json", false, "emit JSON progress")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*message) == "" {
		return errors.New("checkpoint: -message is required")
	}
	return runDirectJob(ctx, collect(), *project, "checkpoint",
		map[string]any{"message": *message}, *asJSON)
}

func runRollback(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	project := fs.String("project", config.GlobalProject, "project to restore")
	snapshotID := fs.String("snapshot", "", "checkpoint id")
	asJSON := fs.Bool("json", false, "emit JSON progress")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *snapshotID == "" && fs.NArg() == 1 {
		*snapshotID = fs.Arg(0)
	}
	if *snapshotID == "" {
		return errors.New("rollback: -snapshot or one checkpoint argument is required")
	}
	return runDirectJob(ctx, collect(), *project, "rollback",
		map[string]any{"snapshot": *snapshotID}, *asJSON)
}

func runExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	project := fs.String("project", config.GlobalProject, "project to export")
	base := fs.String("base", "", "base checkpoint for a delta")
	target := fs.String("target", "", "target checkpoint; defaults to HEAD")
	file := fs.String("file", "", "output .tar basename under shuttle/out")
	asJSON := fs.Bool("json", false, "emit JSON progress")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runDirectJob(ctx, collect(), *project, "export", map[string]any{
		"base": *base, "target": *target, "file": *file,
	}, *asJSON)
}

func runImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	project := fs.String("project", config.GlobalProject, "project carried by the pack")
	file := fs.String("file", "", "input .tar basename under shuttle/in")
	asJSON := fs.Bool("json", false, "emit JSON progress")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runDirectJob(ctx, collect(), *project, "import",
		map[string]any{"file": *file}, *asJSON)
}

func runLockwarm(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("lockwarm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	project := fs.String("project", config.GlobalProject, "project to warm")
	lockPath := fs.String("lock", "", "path to uv.lock")
	host := fs.String("host", "", "client-facing bare hostname or IP")
	asJSON := fs.Bool("json", false, "emit JSON progress")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *lockPath == "" || *host == "" {
		return errors.New("lockwarm: -lock and -host are required")
	}
	body, err := os.ReadFile(*lockPath)
	if err != nil {
		return err
	}
	return runDirectJob(ctx, collect(), *project, "lockwarm",
		map[string]any{"lock": string(body), "host": *host}, *asJSON)
}

func runGC(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	grace := fs.Duration("grace", 0, "override configured minimum candidate age")
	dryRun := fs.Bool("dry-run", false, "report eligible blobs without deleting them")
	asJSON := fs.Bool("json", false, "emit JSON progress")
	if err := fs.Parse(args); err != nil {
		return err
	}
	params := map[string]any{"dry_run": *dryRun}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "grace" {
			params["grace"] = grace.String()
		}
	})
	return runDirectJob(ctx, collect(), "", "gc", params, *asJSON)
}

func runEvict(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("evict", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	project := fs.String("project", "", "limit eviction to one project")
	target := fs.Int64("target-size", 0, "target blob-store size in bytes")
	minFree := fs.Int64("min-free", 0, "minimum filesystem free space in bytes")
	ttl := fs.Duration("ttl", 0, "evict entries not accessed within this duration")
	dryRun := fs.Bool("dry-run", false, "report entries without evicting them")
	asJSON := fs.Bool("json", false, "emit JSON progress")
	if err := fs.Parse(args); err != nil {
		return err
	}
	params := map[string]any{"dry_run": *dryRun}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "target-size":
			params["target_bytes"] = *target
		case "min-free":
			params["min_free_bytes"] = *minFree
		case "ttl":
			params["ttl"] = ttl.String()
		}
	})
	return runDirectJob(ctx, collect(), *project, "evict", params, *asJSON)
}

func runDirectJob(
	ctx context.Context,
	flags config.Flags,
	project, action string,
	params map[string]any,
	asJSON bool,
) error {
	snapshot, err := config.Load(flags)
	if err != nil {
		return err
	}
	instance, err := app.Open(snapshot)
	if err != nil {
		return err
	}
	defer func() { _ = instance.Close() }()
	record, err := instance.Jobs.Submit(project, action, "cli", params)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	offset := 0
	lastStatus := ""
	encoder := json.NewEncoder(os.Stdout)
	for {
		current, err := instance.Jobs.Get(record.ID)
		if err != nil {
			return err
		}
		chunk := ""
		if len(current.Log) > offset {
			chunk = current.Log[offset:]
			offset = len(current.Log)
		}
		if asJSON {
			if chunk != "" || current.Status != lastStatus {
				if err := encoder.Encode(map[string]any{
					"job_id": current.ID, "action": current.Action,
					"status": current.Status, "log": chunk,
				}); err != nil {
					return err
				}
			}
		} else if chunk != "" {
			fmt.Print(chunk)
		}
		lastStatus = current.Status
		switch current.Status {
		case "done":
			return nil
		case "failed":
			return errors.New(current.Error)
		case "cancelled":
			return context.Canceled
		}
		select {
		case <-ctx.Done():
			_ = instance.Jobs.Cancel(record.ID)
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
