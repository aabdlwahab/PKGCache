//go:build darwin

package tray

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The macOS menu bar, through a helper.
//
// NSStatusItem is Cocoa, and there is no pure-Go path to it. The alternative was cgo for
// darwin, which would have cost the project its "one toolchain, one host" release for one
// platform — so the menu bar item is a small signed helper built on the macOS runner the
// client release already uses, and everything that decides anything stays here.
//
// The contract between them is deliberately boring: newline-delimited text.
//
//	Go  → helper   state {"running":true,"full":false,...}   whenever it changes
//	               menu  [{"id":0,"label":"…","enabled":true},…]
//	               quit
//	helper → Go    click 3                                   the id that was chosen
//
// One process, two pipes, no IPC to debug. The helper knows how to draw a menu and nothing
// about caches; this file knows about caches and nothing about AppKit. If the helper is
// missing the tray is simply unsupported, and the caller falls back to the window.

// helperName is the binary shipped beside pkgcache.
const helperName = "pkgcache-menubar"

func run(ctx context.Context, o Options) error {
	path, err := helperPath()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// #nosec G204 -- the path is resolved beside this binary or on PATH, never from input.
	cmd := exec.CommandContext(ctx, path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("tray: start %s: %w", path, err)
	}
	note(o, "pkgcache: in the menu bar")

	state := o.Read()
	send := func(line string) error {
		_, writeErr := stdin.Write([]byte(line + "\n"))
		return writeErr
	}
	push := func() {
		payload, marshalErr := json.Marshal(state)
		if marshalErr != nil {
			return
		}
		_ = send("state " + string(payload))
		if menu, menuErr := json.Marshal(menuFor(state)); menuErr == nil {
			_ = send("menu " + string(menu))
		}
	}
	push()

	// Clicks arrive on the helper's stdout; the ticker refreshes what it shows. Both are
	// funnelled through one goroutine so the state is never touched from two.
	clicks := make(chan Action, 4)
	go func() {
		defer close(clicks)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) != 2 || fields[0] != "click" {
				continue
			}
			var id int
			if _, scanErr := fmt.Sscanf(fields[1], "%d", &id); scanErr != nil {
				continue
			}
			clicks <- Action(id)
		}
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = send("quit")
			_ = cmd.Wait()
			return nil
		case action, open := <-clicks:
			if !open {
				// The helper exited — somebody chose Quit from the menu, or it crashed.
				// Either way the icon is gone and there is nothing left to drive.
				return cmd.Wait()
			}
			if action == ActionQuit {
				_ = send("quit")
				_ = cmd.Wait()
				return nil
			}
			// The window belongs to the helper already running, so it is asked rather than
			// a second one started. No browser is involved on this platform at any point.
			if action == ActionWidget && o.Window != nil {
				if url := o.Window(); url != "" {
					_ = send("open " + url)
					state = o.Read()
					push()
					continue
				}
			}
			if action.Enabled(state) {
				if err := o.Do(action); err != nil {
					note(o, "pkgcache: %v", err)
				}
			}
			state = o.Read()
			push()
		case <-ticker.C:
			next := o.Read()
			if next == state {
				continue
			}
			state = next
			push()
		}
	}
}

// menuItem is what the helper draws.
type menuItem struct {
	ID        int    `json:"id"`
	Label     string `json:"label"`
	Enabled   bool   `json:"enabled"`
	Separator bool   `json:"separator,omitempty"`
}

func menuFor(state State) []menuItem {
	separators := Separators()
	items := make([]menuItem, 0, len(Menu)+2)
	for _, action := range Menu {
		if separators[action] {
			items = append(items, menuItem{Separator: true})
		}
		items = append(items, menuItem{
			ID: int(action), Label: action.Label(state), Enabled: action.Enabled(state),
		})
	}
	return items
}

// HelperEnvVar names the helper explicitly, for the one caller that already knows.
//
// When the menu bar app is opened from Finder it re-executes pkgcache, and it is itself the
// helper pkgcache is about to go looking for. Searching would be absurd — and worse than
// absurd, because an app launched from Finder inherits a PATH of /usr/bin:/bin:/usr/sbin:
// /sbin, which contains neither /usr/local/bin nor the inside of an app bundle. Without
// this the search fails, the error goes to a stderr nobody is reading, and clicking the
// icon does nothing at all.
const HelperEnvVar = "PKGCACHE_MENUBAR"

// helperPath prefers the helper beside this binary, which is what a release ships and what
// somebody who copied two files onto a disconnected Mac will have.
func helperPath() (string, error) {
	if named := strings.TrimSpace(os.Getenv(HelperEnvVar)); named != "" {
		if info, err := os.Stat(named); err == nil && !info.IsDir() {
			return named, nil
		}
		return "", fmt.Errorf("%w: %s names %s, which is not a file",
			ErrUnsupported, HelperEnvVar, named)
	}
	if self, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(self), helperName)
		if info, statErr := os.Stat(beside); statErr == nil && !info.IsDir() {
			return beside, nil
		}
	}
	found, err := exec.LookPath(helperName)
	if err != nil {
		return "", fmt.Errorf(
			"%w: %s is not installed beside pkgcache or on PATH.\n"+
				"  The menu bar item is a separate signed helper on macOS — NSStatusItem is\n"+
				"  Cocoa, and pkgcache itself is built without cgo. `pkgcache widget` opens\n"+
				"  the same window without it: %v",
			ErrUnsupported, helperName, errors.Unwrap(err))
	}
	return found, nil
}
