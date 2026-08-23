// Package tray puts pkgcache in the status bar.
//
// The window is the product; this is how somebody keeps it without keeping a browser tab.
// It shows one icon, a tooltip that says whether the cache is working, and a short menu.
// Everything it can do, `pkgcache` can already do from the command line — the tray is a
// place to click, not a second implementation.
//
// Three platforms, three mechanisms, one model:
//
//	linux    StatusNotifierItem over D-Bus. No cgo, no X11, and the modern protocol; GNOME
//	         needs a shell extension for it, which is why the browser window has to stand
//	         on its own and does.
//	windows  Shell_NotifyIcon through golang.org/x/sys/windows, which is already a
//	         dependency. No cgo either.
//	darwin   a small signed helper. NSStatusItem is Cocoa and there is no pure-Go path, and
//	         accepting cgo for one platform would cost the whole project its "one toolchain,
//	         one host" release. The helper is built and notarized on the macOS runner the
//	         client release already uses.
//
// The rule the whole package obeys: the tray never keeps the daemon alive. It reads the
// state files when nothing is running and says "asleep", because a status icon that held a
// cache up would quietly remove the idle exit that makes this polite to leave installed.
package tray

import (
	"context"
	"errors"
	"fmt"
)

// ErrUnsupported reports that this platform has no status bar this package can reach.
var ErrUnsupported = errors.New("tray: no status bar on this platform")

// State is what the icon and its tooltip say.
//
// The tags are load-bearing, not decoration: on macOS this struct crosses a pipe into a
// helper written in Swift, so its field names are a wire format. Go's default is the Go
// name — capitalised — which decoded to nothing at all on the far side, leaving an icon
// permanently "asleep" that never turned red. See wire_test.go.
type State struct {
	// Running reports whether a daemon is answering. False is "asleep", which is normal
	// and not a fault: the cache exits when nothing has used it.
	Running bool `json:"running"`
	// Full is the condition the tray exists to make visible — serving, and no longer
	// storing.
	Full   bool   `json:"full"`
	Reason string `json:"reason"`
	// Project is the one commands default to.
	Project string `json:"project"`
	// Used and Limit are bytes. Limit is negative when the user chose no cap.
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
	// Served is the share of requests answered from this machine, or -1 when nothing has
	// been asked for yet. A rate of zero and no requests are different facts.
	Served float64 `json:"served"`
}

// Action is one menu item.
type Action int

// The menu, in order. Kept short deliberately: a status bar menu is not a control panel,
// and everything longer than this belongs in the window it opens.
const (
	// ActionWidget opens the window. The default click.
	ActionWidget Action = iota
	ActionConsole
	ActionOffline
	ActionPrune
	ActionStop
	ActionQuit
)

// Label is what the menu shows for an action, given the current state.
func (a Action) Label(state State) string {
	switch a {
	case ActionWidget:
		return "Open pkgcache"
	case ActionConsole:
		return "Open the console"
	case ActionOffline:
		return "Serve from cache only"
	case ActionPrune:
		return "Reclaim space"
	case ActionStop:
		if !state.Running {
			return "Cache is asleep"
		}
		return "Stop the cache"
	case ActionQuit:
		return "Quit this icon"
	}
	return ""
}

// Menu is the order items appear in, with separators where a group ends.
var Menu = []Action{ActionWidget, ActionConsole, ActionOffline, ActionPrune, ActionStop, ActionQuit}

// Separators returns the actions a separator is drawn *before*.
func Separators() map[Action]bool {
	return map[Action]bool{ActionOffline: true, ActionStop: true}
}

// Enabled reports whether an item can be chosen in this state. A greyed item that says
// why is better than a live one that fails.
func (a Action) Enabled(state State) bool {
	switch a {
	case ActionOffline, ActionPrune, ActionStop:
		return state.Running
	}
	return true
}

// Options is what the platform loop needs from the caller.
type Options struct {
	// Read reports the current state. Called on a timer and after every action, so it must
	// be cheap and must never start a daemon.
	Read func() State
	// Do performs a menu action.
	Do func(Action) error
	// Notes receives anything worth saying to a person who started the tray from a
	// terminal. Nil discards them.
	Notes func(string)
	// Window, when set, returns the address of the page the open action should show, or ""
	// if it cannot be reached.
	//
	// It exists for macOS, where the helper drawing the icon also owns the window: asking
	// it to open one in the process that is already running beats launching a second helper
	// beside it, which would put a duplicate in the Dock. Every other platform leaves this
	// nil and the action goes through Do like the rest.
	Window func() string
}

// Tooltip is the sentence the icon carries. One line, because that is all a status bar
// gives, and the most important word first.
func Tooltip(state State) string {
	if !state.Running {
		return "pkgcache — asleep"
	}
	if state.Full {
		return "pkgcache — FULL: serving, not storing"
	}
	line := "pkgcache — caching " + state.Project
	if state.Limit > 0 {
		line += fmt.Sprintf(" · %s of %s", FormatBytes(state.Used), FormatBytes(state.Limit))
	}
	if state.Served >= 0 {
		line += fmt.Sprintf(" · %.0f%% served here", state.Served*100)
	}
	return line
}

// FormatBytes is the short form a tooltip has room for.
//
// Its own rather than the one in internal/local: importing that would pull the whole
// daemon, its lock and its store into a package whose entire job is to draw an icon.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < 0 {
		return "no limit"
	}
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, exponent := float64(n)/unit, 0
	for value >= unit && exponent < 4 {
		value /= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", value, "KMGTP"[exponent])
}

// Run shows the icon until ctx is cancelled, or returns ErrUnsupported.
//
// Blocking, and it owns the calling goroutine: every platform's status bar wants a message
// loop on the thread that created the item, and pretending otherwise is how tray code
// becomes flaky.
func Run(ctx context.Context, o Options) error {
	if o.Read == nil || o.Do == nil {
		return errors.New("tray: Read and Do are required")
	}
	return run(ctx, o)
}

func note(o Options, format string, args ...any) {
	if o.Notes != nil {
		o.Notes(fmt.Sprintf(format, args...))
	}
}
