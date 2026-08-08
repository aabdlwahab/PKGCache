package session

import (
	"slices"
	"strings"
	"testing"
)

func lookup(environment []string, name string) (string, bool) {
	for _, entry := range environment {
		if key, value, _ := strings.Cut(entry, "="); key == name {
			return value, true
		}
	}
	return "", false
}

func TestEnvironmentPointsEveryToolAtTheCache(t *testing.T) {
	environment := Environment([]string{"PATH=/usr/bin"}, Options{
		Prefix: PkgcachePrefix, Kind: "local",
		BaseURL: "http://127.0.0.1:41780", Project: "global",
		AptProxy: "http://127.0.0.1:41780", DockerRegistry: "127.0.0.1:41780",
		GitHosts: []string{"github.com"},
	})
	want := map[string]string{
		"PATH":                     "/usr/bin",
		"PKGCACHE_SESSION":         "local",
		"PKGCACHE_PROJECT":         "global",
		"PKGCACHE_BRIDGE_URL":      "http://127.0.0.1:41780",
		"PKGCACHE_APT_PROXY":       "http://127.0.0.1:41780",
		"PIP_INDEX_URL":            "http://127.0.0.1:41780/global/pypi/root/pypi/+simple/",
		"UV_DEFAULT_INDEX":         "http://127.0.0.1:41780/global/pypi/root/pypi/+simple/",
		"NPM_CONFIG_REGISTRY":      "http://127.0.0.1:41780/global/npm/",
		"GIT_CONFIG_COUNT":         "1",
		"GIT_CONFIG_KEY_0":         "url.http://127.0.0.1:41780/global/git/github.com/.insteadOf",
		"GIT_CONFIG_VALUE_0":       "https://github.com/",
		"NO_PROXY":                 "127.0.0.1,localhost",
		"PKGCACHE_FILES_URL":       "http://127.0.0.1:41780/global/files/",
		"PKGCACHE_GIT_URL":         "http://127.0.0.1:41780/global/git",
		"PKGCACHE_DOCKER_REGISTRY": "127.0.0.1:41780",
	}
	for name, expected := range want {
		got, ok := lookup(environment, name)
		if !ok {
			t.Errorf("%s is not set", name)
			continue
		}
		if got != expected {
			t.Errorf("%s = %q, want %q", name, got, expected)
		}
	}
}

// http_proxy is deliberately not set. The forward proxy relays http:// only, so
// exporting it would send curl, wget and every HTTPS client through something that
// cannot serve them — trading one working tool for several broken ones.
func TestEnvironmentDoesNotHijackEveryClient(t *testing.T) {
	environment := Environment(nil, Options{
		Prefix: PkgcachePrefix, BaseURL: "http://127.0.0.1:41780",
		AptProxy: "http://127.0.0.1:41780",
	})
	for _, name := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY"} {
		if value, ok := lookup(environment, name); ok {
			t.Errorf("%s was set to %q; the apt proxy must be opt-in", name, value)
		}
	}
	if _, ok := lookup(environment, "PKGCACHE_APT_PROXY"); !ok {
		t.Error("the apt proxy address should still be published for deliberate use")
	}
}

// Entering a session twice, or entering pkgcache's inside pkgreg-client's, must not
// leave the previous one's settings behind — most of all the certificate paths, which
// a loopback session does not use and which fail confusingly when stale.
func TestEnvironmentClearsBothNamespacesAndStaleToolSettings(t *testing.T) {
	before := []string{
		"PATH=/usr/bin",
		"PIP_CERT=/etc/pkgreg/ca.crt",
		"PIP_INDEX_URL=https://cache:8443/global/pypi/root/pypi/+simple/",
		"NPM_CONFIG_CAFILE=/etc/pkgreg/ca.crt",
		"NODE_EXTRA_CA_CERTS=/etc/pkgreg/ca.crt",
		"GIT_SSL_CAINFO=/etc/pkgreg/ca.crt",
		"UV_NATIVE_TLS=true",
		"PKGREG_SERVER=https://cache:8443",
		"PKGREG_PROJECT=team-a",
		"PKGREG_CA_FILE=/etc/pkgreg/ca.crt",
		"PKGCACHE_SESSION=local",
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_2=url.http://old/.insteadOf",
		"GIT_CONFIG_VALUE_2=https://old/",
	}
	environment := Environment(before, Options{
		Prefix: PkgcachePrefix, Kind: "local",
		BaseURL: "http://127.0.0.1:41780", Project: "global",
		GitHosts: []string{"github.com"},
	})
	joined := strings.Join(environment, "\n")
	for _, gone := range []string{
		"PIP_CERT=", "NPM_CONFIG_CAFILE=", "NODE_EXTRA_CA_CERTS=", "GIT_SSL_CAINFO=",
		"UV_NATIVE_TLS=", "PKGREG_SERVER=", "PKGREG_PROJECT=", "PKGREG_CA_FILE=",
		"https://cache:8443",
	} {
		if strings.Contains(joined, gone) {
			t.Errorf("a previous session's %q survived:\n%s", gone, joined)
		}
	}
	// The third git redirect from a longer previous session must not survive a shorter
	// one, or a clone would still be sent to a cache that is no longer there.
	if _, ok := lookup(environment, "GIT_CONFIG_KEY_2"); ok {
		t.Error("a stale GIT_CONFIG_KEY_2 survived a session that declares one host")
	}
	if count, _ := lookup(environment, "GIT_CONFIG_COUNT"); count != "1" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 1", count)
	}
	if path, _ := lookup(environment, "PATH"); path != "/usr/bin" {
		t.Error("the caller's own environment was disturbed")
	}
}

