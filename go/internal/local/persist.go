package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

// Persistent settings: the mode that makes tools use the cache without being wrapped.
//
// `pkgcache run` and `pkgcache shell` cover the case where somebody types pkgcache.
// This covers the rest: a Makefile, an IDE, a colleague's script, a terminal opened
// before anybody thought about caching. It writes user-level files only — no root, no
// machine-wide trust store, nothing under /etc — and every one of them is fenced by a
// marker so uninstall removes exactly what was installed.
//
// It is deliberately paired with keeping the cache reachable. Settings that outlive the
// process they point at are the one way this design can leave a machine worse than it
// found it, so persist will not install unless it can also guarantee the address stays
// answerable: socket activation where it exists, a resident daemon where it does not.

const (
	beginMarker = "# >>> pkgcache >>>"
	endMarker   = "# <<< pkgcache <<<"
)

// PersistOptions configures the persistent installation.
type PersistOptions struct {
	// BaseURL is the origin the settings name, without a trailing slash.
	BaseURL string
	// Project scopes the URLs.
	Project string
	// GitHosts are redirected through the cache's mirror. Empty leaves git alone.
	GitHosts []string
	// Home overrides the user's home directory. Tests set it.
	Home string
	// DataDir is where the record of this installation is kept. Empty records nothing,
	// which is what -print and the tests that only inspect files want.
	DataDir string
	// DryRun prints every change and applies none.
	DryRun bool
	// Uninstall reverses a previous run.
	Uninstall bool
	// Print writes the settings to Out instead of applying them, so somebody can read
	// what would land in their home before it does.
	Print     bool
	Out       io.Writer
	Available Availability
}

// Availability describes how the cache stays reachable for settings that outlive a
// process.
type Availability int

const (
	// AvailabilityUnknown means it has not been established, and persist refuses.
	AvailabilityUnknown Availability = iota
	// AvailabilitySocket means an activation socket holds the port open.
	AvailabilitySocket
	// AvailabilityAccepted means the user asked for the settings anyway, knowing the
	// address can stop answering.
	//
	// There is deliberately no "a supervised daemon stays resident" value. It was in an
	// earlier draft, and a constant naming a guarantee nothing implements is worse than
	// its absence: somebody would have read it as a mode that exists.
	AvailabilityAccepted
)

// persistPath is where the record of an installation lives.
func persistPath(dataDir string) string { return filepath.Join(dataDir, "persist.json") }

// Persisted is what a previous `pkgcache persist` installed.
//
// Recorded because the settings outlive the shell that wrote them and name one project
// literally: months later, "which project is my .npmrc pointing at" is a question with an
// answer nobody can see. The files are listed too, since uninstall works by marker and a
// person checking up on it should not have to guess where to look.
type Persisted struct {
	Project string   `json:"project"`
	Files   []string `json:"files"`
	BaseURL string   `json:"base_url"`
}

// ReadPersisted returns the recorded installation, and whether there is one.
//
// A missing or unreadable record reads as "nothing installed". It is a note about files
// that are their own source of truth — each one carries pkgcache's markers — so a lost
// record must never make `status` fail or `uninstall` refuse.
func ReadPersisted(dataDir string) (Persisted, bool) {
	data, err := os.ReadFile(persistPath(dataDir))
	if err != nil {
		return Persisted{}, false
	}
	var record Persisted
	if err := json.Unmarshal(data, &record); err != nil || record.Project == "" {
		return Persisted{}, false
	}
	return record, true
}

func writePersisted(dataDir string, record Persisted) error {
	if dataDir == "" {
		return nil
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(persistPath(dataDir), append(data, '\n'), 0o600)
}

// managedFile is one file persist owns a fenced region of.
type managedFile struct {
	path    string
	content string
}

// ApplyPersist installs or removes the persistent settings.
func ApplyPersist(o PersistOptions) error {
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	home := o.Home
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("local: locate home directory: %w", err)
		}
		home = resolved
	}
	if o.Project == "" {
		o.Project = "global"
	}
	if !o.Uninstall && !o.Print && o.Available == AvailabilityUnknown {
		return errors.New(
			"local: persistent settings would name an address that stops answering when\n" +
				"  the cache goes idle, which fails npm and pip rather than slowing them.\n" +
				"  Install socket activation first, or pass -anyway to accept it")
	}

	files := persistFiles(home, o)
	if o.Print {
		for _, file := range files {
			fmt.Fprintf(out, "# %s\n%s\n", file.path, file.content)
		}
		return nil
	}

	var changed []string
	for _, file := range files {
		action, err := applyManagedFile(file, o.Uninstall, o.DryRun)
		if err != nil {
			return err
		}
		if action != "" {
			changed = append(changed, action)
			fmt.Fprintf(out, "+ %s\n", action)
		}
	}
	if len(changed) == 0 {
		fmt.Fprintln(out, "pkgcache: nothing to change")
		return nil
	}
	if o.DryRun {
		fmt.Fprintln(out, "\nNothing was changed.")
		return nil
	}
	if o.Uninstall {
		if o.DataDir != "" {
			_ = os.Remove(persistPath(o.DataDir))
		}
		fmt.Fprintln(out, "\npkgcache: persistent settings removed")
		return nil
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.path)
	}
	if err := writePersisted(o.DataDir, Persisted{
		Project: o.Project, Files: paths, BaseURL: o.BaseURL,
	}); err != nil {
		return err
	}
	fmt.Fprintln(out, "\npkgcache: settings installed for this user")
	fmt.Fprintln(out, "New shells use the cache. Remove them with `pkgcache persist -uninstall`.")
	return nil
}

