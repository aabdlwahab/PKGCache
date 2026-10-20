package main

import (
	"errors"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

// The invocation the tutorial printed for a year has to become the two pkgcache commands
// that do the same thing. Forwarding it unchanged made it `pkgcache -server …`, which is
// an unknown command.
func TestTranslateTurnsTheSessionFormIntoSetupThenShell(t *testing.T) {
	cases := map[string]struct {
		args []string
		env  map[string]string
		want [][]string
	}{
		"the tutorial's command": {
			args: []string{"-server", "https://cache:8443", "-project", "web", "-ca-sha256", "AB:CD"},
			want: [][]string{
				{"setup", "-no-cache", "-server", "https://cache:8443",
					"-team-project", "web", "-ca-sha256", "AB:CD"},
				{"shell"},
			},
		},
		"double dashes and =": {
			args: []string{"--server=https://cache:8443", "--ca-sha256=AB", "--token-file=/t"},
			want: [][]string{
				{"setup", "-no-cache", "-server", "https://cache:8443",
					"-ca-sha256", "AB", "-token-file", "/t"},
				{"shell"},
			},
		},
		"a CA file, a token and a shell": {
			args: []string{"-server", "https://c", "-ca-file", "ca.pem",
				"-token-file", "tok", "-shell", "zsh"},
			want: [][]string{
				{"setup", "-no-cache", "-server", "https://c", "-ca-file", "ca.pem",
					"-token-file", "tok"},
				{"shell", "zsh"},
			},
		},
		// The old client read its token file from the environment when no flag named
		// one, and CI jobs were written against that.
		"a token file from the environment": {
			args: []string{"-server", "https://c", "-ca-sha256", "AB"},
			env:  map[string]string{"PKGREG_TOKEN_FILE": "/run/token"},
			want: [][]string{
				{"setup", "-no-cache", "-server", "https://c", "-ca-sha256", "AB",
					"-token-file", "/run/token"},
				{"shell"},
			},
		},
		"version": {
			args: []string{"-version"},
			want: [][]string{{"version"}},
		},
		// A subcommand is already pkgcache's own vocabulary.
		"build is forwarded": {
			args: []string{"build", "-t", "app", "."},
			want: [][]string{{"build", "-t", "app", "."}},
		},
		"compose is forwarded": {
			args: []string{"compose", "up", "-d"},
			want: [][]string{{"compose", "up", "-d"}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			getenv := func(key string) string { return tc.env[key] }
			got, err := translate(tc.args, getenv, io.Discard)
			if err != nil {
				t.Fatalf("translate(%q): %v", tc.args, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("translate(%q)\n got %q\nwant %q", tc.args, got, tc.want)
			}
		})
	}
}

// The modes that installed things are answered with what replaced them, not run and not
// rejected as unknown flags.
func TestTranslateNamesTheReplacementForInstallModes(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"--persist":           {[]string{"--persist", "-server", "https://c"}, "pkgcache persist"},
		"-persist":            {[]string{"-persist", "-server", "https://c"}, "setup.sh"},
		"-dry-run":            {[]string{"-dry-run", "-server", "https://c"}, "pkgcache persist"},
		"-docker-trust":       {[]string{"-docker-trust", "-server", "https://c"}, "pkgcache docker-setup"},
		"-docker-build-trust": {[]string{"-docker-build-trust"}, "pkgcache docker-build-setup"},
		"-docker-trust -dry-run": {
			[]string{"-docker-trust", "-dry-run", "-server", "https://c"}, "pkgcache docker-setup",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			steps, err := translate(tc.args, noEnv, io.Discard)
			if err == nil {
				t.Fatalf("translate(%q) = %q, want a refusal", tc.args, steps)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("translate(%q): %v\nwant it to name %q", tc.args, err, tc.want)
			}
		})
	}
}

func TestTranslateRefusesWhatItCannotRun(t *testing.T) {
	if _, err := translate(nil, noEnv, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "-server") {
		t.Fatalf("no arguments: err = %v, want -server is required", err)
	}
	if _, err := translate([]string{"-server", "https://c", "extra"}, noEnv, io.Discard); err == nil {
		t.Fatal("a positional argument after the session flags was accepted")
	}
	if _, err := translate([]string{"-h"}, noEnv, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h: err = %v, want flag.ErrHelp", err)
	}
}

// LookPath appends .exe on Windows by itself; the path beside the shim is built by hand.
func TestExecutableNameCarriesTheWindowsSuffix(t *testing.T) {
	if got := executableName("windows"); got != "pkgcache.exe" {
		t.Fatalf("windows: %q", got)
	}
	if got := executableName("linux"); got != "pkgcache" {
		t.Fatalf("linux: %q", got)
	}
}
