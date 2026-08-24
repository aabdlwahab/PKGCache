package local

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The same contract docker-setup and persist keep: install, assert the file, uninstall,
// assert it is gone, and never touch something a person wrote themselves. Asserted
// against whichever platform is running the tests — the other two are compiled by CI and
// recorded as unrun, like the rest of pkgcache's desktop surface.
func TestAutostartInstallsAndReverses(t *testing.T) {
	home := t.TempDir()
	entry, err := autostartEntry(home, "/usr/local/bin/pkgcache", "widget")
	if err != nil {
		t.Skipf("no login entry is defined here: %v", err)
	}

	var out strings.Builder
	options := AutostartOptions{
		Executable: "/usr/local/bin/pkgcache", Command: "widget", Home: home, Out: &out,
	}
	if err := InstallAutostart(options); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(entry.path)
	if err != nil {
		t.Fatalf("nothing was installed: %v", err)
	}
	if !strings.Contains(string(written), autostartMarker) {
		t.Error("the entry carries no marker, so uninstall cannot know it is ours")
	}
	if !strings.Contains(string(written), "/usr/local/bin/pkgcache") {
		t.Error("the entry does not name the binary it should run")
	}
	// It opens a window. An entry that started the daemon would defeat the idle timeout
	// that makes this polite to leave installed.
	if strings.Contains(string(written), " serve") {
		t.Error("the login entry starts the daemon; it should only open the window")
	}

	remove := options
	remove.Remove = true
	if err := InstallAutostart(remove); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry.path); !os.IsNotExist(err) {
		t.Fatalf("the entry survived removal: %v", err)
	}
}

