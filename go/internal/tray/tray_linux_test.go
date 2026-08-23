//go:build linux

package tray

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

// A stand-in status bar host.
//
// This is the part of the tray that was supposed to be untestable without a desktop, and
// most of it is not: StatusNotifierItem is a D-Bus protocol, and a host is a program that
// claims a well-known name and reads properties. So the test is a host — it registers the
// item, reads back what a real panel would read, and clicks a menu entry the way one does.
//
// What it does not prove is that pixels appear in a panel. Nothing short of a desktop can,
// and the icon's own drawing is asserted separately.
type fakeWatcher struct {
	registered chan string
}

func (w *fakeWatcher) RegisterStatusNotifierItem(service string) *dbus.Error {
	select {
	case w.registered <- service:
	default:
	}
	return nil
}

func (w *fakeWatcher) RegisterStatusNotifierHost(_ string) *dbus.Error { return nil }

const watcherIntrospection = `<node>
  <interface name="org.kde.StatusNotifierWatcher">
    <method name="RegisterStatusNotifierItem"><arg type="s" direction="in"/></method>
    <method name="RegisterStatusNotifierHost"><arg type="s" direction="in"/></method>
    <property name="IsStatusNotifierHostRegistered" type="b" access="read"/>
  </interface>
</node>`

// sessionBus skips the test where there is none — CI containers and SSH sessions have no
// session bus, and a tray test that failed there would be reporting the environment.
func sessionBus(t *testing.T) *dbus.Conn {
	t.Helper()
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("no dbus-daemon to talk to")
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		t.Skipf("no session bus: %v", err)
	}
	return conn
}

func TestStatusItemRegistersAndAnswersAHost(t *testing.T) {
	host := sessionBus(t)
	watcher := &fakeWatcher{registered: make(chan string, 1)}
	if err := host.Export(watcher, watcherPath, watcherIface); err != nil {
		t.Fatal(err)
	}
	if err := host.Export(introspect.Introspectable(watcherIntrospection), watcherPath,
		"org.freedesktop.DBus.Introspectable"); err != nil {
		t.Fatal(err)
	}
	reply, err := host.RequestName(watcherName, dbus.NameFlagDoNotQueue)
	if err != nil {
		t.Fatal(err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		t.Skipf("something else owns %s on this bus", watcherName)
	}
	defer func() { _, _ = host.ReleaseName(watcherName) }()

	// The item under test, in its own goroutine, driven by a state we control.
	state := State{Running: true, Project: "work", Used: 1 << 20, Limit: 1 << 30, Served: 0.64}
	clicked := make(chan Action, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Read: func() State { return state },
			Do:   func(a Action) error { clicked <- a; return nil },
		})
	}()

	var service string
	select {
	case service = <-watcher.registered:
	case err := <-done:
		t.Fatalf("the item exited instead of registering: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the item never registered with the host")
	}

	// What a panel reads to draw the icon.
	item := host.Object(service, itemPath)
	for name, want := range map[string]string{"Id": "pkgcache", "Status": "Active"} {
		variant, err := item.GetProperty(itemIface + "." + name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if got, _ := variant.Value().(string); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// The pixmap has to be ARGB32 of the declared size, or a panel draws noise.
	variant, err := item.GetProperty(itemIface + ".IconPixmap")
	if err != nil {
		t.Fatal(err)
	}
	pixmaps, ok := variant.Value().([][]any)
	if !ok {
		// The library decodes a(iiay) as a slice of structs; either shape is acceptable
		// here, so this asserts through the typed path instead.
		var typed []pixmap
		if storeErr := dbus.Store([]any{variant.Value()}, &typed); storeErr != nil {
			t.Fatalf("IconPixmap is %T: %v", variant.Value(), storeErr)
		}
		if len(typed) != 1 || typed[0].Width != iconSize ||
			len(typed[0].Bytes) != iconSize*iconSize*4 {
			t.Fatalf("pixmap = %dx%d, %d bytes",
				typed[0].Width, typed[0].Height, len(typed[0].Bytes))
		}
	} else if len(pixmaps) != 1 {
		t.Fatalf("IconPixmap has %d entries", len(pixmaps))
	}

	// The menu, as a host reads it: a layout with one child per item.
	menu := host.Object(service, menuPath)
	var revision uint32
	var layout menuLayout
	if err := menu.Call(menuIface+".GetLayout", 0, int32(0), int32(-1), []string{}).
		Store(&revision, &layout); err != nil {
		t.Fatalf("GetLayout: %v", err)
	}
	if len(layout.Children) < len(Menu) {
		t.Fatalf("layout has %d children for %d items", len(layout.Children), len(Menu))
	}

	// And a click, which is the whole point of the menu existing.
	if err := menu.Call(menuIface+".Event", 0,
		int32(ActionWidget)+menuItemID, "clicked", dbus.MakeVariant(""), uint32(0)).Err; err != nil {
		t.Fatalf("Event: %v", err)
	}
	select {
	case action := <-clicked:
		if action != ActionWidget {
			t.Fatalf("clicking the first item did %v", action)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a menu click reached nothing")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the item exited with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the item did not stop when its context was cancelled")
	}
}

// A full cache must be the state a panel highlights, and an asleep one must not look like
// a working one. Both are properties of the model rather than of any desktop.
func TestIconAndStatusFollowTheState(t *testing.T) {
	full := iconPixmap(State{Running: true, Full: true})
	caching := iconPixmap(State{Running: true})
	asleep := iconPixmap(State{})
	for name, pixmaps := range map[string][]pixmap{
		"full": full, "caching": caching, "asleep": asleep,
	} {
		if len(pixmaps) != 1 || len(pixmaps[0].Bytes) != iconSize*iconSize*4 {
			t.Fatalf("%s icon is malformed", name)
		}
	}
	if string(full[0].Bytes) == string(caching[0].Bytes) {
		t.Error("a full cache draws the same icon as a working one")
	}
	if string(asleep[0].Bytes) == string(caching[0].Bytes) {
		t.Error("an asleep cache draws the same icon as a working one")
	}

	item := &statusItem{state: State{Running: true, Full: true}}
	if item.status() != "NeedsAttention" {
		t.Errorf("a full cache reports %q", item.status())
	}
	item.state.Full = false
	if item.status() != "Active" {
		t.Errorf("a working cache reports %q", item.status())
	}
}
