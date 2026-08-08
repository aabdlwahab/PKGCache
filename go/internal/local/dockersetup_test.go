package local

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	document, err := readJSONFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func writeJSON(t *testing.T, path string, document map[string]any) {
	t.Helper()
	if err := writeJSONFile(path, document); err != nil {
		t.Fatal(err)
	}
}

func TestDockerSetupAddsAndReverses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	setup := DockerSetup{Address: "host.docker.internal:41780", ConfigPath: path, Out: io.Discard}

	if err := ApplyDockerSetup(setup); err != nil {
		t.Fatal(err)
	}
	document := readJSON(t, path)
	if got := stringList(document["insecure-registries"]); len(got) != 1 ||
		got[0] != "host.docker.internal:41780" {
		t.Fatalf("insecure-registries = %v", got)
	}
	// The mirror is opt-in: rerouting every pull on a machine is not a side effect.
	if _, present := document["registry-mirrors"]; present {
		t.Error("docker-setup registered a mirror without being asked")
	}

	// Applying twice changes nothing.
	if err := ApplyDockerSetup(setup); err != nil {
		t.Fatal(err)
	}
	if got := stringList(readJSON(t, path)["insecure-registries"]); len(got) != 1 {
		t.Fatalf("a second run duplicated the entry: %v", got)
	}

	uninstall := setup
	uninstall.Uninstall = true
	if err := ApplyDockerSetup(uninstall); err != nil {
		t.Fatal(err)
	}
	if _, present := readJSON(t, path)["insecure-registries"]; present {
		t.Error("uninstall left the entry behind")
	}
}

func TestDockerSetupMirrorIsOptIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	setup := DockerSetup{
		Address: "host.docker.internal:41780", Mirror: true,
		ConfigPath: path, Out: io.Discard,
	}
	if err := ApplyDockerSetup(setup); err != nil {
		t.Fatal(err)
	}
	mirrors := stringList(readJSON(t, path)["registry-mirrors"])
	if len(mirrors) != 1 || mirrors[0] != "http://host.docker.internal:41780" {
		t.Fatalf("registry-mirrors = %v", mirrors)
	}
	uninstall := setup
	uninstall.Uninstall = true
	if err := ApplyDockerSetup(uninstall); err != nil {
		t.Fatal(err)
	}
	document := readJSON(t, path)
	if _, present := document["registry-mirrors"]; present {
		t.Error("uninstall left the mirror behind")
	}
}

// The daemon rejects keys it does not know, so there is nowhere to record a marker and
// the entries are matched by value. That makes "leave everything else alone" the
// property worth proving, byte for byte.
func TestDockerSetupLeavesEverythingElseUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	writeJSON(t, path, map[string]any{
		"log-driver":          "json-file",
		"insecure-registries": []any{"registry.internal:5000"},
		"registry-mirrors":    []any{"https://mirror.internal"},
		"features":            map[string]any{"buildkit": true},
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	setup := DockerSetup{
		Address: "host.docker.internal:41780", Mirror: true,
		ConfigPath: path, Out: io.Discard,
	}
	if err := ApplyDockerSetup(setup); err != nil {
		t.Fatal(err)
	}
	document := readJSON(t, path)
	if got := stringList(document["insecure-registries"]); len(got) != 2 ||
		got[0] != "registry.internal:5000" {
		t.Fatalf("the operator's own registry was disturbed: %v", got)
	}

	uninstall := setup
	uninstall.Uninstall = true
	if err := ApplyDockerSetup(uninstall); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("uninstall did not restore the file byte for byte:\nbefore:\n%s\nafter:\n%s",
			before, after)
	}
}

// Rewriting a file we could not parse would discard whatever the operator put in it.
func TestDockerSetupRefusesUnparseableConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	original := []byte("{ this is not json")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	err := ApplyDockerSetup(DockerSetup{
		Address: "host.docker.internal:41780", ConfigPath: path, Out: io.Discard,
	})
	if err == nil {
		t.Fatal("an unparseable daemon.json was edited anyway")
	}
	current, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(current) != string(original) {
		t.Fatal("the file was modified despite the refusal")
	}
}

func TestDockerSetupDryRunChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	var out strings.Builder
	err := ApplyDockerSetup(DockerSetup{
		Address: "host.docker.internal:41780", DryRun: true, ConfigPath: path, Out: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("-dry-run created the configuration file")
	}
	if !strings.Contains(out.String(), "Nothing was changed") {
		t.Errorf("-dry-run did not say so: %q", out.String())
	}
	if !strings.Contains(out.String(), "insecure-registries") {
		t.Errorf("-dry-run did not describe the change: %q", out.String())
	}
}

func TestDockerBuildProxyInstallsAndReverses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeJSON(t, path, map[string]any{"currentContext": "desktop-linux"})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	setup := DockerBuildProxy{
		Address: "host.docker.internal:41780", ConfigPath: path, Out: io.Discard,
	}
	if err := ApplyDockerBuildProxy(setup); err != nil {
		t.Fatal(err)
	}
	document := readJSON(t, path)
	proxies, _ := document["proxies"].(map[string]any)
	entry, _ := proxies["default"].(map[string]any)
	if entry["httpProxy"] != "http://host.docker.internal:41780" {
		t.Fatalf("httpProxy = %v", entry["httpProxy"])
	}
	if entry[managedKey] != true {
		t.Error("the entry is not marked as ours, so uninstall could not tell")
	}
	// noProxy keeps pip, uv and npm out of a proxy that only relays http://.
	if noProxy, _ := entry["noProxy"].(string); !strings.Contains(noProxy, "127.0.0.1") ||
		!strings.Contains(noProxy, "host.docker.internal") {
		t.Errorf("noProxy = %q", noProxy)
	}
	if document["currentContext"] != "desktop-linux" {
		t.Error("the rest of the configuration was disturbed")
	}

	uninstall := setup
	uninstall.Uninstall = true
	if err := ApplyDockerBuildProxy(uninstall); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("uninstall did not restore the file:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A proxy somebody else configured is not ours to replace or to remove.
func TestDockerBuildProxyRefusesToClobberAHandWrittenEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeJSON(t, path, map[string]any{
		"proxies": map[string]any{
			"default": map[string]any{"httpProxy": "http://corporate.proxy:3128"},
		},
	})
	setup := DockerBuildProxy{
		Address: "host.docker.internal:41780", ConfigPath: path, Out: io.Discard,
	}
	if err := ApplyDockerBuildProxy(setup); err == nil {
		t.Fatal("a hand-written build proxy was overwritten")
	}

	uninstall := setup
	uninstall.Uninstall = true
	if err := ApplyDockerBuildProxy(uninstall); err != nil {
		t.Fatal(err)
	}
	document := readJSON(t, path)
	proxies, _ := document["proxies"].(map[string]any)
	entry, _ := proxies["default"].(map[string]any)
	if entry == nil || entry["httpProxy"] != "http://corporate.proxy:3128" {
		t.Fatal("uninstall removed a proxy pkgcache did not install")
	}
}

func TestWriteJSONFilePreservesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, path, map[string]any{"a": "b"})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %o, want the file's original 600", mode)
	}
	var document map[string]any
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document["a"] != "b" {
		t.Fatalf("document = %v", document)
	}
}