// Installing twice is not an error, and leaves one entry.
func TestAutostartIsIdempotent(t *testing.T) {
	home := t.TempDir()
	entry, err := autostartEntry(home, "pkgcache", "widget")
	if err != nil {
		t.Skip(err)
	}
	options := AutostartOptions{Executable: "pkgcache", Command: "widget", Home: home, Out: &strings.Builder{}}
	for range 2 {
		if err := InstallAutostart(options); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(entry.path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("two installs left %d files", len(entries))
	}
}

// Somebody else's file of the same name is left exactly as it was, in both directions.
func TestAutostartRefusesAHandWrittenEntry(t *testing.T) {
	home := t.TempDir()
	entry, err := autostartEntry(home, "pkgcache", "widget")
	if err != nil {
		t.Skip(err)
	}
	if err := os.MkdirAll(filepath.Dir(entry.path), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := "mine, do not touch\n"
	if err := os.WriteFile(entry.path, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	options := AutostartOptions{Executable: "pkgcache", Command: "widget", Home: home, Out: &strings.Builder{}}
	if err := InstallAutostart(options); err == nil {
		t.Error("a hand-written entry was overwritten")
	}
	remove := options
	remove.Remove = true
	if err := InstallAutostart(remove); err == nil {
		t.Error("a hand-written entry was removed")
	}
	if data, _ := os.ReadFile(entry.path); string(data) != mine {
		t.Fatalf("the file changed: %q", data)
	}
}

func TestAutostartDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	entry, err := autostartEntry(home, "pkgcache", "widget")
	if err != nil {
		t.Skip(err)
	}
	var out strings.Builder
	err = InstallAutostart(AutostartOptions{
		Executable: "pkgcache", Command: "widget", Home: home, DryRun: true, Out: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry.path); !os.IsNotExist(err) {
		t.Fatal("a dry run wrote the entry")
	}
	if !strings.Contains(out.String(), "Nothing was changed") {
		t.Fatalf("a dry run did not say so:\n%s", out.String())
	}
}

// The icon and the window are separate entries. Somebody may want one and not the other,
// and a shared filename would make -off-login on one silently remove the other.
func TestAutostartKeepsTheTwoCommandsApart(t *testing.T) {
	home := t.TempDir()
	widget, err := autostartEntry(home, "pkgcache", "widget")
	if err != nil {
		t.Skip(err)
	}
	tray, err := autostartEntry(home, "pkgcache", "tray")
	if err != nil {
		t.Fatal(err)
	}
	if widget.path == tray.path {
		t.Fatalf("both commands write %s", widget.path)
	}
	for _, command := range []string{"widget", "tray"} {
		if err := InstallAutostart(AutostartOptions{
			Executable: "pkgcache", Command: command, Home: home, Out: &strings.Builder{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := InstallAutostart(AutostartOptions{
		Executable: "pkgcache", Command: "widget", Home: home, Remove: true,
		Out: &strings.Builder{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(widget.path); !os.IsNotExist(err) {
		t.Error("removing the window entry left it behind")
	}
	if _, err := os.Stat(tray.path); err != nil {
		t.Errorf("removing the window entry took the icon's with it: %v", err)
	}
	if content, readErr := os.ReadFile(tray.path); readErr == nil &&
		!strings.Contains(string(content), "tray") {
		t.Error("the icon's entry does not run the tray")
	}
}

// A login entry written under the old name is removed when the new one is written.
//
// launchd loads whatever plists are in LaunchAgents, so a rename that leaves the previous
// file behind gives an upgraded machine two entries: two icons at login, and `-off-login`
// removing only the one whose name this version knows. The person is left turning off a
// thing that keeps coming back.
func TestAutostartRetiresAnEntryLeftByAnOlderName(t *testing.T) {
	home := t.TempDir()
	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(agents, legacyAutostartLabels[0]+".tray.plist")
	if err := os.WriteFile(legacy,
		[]byte("<!-- "+autostartMarker+" -->\n<plist/>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	retireLegacyAutostart("darwin", home, "tray", false, &out)

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("the entry under the old name survived: %v", err)
	}
	if !strings.Contains(out.String(), "older name") {
		t.Fatalf("removing it was not reported: %q", out.String())
	}
}

// One somebody wrote themselves is not ours to delete, even under a name we used to use.
func TestAutostartLeavesAHandWrittenLegacyEntryAlone(t *testing.T) {
	home := t.TempDir()
	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(agents, legacyAutostartLabels[0]+".plist")
	if err := os.WriteFile(mine, []byte("<plist>hand written</plist>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	retireLegacyAutostart("darwin", home, "widget", false, &strings.Builder{})

	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("a file pkgcache did not write was removed: %v", err)
	}
}

// And nothing is touched on a platform that never had that entry.
func TestAutostartRetirementIsDarwinOnly(t *testing.T) {
	home := t.TempDir()
	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(agents, legacyAutostartLabels[0]+".plist")
	if err := os.WriteFile(path,
		[]byte("<!-- "+autostartMarker+" -->\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retireLegacyAutostart("linux", home, "widget", false, &strings.Builder{})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a linux run reached into LaunchAgents: %v", err)
	}
}

// The desktop app's login entry, which is the one command here that is a separate binary
// rather than a pkgcache subcommand.
func TestAutostartRunsTheAppInTheBackground(t *testing.T) {
	home := t.TempDir()
	entry, err := autostartEntry(home, "/usr/bin/pkgcache-app", "app")
	if err != nil {
		t.Skip(err)
	}
	if err := InstallAutostart(AutostartOptions{
		Executable: "/usr/bin/pkgcache-app", Command: "app", Home: home,
		Out: &strings.Builder{},
	}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(entry.path)
	if err != nil {
		t.Fatal(err)
	}
	// -background, not "app": a login entry that opened a window across whatever the
	// person was about to do would be turned off the same day.
	if !strings.Contains(string(content), "-background") {
		t.Errorf("the app's login entry does not ask for the background:\n%s", content)
	}
	if strings.Contains(string(content), "pkgcache-app app") {
		t.Errorf("\"app\" leaked through as a subcommand:\n%s", content)
	}
}

func TestAutostartKeepsTheAppApartFromTheOldEntries(t *testing.T) {
	// Somebody upgrading has a tray entry already. Installing the app's must not silently
	// replace it, or -off-login on one removes the other.
	home := t.TempDir()
	app, err := autostartEntry(home, "pkgcache-app", "app")
	if err != nil {
		t.Skip(err)
	}
	tray, err := autostartEntry(home, "pkgcache", "tray")
	if err != nil {
		t.Fatal(err)
	}
	if app.path == tray.path {
		t.Fatalf("the app and the tray both write %s", app.path)
	}
}

func TestDescribeNamesTheApp(t *testing.T) {
	// The line a person reads after installing the login entry.
	if got := describe("app"); got != "the app" {
		t.Errorf("describe(\"app\") = %q, want %q", got, "the app")
	}
}
