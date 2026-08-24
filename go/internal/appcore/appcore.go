// Package appcore is the desktop app's brain, with no toolkit in it.
//
// The app is a window and a status bar item, and neither of those is where the thinking
// happens: reading what the cache is doing, deciding what a menu item should say, and
// noticing that something has changed enough to be worth interrupting somebody over are
// all decisions this package makes and hands over already made.
//
// It is separate from the toolkit for the reason every part of this client is separate
// from its toolkit — the toolkit cannot be built everywhere, and none of this needs it.
// A test can drive the whole of the app's behaviour on a machine with no display, no GTK
// and no status bar, which is exactly the machine this was written on.
//
// The rule it inherits from the tray it replaces, and the one thing here that is not
// negotiable: the app never keeps the daemon alive. Everything reads state without
// starting anything, and the single exception — opening the window, which is somebody
// asking to look at the cache — is marked where it happens.
package appcore

import (
	"context"
	"fmt"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
	"github.com/aabdlwahab/PKGCache/internal/tray"
)

// Core is the app's view of one machine's cache.
//
// Not safe for concurrent use. The app drives it from one goroutine — the same one that
// owns the toolkit's message loop — and adding a mutex would only hide a caller that had
// stopped doing that.
type Core struct {
	snap *config.Snapshot
	// daemon is the pkgcache binary to start when something needs a cache that is not
	// running. Empty means "re-execute whoever is calling", which is local.Ensure's
	// default and is only correct for pkgcache itself. See UseDaemon.
	daemon string
	// seen is the last state Observe was given, and the whole of the memory this keeps.
	// Notifications are transitions, so something has to remember the previous edge.
	seen     tray.State
	observed bool
}

// New builds a Core over a loaded configuration.
func New(snap *config.Snapshot) *Core { return &Core{snap: snap} }

// UseDaemon names the pkgcache binary to start when the cache is not running.
//
// pkgcache itself leaves this alone, and local.Ensure re-executes the calling binary,
// which is exactly right there: pkgcache starting pkgcache cannot produce a client and a
// daemon from different builds.
//
// The app is a different binary, and that default is actively wrong for it — re-executing
// pkgcache-app as a daemon starts a second app, not a cache. So the app has to say where
// the daemon is, and this is where it says it.
func (c *Core) UseDaemon(path string) { c.daemon = path }

// ensure reaches the daemon, starting one only when allowed to.
//
// One place, so no caller can forget the executable and silently get the wrong default.
func (c *Core) ensure(ctx context.Context, start bool) (local.State, error) {
	return local.Ensure(ctx, local.EnsureOptions{
		Snapshot: c.snap, Executable: c.daemon, NoStart: !start,
		// Set exactly when a daemon binary was named, which is the app's case and not
		// the CLI's. Without it the version check treats every poll as an upgrade and
		// stops the cache two seconds after starting it.
		DifferentBinary: c.daemon != "",
	})
}

// State reports what the icon, the tooltip and the window title should say.
//
// Two sources, and which one answers is the point: a running daemon is asked over the
// API, and a cache that has gone idle is read off disk from the files it published on its
// way out. That is what lets the app say "asleep, 8.3 GiB held" rather than going blank,
// and it is why this never starts anything.
func (c *Core) State(ctx context.Context) tray.State {
	state := tray.State{Project: local.CurrentProject(c.snap.DataDir), Served: -1, Limit: -1}
	if budget, err := local.ReadBudget(c.snap.DataDir); err == nil {
		state.Limit = budget.LimitBytes
	}
	if usage, _, found := local.ReadUsage(c.snap.DataDir); found {
		state.Used, state.Full, state.Reason = usage.Bytes, usage.Full, usage.Reason
	}

	daemon, err := c.ensure(ctx, false)
	if err != nil {
		return state
	}
	state.Running = true
	// The live figures replace the published ones where they are available: usage.json is
	// whatever the daemon last measured, which may be minutes old.
	reports, err := local.ProjectReports(ctx, daemon)
	if err != nil {
		return state
	}
	for _, report := range reports {
		if report.Project != state.Project {
			continue
		}
		if rate, known := report.Served(); known {
			state.Served = rate
		}
	}
	return state
}

