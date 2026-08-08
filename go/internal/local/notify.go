package local

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Telling somebody the cache stopped caching is most of the feature.
//
// A cache that silently degrades is worse than one that fails, because the failure at
// least gets investigated. So the condition is reported four ways, and none of them is
// the only one: a person running a command sees it on stderr, a person looking sees it
// in `pkgcache status`, a person working sees a desktop notification, and a pipeline
// with nobody watching sees a non-zero exit.

// usagePath is where the daemon publishes what it last measured, so a client can
// report the cache's state without opening the store or walking it.
func usagePath(dataDir string) string { return filepath.Join(dataDir, "usage.json") }

type publishedUsage struct {
	Bytes     int64     `json:"bytes"`
	Objects   int64     `json:"objects"`
	FreeBytes int64     `json:"free_bytes"`
	Limit     int64     `json:"limit_bytes"`
	MinFree   int64     `json:"min_free_bytes"`
	Full      bool      `json:"full"`
	Reason    string    `json:"reason,omitempty"`
	Sampled   time.Time `json:"sampled"`
}

// PublishUsage records what the daemon last measured.
//
// Best effort by design: failing to write a status file must never fail the download
// it was measured during.
func PublishUsage(dataDir string, u Usage) {
	data, err := json.MarshalIndent(publishedUsage{
		Bytes: u.Bytes, Objects: u.Objects, FreeBytes: u.FreeBytes,
		Limit: u.Budget.LimitBytes, MinFree: u.Budget.MinFreeBytes,
		Full: u.Full, Reason: u.Reason, Sampled: time.Now(),
	}, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(usagePath(dataDir), append(data, '\n'), 0o600)
}

// ReadUsage returns what the daemon last published, and whether anything was found.
func ReadUsage(dataDir string) (Usage, time.Time, bool) {
	data, err := os.ReadFile(usagePath(dataDir))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return Usage{}, time.Time{}, false
		}
		return Usage{}, time.Time{}, false
	}
	var published publishedUsage
	if err := json.Unmarshal(data, &published); err != nil {
		return Usage{}, time.Time{}, false
	}
	return Usage{
		Bytes: published.Bytes, Objects: published.Objects, FreeBytes: published.FreeBytes,
		Budget: Budget{LimitBytes: published.Limit, MinFreeBytes: published.MinFree},
		Full:   published.Full, Reason: published.Reason,
	}, published.Sampled, true
}

// ClearUsage forgets the published measurement.
//
// Called by the commands that change the answer — prune, gc, limit — so that a cache
// somebody has just made room in does not keep reporting itself full until the next
// download happens to re-measure it.
func ClearUsage(dataDir string) { _ = os.Remove(usagePath(dataDir)) }

// FullExitCode is returned when a command succeeded but the cache stopped caching.
//
// 75 is EX_TEMPFAIL: a temporary failure, retriable once space is made. No package
// manager returns it, so "the build failed" and "the build worked but nothing was
// cached" can never be confused for one another — which matters because pkgcache
// deliberately reports the second as a non-zero status.
const FullExitCode = 75

// Notify raises a desktop notification, if this machine has a desktop.
//
// Every failure is silent on purpose. There is no desktop session in CI, over SSH, or
// in a container, and a cache that refused to serve because notify-send is missing
// would be a worse product than one that quietly skips the toast — the other three
// channels have already reported the same thing.
func Notify(title, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	name, args := notifyCommand(title, body)
	if name == "" {
		return
	}
	if _, err := exec.LookPath(name); err != nil {
		return
	}
	// #nosec G204 -- the program is one of three fixed names and the message is this
	// package's own text.
	_ = exec.CommandContext(ctx, name, args...).Run()
}

func notifyCommand(title, body string) (string, []string) {
	switch runtime.GOOS {
	case "linux":
		// No desktop session means no bus to notify on, and notify-send would block or
		// fail. Checking first keeps the common headless case free of a process spawn.
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return "", nil
		}
		return "notify-send", []string{"--app-name=pkgcache", title, body}
	case "darwin":
		// Unverified on macOS; see docs/local-cache-plan.md for what was run where.
		script := "display notification " + appleScriptString(body) +
			" with title " + appleScriptString(title)
		return "osascript", []string{"-e", script}
	case "windows":
		// Unverified on Windows. PowerShell's toast APIs need a registered application
		// id, so this uses the message box that is always available instead.
		script := "[System.Reflection.Assembly]::LoadWithPartialName('System.Windows.Forms')" +
			">$null; [System.Windows.Forms.MessageBox]::Show(" +
			powerShellString(body) + "," + powerShellString(title) + ")>$null"
		return "powershell.exe", []string{"-NoProfile", "-NonInteractive", "-Command", script}
	default:
		return "", nil
	}
}

func appleScriptString(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

func powerShellString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
