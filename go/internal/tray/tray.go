// Package tray is what the status bar shows, and nothing about how.
//
// One icon, a tooltip that says whether the cache is working, and a short menu. Every
// decision here is platform-free — what an item is called, whether it can be chosen, what
// the tooltip says — and the drawing belongs to whatever is displaying it. Today that is
// pkgcache-app, through Wails.
//
// It used to be three implementations as well: StatusNotifierItem over D-Bus on Linux,
// Shell_NotifyIcon on Windows, and a Swift helper talking newline-delimited JSON over a
// pipe on macOS. About 1,100 lines of it, three ways to be wrong about the same menu. The
// app renders this table instead, and what survived is the part that was always right.
//
// The rule the whole package still obeys: none of this keeps the daemon alive. A status
// icon that held a cache up would quietly remove the idle exit that makes this polite to
// leave installed, so Enabled greys the items that need a running daemon rather than
// offering to start one.
package tray

import "fmt"

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