// Do performs a menu choice that acts on the cache.
//
// The window actions are not here, and the omission is the seam this package exists to
// draw: showing a page means a native window to the app and a browser to the CLI, and
// neither of them is a decision about caches. Callers handle those two with WindowURL.
func (c *Core) Do(ctx context.Context, action tray.Action) error {
	switch action {
	case tray.ActionOffline:
		daemon, err := c.ensure(ctx, false)
		if err != nil {
			return err
		}
		return local.ToggleOffline(ctx, daemon, local.CurrentProject(c.snap.DataDir))

	case tray.ActionPrune:
		daemon, err := c.ensure(ctx, false)
		if err != nil {
			return err
		}
		return local.CollectVia(ctx, daemon)

	case tray.ActionStop:
		_, err := local.Stop(ctx, c.snap.DataDir, 15*time.Second)
		return err
	}
	return nil
}

// WindowURL is the address the window should show, starting the cache if it is not
// running.
//
// The one thing here allowed to start a daemon, because it is somebody asking to look at
// the cache. Every other path in this package passes NoStart.
func (c *Core) WindowURL(ctx context.Context, action tray.Action) (string, error) {
	path := "/widget"
	if action == tray.ActionConsole {
		path = "/console"
	}
	daemon, err := c.ensure(ctx, true)
	if err != nil {
		return "", err
	}
	return daemon.BaseURL() + path, nil
}

// FallbackURL is the address to show when the daemon cannot be reached at all.
//
// Printing an address somebody can try themselves beats a window that says only that
// something went wrong, and over SSH or in a container it is the entire useful answer.
func (c *Core) FallbackURL() string { return c.snap.LocalBaseURL() + "/widget" }

// Notification is something worth interrupting somebody over.
type Notification struct {
	Title string
	Body  string
}

// Observe records a new state and reports what is worth saying about the change.
//
// Transitions only, and deliberately very few of them. A status bar item that spoke every
// time anything moved would be muted within a day, and a muted notification is worth less
// than none because it is also a promise the person has stopped believing.
//
// So: the cache filling up, and the cache recovering. Filling up is the condition this
// whole surface exists to make visible — it is the one state where the cache is still
// answering, so nothing appears broken, while it has quietly stopped storing anything new.
// Today that fact lives in a tooltip nobody hovers.
//
// What is deliberately *not* here: the daemon starting and stopping. It exits when nothing
// has used it for a while and starts again on the next command, perhaps dozens of times a
// day. That is the design working, not news.
//
// The first call reports a full cache even though there is no previous edge to compare
// against, and that is intended rather than an accident of the zero value: an app that
// opens at login onto an already-full cache should say so. It will say so again tomorrow,
// which is correct — the problem is still there and still needs a person.
func (c *Core) Observe(next tray.State) []Notification {
	previous, had := c.seen, c.observed
	c.seen, c.observed = next, true

	var out []Notification
	switch {
	case next.Full && (!had || !previous.Full):
		body := "Serving from the cache, but no longer storing anything new."
		if next.Reason != "" {
			body = next.Reason
		}
		out = append(out, Notification{Title: "pkgcache is full", Body: body})

	case had && previous.Full && !next.Full:
		out = append(out, Notification{
			Title: "pkgcache has room again",
			Body:  "Storing new downloads once more.",
		})
	}
	return out
}

// Seen is the last state passed to Observe, and whether there has been one.
func (c *Core) Seen() (tray.State, bool) { return c.seen, c.observed }

// Title is the window's title bar.
//
// It carries the project because somebody with two checkouts open has two of these, and
// a pair of identically titled windows is a small daily annoyance that costs one line here.
func Title(state tray.State) string {
	if state.Project == "" {
		return "pkgcache"
	}
	return fmt.Sprintf("pkgcache — %s", state.Project)
}
