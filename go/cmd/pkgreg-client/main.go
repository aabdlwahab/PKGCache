// Command pkgreg-client opens a verified session against a pkgreg cache.
//
// It is now a shim. Everything it did lives in pkgcache, which is the same program
// with a local cache it does not have to use: `pkgcache setup -no-cache` is exactly
// this client's behaviour, and every other mode is a superset of it.
//
// The shim exists because instructions outlive binaries. A tutorial, a Makefile and a
// CI job that say `pkgreg-client` should keep working across the release that merges
// the two, and be told once, on stderr, where they should be pointed instead.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"pkgreg-client: this is now pkgcache, which does the same thing and can also\n"+
			"  cache locally. `pkgcache setup -no-cache -server … -ca-sha256 …` is this\n"+
			"  client exactly. Forwarding.")

	target, err := locate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pkgreg-client: %v\n", err)
		os.Exit(1)
	}
	// Wrapped rather than exec'd, because syscall.Exec does not exist on Windows and a
	// shim that behaves differently per platform is a shim that gets debugged. The cost
	// is one extra process for the release or two this exists.
	//
	// Standard streams are handed over directly, so the terminal, the child's prompt and
	// its interactive shell all work; Ctrl-C reaches both through the process group.
	// #nosec G204 -- target is pkgcache, resolved beside this binary or on PATH.
	cmd := exec.Command(target, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			// pkgcache has already said whatever it wanted to; its status is the answer.
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pkgreg-client: run %s: %v\n", target, err)
		os.Exit(1)
	}
}

// locate prefers the pkgcache sitting beside this binary, because that is what a
// release ships and what an air-gapped host will have copied.
func locate() (string, error) {
	if self, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(self), "pkgcache")
		if info, statErr := os.Stat(beside); statErr == nil && !info.IsDir() {
			return beside, nil
		}
	}
	found, err := exec.LookPath("pkgcache")
	if err != nil {
		return "", fmt.Errorf(
			"pkgcache is not installed beside this binary or on PATH; install it from the "+
				"same place this came from: %w", err)
	}
	return found, nil
}
