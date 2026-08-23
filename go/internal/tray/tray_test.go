package tray

import (
	"strings"
	"testing"
)

// The tooltip is the whole of what a status bar says without being clicked, so what it says
// in each state is worth pinning. Most important: an asleep cache must not read as a broken
// one, and a full one must be unmissable.
func TestTooltipSaysTheStateFirst(t *testing.T) {
	cases := []struct {
		name  string
		state State
		want  string
	}{
		{"asleep", State{Served: -1}, "asleep"},
		{
			"full", State{Running: true, Full: true, Project: "work", Served: -1},
			"FULL",
		},
		{
			"caching", State{Running: true, Project: "work", Used: 1 << 20, Limit: 1 << 30, Served: 0.64},
			"caching work",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Tooltip(c.state)
			if !strings.Contains(got, c.want) {
				t.Fatalf("Tooltip = %q, want something containing %q", got, c.want)
			}
			if strings.Count(got, "\n") != 0 {
				t.Errorf("a tooltip is one line: %q", got)
			}
		})
	}
	// A cache nobody has asked anything of has no rate, and printing 0% would be a claim
	// about it rather than an absence of one.
	quiet := Tooltip(State{Running: true, Project: "global", Served: -1})
	if strings.Contains(quiet, "%") {
		t.Errorf("an unused cache reports a rate: %q", quiet)
	}
}

// Every item that needs a running cache is greyed rather than starting one, because a status
// icon that woke a daemon would undo the idle exit. Opening the window is the exception: it
// is somebody asking to look.
func TestOnlyOpeningTheWindowWorksWhileAsleep(t *testing.T) {
	asleep := State{}
	for _, action := range Menu {
		enabled := action.Enabled(asleep)
		switch action {
		case ActionWidget, ActionConsole, ActionQuit:
			if !enabled {
				t.Errorf("%v is greyed while asleep and should not be", action.Label(asleep))
			}
		default:
			if enabled {
				t.Errorf("%v is live while the cache is asleep", action.Label(asleep))
			}
		}
	}
	running := State{Running: true}
	for _, action := range Menu {
		if !action.Enabled(running) {
			t.Errorf("%v is greyed on a running cache", action.Label(running))
		}
	}
}

// A label that changes with the state is the only way a menu can explain why an item is
// greyed, so the one that does is worth asserting.
func TestStopSaysWhyItIsGreyed(t *testing.T) {
	if label := ActionStop.Label(State{}); !strings.Contains(label, "asleep") {
		t.Errorf("stop reads %q while asleep", label)
	}
	if label := ActionStop.Label(State{Running: true}); !strings.Contains(label, "Stop") {
		t.Errorf("stop reads %q on a running cache", label)
	}
}

func TestEveryMenuItemHasALabel(t *testing.T) {
	for _, action := range Menu {
		for _, state := range []State{{}, {Running: true}, {Running: true, Full: true}} {
			if action.Label(state) == "" {
				t.Errorf("action %d has no label in %+v", action, state)
			}
		}
	}
	// Separators are drawn before items that begin a group, and every one of them has to be
	// an item that exists — a separator before nothing is a line at the bottom of a menu.
	known := map[Action]bool{}
	for _, action := range Menu {
		known[action] = true
	}
	for action := range Separators() {
		if !known[action] {
			t.Errorf("a separator is drawn before action %d, which is not in the menu", action)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		-1: "no limit", 0: "0 B", 900: "900 B", 1 << 10: "1.0 KiB",
		1 << 20: "1.0 MiB", 25 << 30: "25.0 GiB",
	}
	for input, want := range cases {
		if got := FormatBytes(input); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", input, got, want)
		}
	}
}

func TestRunRefusesWithoutItsCallbacks(t *testing.T) {
	if err := Run(nil, Options{}); err == nil {
		t.Fatal("Run accepted options with no Read or Do")
	}
}
