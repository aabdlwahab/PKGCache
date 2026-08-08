package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/control"
)

func runAudit(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	limit := fs.Int("limit", 100, "maximum records to print; zero means all")
	asJSON := fs.Bool("json", false, "print one JSON object per line")
	if err := fs.Parse(args); err != nil {
		return err
	}
	snapshot, err := config.Load(collect())
	if err != nil {
		return err
	}
	db, err := control.Open(snapshot.ControlPath())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	records, err := db.ListAudit(*limit)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	for _, record := range records {
		if *asJSON {
			if err := encoder.Encode(record); err != nil {
				return err
			}
			continue
		}
		fmt.Printf("%s  %-16s %-24s %s  %s\n",
			record.Time.Format("2006-01-02T15:04:05Z"),
			record.Actor, record.Action, record.Target, record.ClientIP)
	}
	return nil
}
