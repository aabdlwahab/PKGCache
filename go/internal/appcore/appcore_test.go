package appcore

import (
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/tray"
)

// The notification rules, which are the part of this package a person actually feels.
// Every case here is one somebody would complain about if it were wrong: silence when the
// cache stops storing, or chatter when nothing happened.

func TestObserveAnnouncesFullOnce(t *testing.T) {
	core := &Core{}
	full := tray.State{Running: true, Full: true, Reason: "disk is within 2 GiB of the floor"}

	first := core.Observe(full)
	if len(first) != 1 {
		t.Fatalf("a cache that has filled up should say so once: got %d notifications", len(first))
	}
	if first[0].Body != full.Reason {
		t.Errorf("the reason is the useful half of the message\n got: %q\nwant: %q",
			first[0].Body, full.Reason)
	}

	// Still full a second later. Saying so again would be the fastest way to be muted.
	if again := core.Observe(full); len(again) != 0 {
		t.Errorf("a cache that is still full is not news: got %d notifications", len(again))
	}
}

func TestObserveAnnouncesRecovery(t *testing.T) {
	core := &Core{}
	core.Observe(tray.State{Running: true, Full: true})

	freed := core.Observe(tray.State{Running: true, Full: false})
	if len(freed) != 1 {
		t.Fatalf("reclaiming space is worth one line: got %d notifications", len(freed))
	}
	if freed[0].Title == "" {
		t.Error("a notification with no title is not a notification")
	}
}

func TestObserveIsSilentAboutTheDaemonSleeping(t *testing.T) {
	// The daemon exits when nothing has used it and starts again on the next command,
	// perhaps dozens of times a day. That is the idle timeout working, not an event.
	core := &Core{}
	core.Observe(tray.State{Running: true})

	for _, state := range []tray.State{
		{Running: false},
		{Running: true},
		{Running: false, Used: 9 << 30},
	} {
		if got := core.Observe(state); len(got) != 0 {
			t.Errorf("running=%v produced %d notifications; sleeping is not news",
				state.Running, len(got))
		}
	}
}

func TestObserveSpeaksOnAFullFirstSight(t *testing.T) {
	// An app opening at login onto an already-full cache has no previous edge to compare
	// against, and still has something worth saying.
	core := &Core{}
	if got := core.Observe(tray.State{Running: true, Full: true}); len(got) != 1 {
		t.Fatalf("a cache that is already full when the app opens should say so: got %d", len(got))
	}
}

func TestObserveRemembersWhatItSaw(t *testing.T) {
	core := &Core{}
	if _, had := core.Seen(); had {
		t.Error("a Core that has observed nothing should say so")
	}
	want := tray.State{Running: true, Project: "web", Used: 42}
	core.Observe(want)
	got, had := core.Seen()
	if !had || got != want {
		t.Errorf("Seen() = %+v, %v; want %+v, true", got, had, want)
	}
}

func TestFullWithNoReasonStillExplainsItself(t *testing.T) {
	core := &Core{}
	got := core.Observe(tray.State{Running: true, Full: true})
	if len(got) != 1 || got[0].Body == "" {
		t.Fatal("a full cache with no recorded reason still needs a body somebody can read")
	}
}

func TestTitleCarriesTheProject(t *testing.T) {
	// Two checkouts means two of these windows, and two identical title bars is a small
	// daily annoyance that costs one line to avoid.
	if got := Title(tray.State{Project: "web"}); got != "pkgcache — web" {
		t.Errorf("Title = %q, want %q", got, "pkgcache — web")
	}
	if got := Title(tray.State{}); got != "pkgcache" {
		t.Errorf("with no project, Title = %q, want %q", got, "pkgcache")
	}
}

// The two window items are two pages. They were one for a while — the app sent both menu
// clicks down the same path and resolved the widget's address either way — and the symptom
// was a console item that opened the panel. A caller asking for the console gets the
// console.
func TestTheConsoleAndTheWidgetAreDifferentPages(t *testing.T) {
	widget := WindowPath(tray.ActionWidget)
	console := WindowPath(tray.ActionConsole)
	if widget != "/widget" || console != "/console" {
		t.Fatalf("widget = %q, console = %q", widget, console)
	}
	// Every other action is a cache operation rather than a page, and the window they
	// would open if one somehow asked is the panel, not the operator console.
	for _, action := range []tray.Action{tray.ActionOffline, tray.ActionPrune, tray.ActionStop} {
		if got := WindowPath(action); got != "/widget" {
			t.Errorf("WindowPath(%v) = %q", action, got)
		}
	}
}
