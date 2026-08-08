package clientinstaller

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	return dir
}

func readConfig(t *testing.T, dir string) map[string]any {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func defaults(t *testing.T, dir string) map[string]any {
	t.Helper()
	proxies, _ := readConfig(t, dir)["proxies"].(map[string]any)
	entry, ok := proxies["default"].(map[string]any)
	if !ok {
		t.Fatalf("no proxies.default in %s", dir)
	}
	return entry
}

func TestBuildTrustWritesOnlyTheHTTPProxy(t *testing.T) {
	dir := configDir(t)
	options := Options{Server: "https://cache.example.com:8443", Stdout: &bytes.Buffer{}}
	if err := buildTrust(options); err != nil {
		t.Fatal(err)
	}
	entry := defaults(t, dir)
	if entry["httpProxy"] != "http://cache.example.com:8443" {
		t.Fatalf("httpProxy = %v", entry["httpProxy"])
	}
	// The rule this feature lives or dies by: the cache answers CONNECT with 405, so
	// an httpsProxy here would break every https fetch in every build on the machine,
	// including builds that have nothing to do with pkgreg.
	if _, set := entry["httpsProxy"]; set {
		t.Fatal("httpsProxy was written; this breaks every https fetch in every build")
	}
	if !strings.Contains(entry["noProxy"].(string), "127.0.0.1") {
		t.Fatalf("noProxy = %v, must exempt loopback or the bridge is unreachable", entry["noProxy"])
	}
}

func TestSessionProxyIsPreferredOverTheServerOrigin(t *testing.T) {
	dir := configDir(t)
	// A project-scoped proxy carries the project in its userinfo, which deriving the
	// address from -server alone would silently drop.
	t.Setenv("PKGREG_APT_PROXY", "http://team-a@cache.example.com:3142")
	if err := buildTrust(Options{Server: "https://cache.example.com:8443", Stdout: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if got := defaults(t, dir)["httpProxy"]; got != "http://team-a@cache.example.com:3142" {
		t.Fatalf("httpProxy = %v, want the session's project-scoped proxy", got)
	}
}

func TestExistingSettingsInTheDockerConfigSurvive(t *testing.T) {
	dir := configDir(t)
	original := `{"currentContext":"desktop-linux","auths":{"registry.example":{"auth":"x"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := buildTrust(Options{Server: "https://cache:8443", Stdout: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	document := readConfig(t, dir)
	if document["currentContext"] != "desktop-linux" {
		t.Fatalf("currentContext lost: %v", document["currentContext"])
	}
	if _, ok := document["auths"]; !ok {
		t.Fatal("registry credentials were dropped")
	}
}

// Overwriting a corporate proxy would break every build on the machine, and nobody
// would suspect the command that did it.
func TestAForeignProxyIsRefusedRatherThanOverwritten(t *testing.T) {
	dir := configDir(t)
	existing := `{"proxies":{"default":{"httpProxy":"http://corp-proxy:3128"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	err := buildTrust(Options{Server: "https://cache:8443", Stdout: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("overwrote a proxy pkgreg did not install")
	}
	if !strings.Contains(err.Error(), "corp") && !strings.Contains(err.Error(), "already sets") {
		t.Fatalf("unhelpful refusal: %v", err)
	}
	if got := defaults(t, dir)["httpProxy"]; got != "http://corp-proxy:3128" {
		t.Fatalf("the existing proxy was changed to %v", got)
	}
}

func TestUninstallRemovesOnlyWhatWasInstalled(t *testing.T) {
	dir := configDir(t)
	if err := buildTrust(Options{Server: "https://cache:8443", Stdout: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if err := buildTrust(Options{Uninstall: true, Stdout: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if proxies, ok := readConfig(t, dir)["proxies"]; ok {
		t.Fatalf("proxies survived uninstall: %v", proxies)
	}
}

func TestUninstallLeavesAForeignProxyAlone(t *testing.T) {
	dir := configDir(t)
	existing := `{"proxies":{"default":{"httpProxy":"http://corp-proxy:3128"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	buffer := &bytes.Buffer{}
	if err := buildTrust(Options{Uninstall: true, Stdout: buffer}); err != nil {
		t.Fatal(err)
	}
	if got := defaults(t, dir)["httpProxy"]; got != "http://corp-proxy:3128" {
		t.Fatalf("uninstall removed somebody else's proxy: %v", got)
	}
	if !strings.Contains(buffer.String(), "nothing installed by pkgreg") {
		t.Fatalf("uninstall said %q", buffer.String())
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	dir := configDir(t)
	buffer := &bytes.Buffer{}
	if err := buildTrust(Options{Server: "https://cache:8443", DryRun: true, Stdout: buffer}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatal("dry run wrote the file")
	}
	if !strings.Contains(buffer.String(), "Nothing was changed") {
		t.Fatalf("dry run output: %s", buffer.String())
	}
}

func TestRepeatedInstallIsIdempotent(t *testing.T) {
	dir := configDir(t)
	for range 3 {
		if err := buildTrust(Options{Server: "https://cache:8443", Stdout: &bytes.Buffer{}}); err != nil {
			t.Fatal(err)
		}
	}
	proxies, _ := readConfig(t, dir)["proxies"].(map[string]any)
	if len(proxies) != 1 {
		t.Fatalf("proxies accumulated: %v", proxies)
	}
}

func TestMissingAddressIsRefusedWithAnActionableMessage(t *testing.T) {
	configDir(t)
	err := buildTrust(Options{Stdout: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("installed with no address at all")
	}
	if !strings.Contains(err.Error(), "pkgreg shell") || !strings.Contains(err.Error(), "-server") {
		t.Fatalf("message does not say what to do: %v", err)
	}
}