// persistFiles is every file persist owns, and is the single place the layout is
// described.
func persistFiles(home string, o PersistOptions) []managedFile {
	base := strings.TrimRight(o.BaseURL, "/")
	projectBase := base + "/" + o.Project
	files := []managedFile{
		{
			path:    filepath.Join(home, ".npmrc"),
			content: fmt.Sprintf("registry=%s/npm/\n", projectBase),
		},
		{
			path:    filepath.Join(home, ".config", "pip", "pip.conf"),
			content: fmt.Sprintf("[global]\nindex-url = %s/pypi/root/pypi/+simple/\n", projectBase),
		},
		{
			path:    filepath.Join(home, ".config", "uv", "uv.toml"),
			content: fmt.Sprintf("[[index]]\nurl = \"%s/pypi/root/pypi/+simple/\"\ndefault = true\n", projectBase),
		},
	}
	if len(o.GitHosts) > 0 {
		var git bytes.Buffer
		for _, host := range o.GitHosts {
			fmt.Fprintf(&git, "[url \"%s/git/%s/\"]\n\tinsteadOf = https://%s/\n",
				projectBase, host, host)
		}
		files = append(files, managedFile{
			path:    filepath.Join(home, ".gitconfig"),
			content: git.String(),
		})
	}
	return files
}

// applyManagedFile adds or removes this tool's fenced block, and reports what it did.
//
// Fenced rather than whole-file, because every one of these is a file the user may
// already have their own settings in. Removing a block leaves the rest exactly as it
// was, which is the property that makes uninstall safe to run without reading it first.
func applyManagedFile(file managedFile, uninstall, dryRun bool) (string, error) {
	existing, err := os.ReadFile(file.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("local: read %s: %w", file.path, err)
	}
	current := string(existing)
	stripped, had := removeBlock(current)

	if uninstall {
		if !had {
			return "", nil
		}
		if dryRun {
			return "remove pkgcache settings from " + file.path, nil
		}
		if strings.TrimSpace(stripped) == "" {
			// The file existed only for us. Removing it is tidier than leaving an empty
			// one somebody later wonders about.
			if err := os.Remove(file.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			return "remove " + file.path, nil
		}
		if err := writeFilePreservingMode(file.path, stripped); err != nil {
			return "", err
		}
		return "remove pkgcache settings from " + file.path, nil
	}

	block := beginMarker + "\n" + strings.TrimRight(file.content, "\n") + "\n" + endMarker + "\n"
	updated := stripped
	if updated != "" && !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	updated += block
	if updated == current {
		return "", nil
	}
	action := "add pkgcache settings to " + file.path
	if !had && strings.TrimSpace(stripped) == "" {
		action = "write " + file.path
	}
	if dryRun {
		return action, nil
	}
	if err := os.MkdirAll(filepath.Dir(file.path), 0o755); err != nil {
		return "", fmt.Errorf("local: create %s: %w", filepath.Dir(file.path), err)
	}
	if err := writeFilePreservingMode(file.path, updated); err != nil {
		return "", err
	}
	return action, nil
}

// removeBlock strips a previously written fenced region, reporting whether one existed.
func removeBlock(content string) (string, bool) {
	start := strings.Index(content, beginMarker)
	if start < 0 {
		return content, false
	}
	end := strings.Index(content[start:], endMarker)
	if end < 0 {
		// An unterminated block: everything from the marker is ours. This happens when
		// somebody edits the file and deletes the closing line, and guessing "nothing is
		// ours" would leave settings nobody can remove.
		return strings.TrimRight(content[:start], "\n"), true
	}
	tail := content[start+end+len(endMarker):]
	head := strings.TrimRight(content[:start], "\n")
	tail = strings.TrimLeft(tail, "\n")
	switch {
	case head == "":
		return tail, true
	case tail == "":
		return head + "\n", true
	default:
		return head + "\n" + tail, true
	}
}

func writeFilePreservingMode(path, content string) error {
	mode := fs.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, []byte(content), mode)
}

