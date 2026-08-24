package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/feed"
)

// runPublishApt writes the apt repository this instance serves.
//
// The Linux half of the update story. Two packages — pkgcache and pkgcache-desktop —
// mean the desktop install is one apt command that pulls both, and a headless machine can
// still take the daemon alone; that only works from a repository, so the server that
// serves the packages publishes one.
//
// It runs where publish-client runs, for the same reason: a production host has this
// binary and nothing else. No dpkg-scanpackages, no apt-ftparchive, no reprepro, and no
// gpg — the indexes are rendered and the Release signed in process.
func runPublishApt(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("publish-apt", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	suite := fs.String("suite", "stable", "the suite to publish into")
	component := fs.String("component", "main", "the component to publish into")
	origin := fs.String("origin", "",
		"the URL clients will fetch from, used for the sources file this prints")
	algorithm := fs.String("key-algorithm", string(feed.KeyRSA),
		"algorithm for a newly created signing key: rsa or ed25519")
	showKey := fs.Bool("print-key", false, "print the public key and exit")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: pkgreg publish-apt [flags] [path...]

Publishes .deb packages into the apt repository this instance serves. Each path may be a
.deb or a directory holding them; with no path, the directory containing this pkgreg
binary is used.

The first run creates the repository's signing key and prints its fingerprint. Keep that
fingerprint: it is what a machine checks before trusting anything this repository serves.
The key itself never leaves the data directory, and is not inside the part of it that is
served.

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

	key, created, err := loadOrCreateSigningKey(snap.DataDir, feed.KeyAlgorithm(*algorithm))
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("created a signing key for this repository\n  fingerprint: %s\n\n",
			key.Fingerprint())
	}
	if *showKey {
		public, err := key.ArmoredPublic()
		if err != nil {
			return err
		}
		fmt.Print(string(public))
		return nil
	}

	paths := fs.Args()
	source := "the given paths"
	if len(paths) == 0 {
		// The common case on a real host: the operator copied the release next to the
		// server binary. Guessing this beats making them type a path they can see.
		executable, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate this pkgreg binary: %w", err)
		}
		paths = []string{filepath.Dir(executable)}
		source = paths[0]
	}
	debs, err := collectDebs(paths)
	if err != nil {
		return err
	}
	if len(debs) == 0 {
		return fmt.Errorf(
			"no .deb packages found in %s\n"+
				"Build them with `make deb` on a machine with Go, then copy the files here",
			source)
	}

	result, err := feed.WriteRepository(feed.RepoOptions{
		Root:      feed.RepoDir(snap.DataDir),
		Debs:      debs,
		Key:       key,
		Suite:     *suite,
		Component: *component,
		Origin:    "pkgreg",
		Label:     "pkgcache",
	})
	if err != nil {
		return err
	}

	fmt.Printf("published %d package(s) for %s into %s\n",
		len(result.Packages), strings.Join(result.Architectures, ", "),
		feed.RepoDir(snap.DataDir))
	for _, pkg := range result.Packages {
		fmt.Printf("  %-24s %-8s %s\n",
			pkg.Get("Package"), pkg.Get("Version"), pkg.Get("Architecture"))
	}
	fmt.Printf("\nsigned with %s\n", result.Fingerprint)

	if *origin != "" {
		fmt.Printf("\nOn a client, as /etc/apt/sources.list.d/pkgcache.sources:\n\n%s\n",
			indent(feed.SourcesLine(*origin, *suite, *component)))
		fmt.Printf("The keyring it names is published at %s/pkgcache-archive-keyring.asc;\n"+
			"`pkgcache setup` installs both.\n", strings.TrimSuffix(*origin, "/"))
	} else {
		fmt.Printf("\nRe-run with -origin https://<this host>/apt to print the sources file\n" +
			"a client needs.\n")
	}
	return nil
}

// loadOrCreateSigningKey reads the repository key, making one the first time.
//
// Created rather than demanded, because an operator publishing their first package should
// not have to learn gpg to do it. Once it exists it is never regenerated: a new key means
// every machine that trusted the old one stops trusting this repository, which is a much
// worse day than a missing key.
func loadOrCreateSigningKey(dataDir string, algorithm feed.KeyAlgorithm) (*feed.PGPKey, bool, error) {
	path := feed.KeyPath(dataDir)
	// #nosec G304 -- a path derived from the configured data directory.
	existing, err := os.ReadFile(path)
	if err == nil {
		key, err := feed.LoadKey(existing)
		if err != nil {
			return nil, false, fmt.Errorf(
				"%s is not a usable signing key: %w\n"+
					"  Move it aside to have a new one created — but every machine that "+
					"trusted the old key\n  will need the new one", path, err)
		}
		return key, false, nil
	}
	if !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("read signing key: %w", err)
	}

	key, err := feed.GenerateKey(feed.KeyOptions{
		Name:      "pkgcache repository",
		Email:     "pkgcache@localhost",
		Algorithm: algorithm,
	})
	if err != nil {
		return nil, false, err
	}
	armored, err := key.ArmoredPrivate()
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("create key directory: %w", err)
	}
	// 0600, and the directory 0700. Anybody who reads this file can publish a package
	// every machine in the organisation will install without complaint.
	if err := os.WriteFile(path, armored, 0o600); err != nil {
		return nil, false, fmt.Errorf("write signing key: %w", err)
	}
	return key, true, nil
}

// collectDebs finds .deb files among the given files and directories.
//
// One level, not a walk: an operator pointing at a directory means the files in it, and
// recursing would pick up whatever else happens to be on the host.
func collectDebs(paths []string) ([]string, error) {
	seen := make(map[string]bool)
	var out []string
	add := func(name string) {
		if strings.HasSuffix(name, ".deb") && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, given := range paths {
		info, err := os.Stat(given)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", given, err)
		}
		if !info.IsDir() {
			add(given)
			continue
		}
		entries, err := os.ReadDir(given)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", given, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				add(filepath.Join(given, entry.Name()))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func indent(text string) string {
	var out strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		out.WriteString("    " + line + "\n")
	}
	return out.String()
}
