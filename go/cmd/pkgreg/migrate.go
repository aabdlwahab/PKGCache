package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/migrate/frompython"
)

func runMigrate(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, `usage: pkgreg migrate from-python [flags]

Imports a live Python cache into the Go store without modifying the source.
The operation is resumable; run it once in the background and once immediately
before cutover to capture content added during the bulk pass.`)
		return nil
	}
	if args[0] != "from-python" && args[0] != "frompython" {
		return fmt.Errorf("migrate: unknown source %q (want from-python)", args[0])
	}
	fs := flag.NewFlagSet("migrate from-python", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	source := fs.String("source", "", "Python caches directory")
	registry := fs.String("registry-dir", "", "directory containing projects.json and users.json")
	strict := fs.Bool("strict", false, "stop on a stale ledger row instead of warning")
	skipUsers := fs.Bool("skip-users", false, "do not stage the legacy users registry")
	skipGit := fs.Bool("skip-git", false, "do not import managed bare Git mirrors")
	checkpoint := fs.Bool("checkpoint", true, "create checkpoint #1 for imported projects")
	asJSON := fs.Bool("json", false, "emit the final report as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*source) == "" {
		return errors.New("migrate from-python: -source is required")
	}
	snapshot, err := config.Load(collect())
	if err != nil {
		return err
	}
	progress := func(message string) {
		fmt.Fprintln(os.Stderr, message)
	}
	report, err := frompython.Run(ctx, frompython.Options{
		SourceDir: *source, DataDir: snapshot.DataDir, ConfigDir: *registry,
		Strict: *strict, SkipUsers: *skipUsers, SkipGit: *skipGit,
		Progress: progress,
	})
	if err != nil {
		return err
	}
	if *checkpoint {
		for _, project := range report.NeedsCheckpoint {
			if err := runCheckpoint(ctx, []string{
				"-data-dir", snapshot.DataDir,
				"-project", project,
				"-message", "migrated from Python cache",
			}); err != nil {
				return fmt.Errorf("initial checkpoint for %s: %w", project, err)
			}
		}
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	fmt.Printf(
		"migration complete in %s: %d CAS blobs, %d entries, %d artifacts, %d refs, %d managed Git files\n",
		report.Elapsed, report.CASBlobs, report.Entries, report.Artifacts,
		report.Refs, report.ManagedFiles,
	)
	fmt.Printf("linked %s without re-hashing; hashed %d non-CAS files; resumed over %d completed items\n",
		humanBytes(report.LinkedBytes), report.HashedFiles, report.Skipped)
	if len(report.Warnings) > 0 {
		fmt.Printf("%d warning(s); use -strict to make stale source rows fatal\n", len(report.Warnings))
	}
	return nil
}