// InstallService installs the unit that keeps the cache's address answerable.
//
// systemd user socket activation on Linux: the socket unit binds the port and holds it
// open, the service unit starts on the first connection and inherits the descriptor,
// and the daemon may still exit when idle. That is what makes a .npmrc naming a fixed
// port correct rather than hopeful.
//
// The macOS equivalent is a launchd agent with the same shape, and is written but
// unverified. Windows has no equivalent that does not amount to a resident process.
func InstallService(executable, dataDir string, uninstall bool, out io.Writer) (Availability, error) {
	if out == nil {
		out = os.Stdout
	}
	switch runtime.GOOS {
	case "linux":
		return installSystemdSocket(executable, dataDir, uninstall, out)
	case "darwin":
		return installLaunchdAgent(executable, dataDir, uninstall, out)
	default:
		return AvailabilityUnknown, fmt.Errorf(
			"local: no socket activation on %s; run `pkgcache serve` yourself, or accept "+
				"that the address can stop answering with -anyway", runtime.GOOS)
	}
}

func installSystemdSocket(
	executable, dataDir string, uninstall bool, out io.Writer,
) (Availability, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return AvailabilityUnknown, err
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	socketPath := filepath.Join(unitDir, "pkgcache.socket")
	servicePath := filepath.Join(unitDir, "pkgcache.service")

	if uninstall {
		_ = runSystemctl("--user", "disable", "--now", "pkgcache.socket")
		_ = runSystemctl("--user", "stop", "pkgcache.service")
		for _, path := range []string{socketPath, servicePath} {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return AvailabilityUnknown, err
			}
		}
		_ = runSystemctl("--user", "daemon-reload")
		fmt.Fprintln(out, "pkgcache: removed the user socket unit")
		return AvailabilityUnknown, nil
	}

	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return AvailabilityUnknown, err
	}
	socketUnit := fmt.Sprintf(`[Unit]
Description=pkgcache socket

[Socket]
ListenStream=127.0.0.1:%d
Accept=no

[Install]
WantedBy=sockets.target
`, defaultPort())
	serviceUnit := fmt.Sprintf(`[Unit]
Description=pkgcache
Requires=pkgcache.socket

[Service]
Type=simple
ExecStart=%s serve -data-dir %s
Environment=PKGCACHE_DATA_DIR=%s
`, executable, dataDir, dataDir)

	if err := os.WriteFile(socketPath, []byte(socketUnit), 0o644); err != nil {
		return AvailabilityUnknown, err
	}
	if err := os.WriteFile(servicePath, []byte(serviceUnit), 0o644); err != nil {
		return AvailabilityUnknown, err
	}
	if err := runSystemctl("--user", "daemon-reload"); err != nil {
		return AvailabilityUnknown, fmt.Errorf("local: systemctl --user daemon-reload: %w", err)
	}
	if err := runSystemctl("--user", "enable", "--now", "pkgcache.socket"); err != nil {
		return AvailabilityUnknown, fmt.Errorf("local: enable pkgcache.socket: %w", err)
	}
	fmt.Fprintf(out, "pkgcache: installed %s\n", socketPath)
	fmt.Fprintln(out,
		"The port is now held open by systemd; the cache starts on the first connection\n"+
			"and still exits when idle.")
	return AvailabilitySocket, nil
}

// installLaunchdAgent is the macOS equivalent. Written to the same contract and NOT
// verified on a macOS host.
func installLaunchdAgent(
	executable, dataDir string, uninstall bool, out io.Writer,
) (Availability, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return AvailabilityUnknown, err
	}
	path := filepath.Join(home, "Library", "LaunchAgents", "dev.pkgcache.plist")
	if uninstall {
		_ = exec.Command("launchctl", "unload", path).Run() // #nosec G204 -- fixed path
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return AvailabilityUnknown, err
		}
		fmt.Fprintln(out, "pkgcache: removed the launch agent")
		return AvailabilityUnknown, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return AvailabilityUnknown, err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.pkgcache</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>serve</string>
    <string>-data-dir</string><string>%s</string></array>
  <key>Sockets</key>
  <dict><key>Listeners</key>
    <dict><key>SockNodeName</key><string>127.0.0.1</string>
      <key>SockServiceName</key><string>%d</string></dict></dict>
  <key>RunAtLoad</key><false/>
</dict>
</plist>
`, executable, dataDir, defaultPort())
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return AvailabilityUnknown, err
	}
	// #nosec G204 -- a fixed program and a path this function just wrote.
	if err := exec.Command("launchctl", "load", path).Run(); err != nil {
		return AvailabilityUnknown, fmt.Errorf("local: launchctl load: %w", err)
	}
	fmt.Fprintf(out, "pkgcache: installed %s (unverified on macOS)\n", path)
	return AvailabilitySocket, nil
}

// defaultPort is the port the units bind. It is the fixed one on purpose: a unit that
// bound an ephemeral port would defeat the whole reason persistent settings exist.
func defaultPort() int { return config.LocalPort }

func runSystemctl(args ...string) error {
	// #nosec G204 -- every argument is a literal in this file.
	return exec.Command("systemctl", args...).Run()
}
