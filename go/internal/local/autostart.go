package local

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Opening the widget when somebody logs in.
//
// Distinct from the socket activation `persist` installs, and the difference matters. That
// keeps the *cache* reachable — a unit holding the port so a .npmrc naming it never fails.
// This opens a *window*, which is a desktop thing: nothing depends on it, nothing breaks
// without it, and it must never be what starts the daemon.
//
// Every file is marked, written under the user's own home, and removed exactly by
// -off-login — the same contract docker-setup and persist already keep, for the same
// reason: a program that installs something into somebody's session owes them a way to
// take it out again that leaves no trace.

// autostartMarker identifies files this wrote. A hand-written entry of the same name is
// left alone rather than replaced.
const autostartMarker = "# installed by pkgcache; remove it with -off-login"

// AutostartOptions configures the login entry.
type AutostartOptions struct {
	// Executable is the pkgcache binary the entry runs. Resolved by the caller, because
	// a login entry naming a binary that has since moved is worse than none.
	Executable string
	// Command is the subcommand it runs: "widget" for the window, "tray" for the status
	// bar item. Each gets its own file, so installing one never disturbs the other.
	Command string
	// Home overrides the user's home directory. Tests set it.
	Home   string
	Remove bool
	DryRun bool
	Out    io.Writer
}