func TestEnvironmentPreservesAndExtendsNoProxy(t *testing.T) {
	environment := Environment([]string{"NO_PROXY=example.test,127.0.0.1"}, Options{
		Prefix: PkgcachePrefix, BaseURL: "http://127.0.0.1:41780",
	})
	value, _ := lookup(environment, "NO_PROXY")
	if value != "example.test,127.0.0.1,localhost" {
		t.Fatalf("NO_PROXY = %q", value)
	}
	lower, _ := lookup(environment, "no_proxy")
	if lower != value {
		t.Fatalf("no_proxy = %q, want it to match NO_PROXY", lower)
	}
}

func TestEnvironmentLeavesGitAloneWhenAsked(t *testing.T) {
	environment := Environment(nil, Options{
		Prefix: PkgcachePrefix, BaseURL: "http://127.0.0.1:41780",
	})
	for _, name := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0"} {
		if _, ok := lookup(environment, name); ok {
			t.Errorf("%s was set for a session that declares no git hosts", name)
		}
	}
}

func TestEnvironmentRedirectsSeveralGitHosts(t *testing.T) {
	environment := Environment(nil, Options{
		Prefix: PkgcachePrefix, BaseURL: "http://127.0.0.1:41780", Project: "global",
		GitHosts: []string{"github.com", "gitlab.com"},
	})
	if count, _ := lookup(environment, "GIT_CONFIG_COUNT"); count != "2" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want 2", count)
	}
	if key, _ := lookup(environment, "GIT_CONFIG_KEY_1"); !strings.Contains(key, "gitlab.com") {
		t.Fatalf("GIT_CONFIG_KEY_1 = %q", key)
	}
}

func TestEnvironmentDefaultsTheProject(t *testing.T) {
	environment := Environment(nil, Options{
		Prefix: PkgcachePrefix, BaseURL: "http://127.0.0.1:41780/",
	})
	index, _ := lookup(environment, "PIP_INDEX_URL")
	if !strings.HasPrefix(index, "http://127.0.0.1:41780/global/") {
		t.Fatalf("PIP_INDEX_URL = %q; a trailing slash on the base leaked through", index)
	}
}

func TestShellPicksAnInteractiveShell(t *testing.T) {
	program, args, err := Shell("/bin/bash", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if program != "/bin/bash" || !slices.Contains(args, "-i") {
		t.Fatalf("Unix shell = %q %v", program, args)
	}

	program, args, err = Shell("", "windows")
	if err != nil {
		t.Fatal(err)
	}
	if program != "powershell.exe" || !slices.Contains(args, "-NoLogo") {
		t.Fatalf("Windows shell = %q %v", program, args)
	}

	if _, _, err := Shell("", "plan9"); err == nil {
		t.Fatal("Shell accepted an unsupported operating system")
	}
}

func TestShellFallsBackToTheUsersOwn(t *testing.T) {
	t.Setenv("SHELL", "/usr/bin/fish")
	program, _, err := Shell("", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if program != "/usr/bin/fish" {
		t.Fatalf("shell = %q, want $SHELL", program)
	}
	t.Setenv("SHELL", "")
	program, _, err = Shell("", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if program != "/bin/sh" {
		t.Fatalf("shell = %q, want the POSIX fallback", program)
	}
}
