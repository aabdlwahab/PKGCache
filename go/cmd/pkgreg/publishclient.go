package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/brightskies/pkgreg/internal/clientrelease"
	"github.com/brightskies/pkgreg/internal/config"
)

// runPublishClient copies client binaries into the data directory so the tutorial and
// Console → Connect can hand them out.
//
// This exists because the previous instruction — "from the go/ source directory, run
// make client-publish" — asks for something a production host does not have. The
// product is one static binary with no containers and no toolchain; an operator who
// deployed it that way has no Makefile, no Go compiler, and no cross-compiler, and so
// could not publish the client at all. The download page then dead-ends on every
// instance, which is the worst possible place for a dead end: it is the first page a
// new developer is sent to.
//
// So the server that serves the downloads is also what installs them. The operator
// copies the release files onto the host by whatever means they already use, and runs
// this.
func runPublishClient(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("publish-client", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: pkgreg publish-client [flags] [path...]

Publishes pkgreg-client binaries into <data-dir>/downloads and records their
SHA-256 digests. Each path may be a release file or a directory holding them;
with no path, the directory containing this pkgreg binary is used.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	snap, err := config.Load(collect())
	if err != nil {
		return err
	}

	paths := fs.Args()
	source := "given paths"
	if len(paths) == 0 {
		// The common case on a real host: the operator copied the release next to the
		// server binary. Guessing this beats making them type a path they can see.
		executable, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate this pkgreg binary: %w", err)
		}
		beside := filepath.Dir(executable)
		paths = []string{beside}
		source = beside
	}

	found, err := clientrelease.Collect(paths)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf(
			"no client binaries found in %s\n"+
				"Expected one or more of: %s\n"+
				"Build them with `make client-release` on a machine with Go, then copy "+
				"bin/pkgreg-client-* and bin/pkgreg-client-SHA256SUMS to this host.%s",
			source, strings.Join(clientrelease.ClientPlatforms(), ", "),
			archiveHint(paths))
	}

	dir := clientrelease.Dir(snap.DataDir)
	published, err := clientrelease.Publish(dir, found)
	if err != nil {
		return err
	}

	fmt.Printf("published into %s\n\n", dir)
	for _, binary := range published {
		fmt.Printf("  %-14s %-32s %9s  %s\n",
			binary.Platform(), binary.Name, humanBytes(binary.Bytes), short(binary.SHA256))
	}

	missing, corrupt, unrecorded, err := clientrelease.Verify(dir)
	if err != nil {
		return err
	}
	fmt.Println()
	switch {
	case len(corrupt) > 0:
		return fmt.Errorf("%s does not match its recorded digest after publishing",
			strings.Join(corrupt, ", "))
	case len(unrecorded) > 0:
		return fmt.Errorf("no digest was recorded for %s", strings.Join(unrecorded, ", "))
	case len(missing) > 0:
		fmt.Printf(`Still missing %d of %d client platforms:
  %s

Developers on those platforms will not see a download. Build the full set with
`+"`make client-release`"+` and publish it, or publish only the platforms your
team actually uses.%s
`, len(missing), len(clientrelease.ClientPlatforms()), strings.Join(missing, "\n  "),
			archiveHint(paths))
	default:
		fmt.Println("All 5 client platforms are published with matching digests.")
	}
	fmt.Printf("\nDevelopers can now download from https://<this-host>%s/tutorial\n",
		portSuffix(snap.Server.UnifiedAddr))
	return nil
}

// archiveHint names any release file that is a publishable binary still inside its
// archive, so "nothing found" and "still missing" stop being dead ends for the most
// likely input: a downloaded release whose macOS artifacts arrived as .zip.
func archiveHint(paths []string) string {
	archived := clientrelease.CollectNearMisses(paths)
	if len(archived) == 0 {
		return ""
	}
	return "\n\nThese files are archives holding a publishable binary. Extract each one\n" +
		"and publish the result:\n  " + strings.Join(archived, "\n  ")
}

// portSuffix renders ":8443" from a listener address, or nothing if the address has no
// port to report. The host half is deliberately not used: it is what the process binds
// to, which is routinely 0.0.0.0 and never the name a developer types.
func portSuffix(address string) string {
	_, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return ""
	}
	return ":" + port
}

// short abbreviates a digest for a report line. The full value is in the sums file and
// in the API, and a 64-character column pushes everything useful off the terminal.
func short(digest string) string {
	if len(digest) <= 16 {
		return digest
	}
	return digest[:16] + "…"
}
