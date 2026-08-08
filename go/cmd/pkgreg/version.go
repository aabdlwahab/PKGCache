package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/brightskies/pkgreg/internal/buildinfo"
)

func runVersion(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "print machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	info := buildinfo.Get()
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}
	fmt.Println(info.String())
	return nil
}