// InstallAutostart writes, or removes, the entry that opens the widget on login.
func InstallAutostart(o AutostartOptions) error {
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	home := o.Home
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("locate home directory: %w", err)
		}
		home = resolved
	}
	command := o.Command
	if command == "" {
		command = "widget"
	}
	entry, err := autostartEntry(home, o.Executable, command)
	if err != nil {
		return err
	}

	// Whatever happens next, the entry this project used to write is cleared out first.
	// It names the same program by an older label, so leaving it behind means two login
	// entries starting two icons — and `-off-login` would remove only one of them,
	// because it looks for the name in use today.
	retireLegacyAutostart(runtime.GOOS, home, command, o.DryRun, out)

	if o.Remove {
		return removeAutostart(entry, command, o.DryRun, out)
	}
	if existing, err := os.ReadFile(entry.path); err == nil &&
		!strings.Contains(string(existing), autostartMarker) {
		return fmt.Errorf(
			"%s exists and was not written by pkgcache, so it is left alone.\n"+
				"  Remove it yourself first if you want this to manage it", entry.path)
	}
	if o.DryRun {
		fmt.Fprintf(out, "+ %s\n\n%s\n", entry.path, entry.content)
		fmt.Fprintln(out, "Nothing was changed.")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(entry.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(entry.path, []byte(entry.content), entry.mode); err != nil {
		return err
	}
	fmt.Fprintf(out, "+ %s\n", entry.path)
	fmt.Fprintf(out, "pkgcache: %s will start when you log in\n", describe(command))
	fmt.Fprintln(out, "  It watches the cache; it does not keep it running.")
	return nil
}

// describe names what an entry starts, in the words its own command uses.
func describe(command string) string {
	switch command {
	case "tray":
		return "the status bar icon"
	case "app":
		return "the app"
	}
	return "the widget"
}

func removeAutostart(entry loginEntry, command string, dryRun bool, out io.Writer) error {
	contents, err := os.ReadFile(entry.path)
	if err != nil {
		fmt.Fprintln(out, "pkgcache: no login entry to remove")
		return nil
	}
	if !strings.Contains(string(contents), autostartMarker) {
		return fmt.Errorf("%s was not written by pkgcache; it is left alone", entry.path)
	}
	if dryRun {
		fmt.Fprintf(out, "- %s\n\nNothing was changed.\n", entry.path)
		return nil
	}
	if err := os.Remove(entry.path); err != nil {
		return err
	}
	fmt.Fprintf(out, "- %s\n", entry.path)
	fmt.Fprintf(out, "pkgcache: %s will not start on login\n", describe(command))
	return nil
}

// loginEntry is one file, and the mode it needs.
type loginEntry struct {
	path    string
	content string
	mode    os.FileMode
}

// autostartEntry is the per-platform shape. Three formats, one contract: a marked file
// under the user's home that runs `pkgcache widget` once, at login.
// execArgs is what a login entry runs after the executable.
//
// Every command here is a pkgcache subcommand except one: the desktop app is its own
// binary, and what it needs is a flag telling it to come up as an icon rather than as a
// window across whatever the person was about to do.
func execArgs(command string) string {
	if command == "app" {
		return "-background"
	}
	return command
}

func autostartEntry(home, executable, command string) (loginEntry, error) {
	// One file per command. A person may want the icon and not the window, or the reverse,
	// and a shared filename would make -off-login on one silently remove the other.
	suffix := "-" + command
	switch runtime.GOOS {
	case "linux":
		// The XDG autostart directory, which every desktop environment on Linux reads.
		// No systemd unit: a window is not a service, and a user unit that opened a
		// browser would be restarted by systemd when somebody closed it.
		return loginEntry{
			path: filepath.Join(home, ".config", "autostart", "pkgcache"+suffix+".desktop"),
			mode: 0o644,
			content: autostartMarker + "\n" +
				"[Desktop Entry]\n" +
				"Type=Application\n" +
				"Name=pkgcache\n" +
				"Comment=Watch this machine's package cache\n" +
				"Exec=" + executable + " " + execArgs(command) + "\n" +
				"Terminal=false\n" +
				"X-GNOME-Autostart-enabled=true\n",
		}, nil
	case "darwin":
		// RunAtLoad with KeepAlive absent: launchd opens the window once and does not
		// reopen it when it is closed, which is the difference between an autostart entry
		// and a supervised service.
		return loginEntry{
			path: filepath.Join(home, "Library", "LaunchAgents",
				autostartLabel+suffix+".plist"),
			mode: 0o644,
			content: `<?xml version="1.0" encoding="UTF-8"?>
<!-- ` + autostartMarker + ` -->
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key><string>` + autostartLabel + suffix + `</string>
    <key>ProgramArguments</key>
    <array>
      <string>` + executable + `</string>
      <string>` + execArgs(command) + `</string>
    </array>
    <key>RunAtLoad</key><true/>
  </dict>
</plist>
`,
		}, nil
	case "windows":
		// A .cmd in the Startup folder rather than a registry Run value or a .lnk. All
		// three work; this one is a text file somebody can read, edit and delete without
		// regedit, and it needs no COM to create a shortcut with.
		return loginEntry{
			path: filepath.Join(home, "AppData", "Roaming", "Microsoft", "Windows",
				"Start Menu", "Programs", "Startup", "pkgcache"+suffix+".cmd"),
			mode: 0o644,
			content: "@echo off\nrem " + strings.TrimPrefix(autostartMarker, "# ") + "\n" +
				"start \"\" \"" + executable + "\" " + execArgs(command) + "\n",
		}, nil
	default:
		return loginEntry{}, fmt.Errorf("no login entry is defined for %s", runtime.GOOS)
	}
}

// autostartLabel is the reverse-DNS name of the login entry on macOS.
const autostartLabel = "org.pkgreg.pkgcache"

// legacyAutostartLabels are names this project wrote before, and no longer does.
//
// Renaming a login entry does not remove the old one: launchd keeps loading whatever
// plists are in LaunchAgents, so an install that has been upgraded would start the icon
// twice and `-off-login` would turn off only the half it knows the name of.
var legacyAutostartLabels = []string{"com.brightskies.pkgcache"}

// retireLegacyAutostart deletes login entries written under a previous name.
//
// Only ones carrying this program's marker are touched. A file somebody wrote themselves
// that happens to share the name is left where it is, which is the same rule install and
// remove already follow.
// The operating system is a parameter rather than read from runtime, so this can be
// tested on the host that builds it rather than only on the one platform it applies to.
func retireLegacyAutostart(goos, home, command string, dryRun bool, out io.Writer) {
	if goos != "darwin" {
		return
	}
	suffix := ""
	if command != "widget" {
		suffix = "." + command
	}
	for _, label := range legacyAutostartLabels {
		path := filepath.Join(home, "Library", "LaunchAgents", label+suffix+".plist")
		body, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(body), autostartMarker) {
			continue
		}
		if dryRun {
			fmt.Fprintf(out, "would remove the login entry left by an older name: %s\n", path)
			continue
		}
		if err := os.Remove(path); err == nil {
			fmt.Fprintf(out, "removed the login entry left by an older name: %s\n", path)
		}
	}
}
