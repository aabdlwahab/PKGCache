//go:build linux

package tray

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

// StatusNotifierItem, which is how a status bar icon works on Linux now.
//
// The item is a D-Bus object this process exports; the desktop's watcher discovers it,
// asks for its properties, and draws it. There is no X11 in this file and no cgo: the
// whole protocol is method calls and properties, which is why this platform can be done
// in pure Go while macOS cannot.
//
// The menu is DBusMenu, a second interface on the same connection. It is the fiddly half —
// a layout is a nested variant tree with a revision number the host compares — so it is
// kept to one flat list of items, which is all a status bar menu should be anyway.
//
// KDE, Plasma, most tiling setups and anything with libappindicator show this natively.
// GNOME needs a shell extension, which is a real adoption wart and exactly why the browser
// window has to stand on its own.

const (
	watcherName  = "org.kde.StatusNotifierWatcher"
	watcherPath  = "/StatusNotifierWatcher"
	itemPath     = "/StatusNotifierItem"
	menuPath     = "/MenuBar"
	itemIface    = "org.kde.StatusNotifierItem"
	menuIface    = "com.canonical.dbusmenu"
	refreshEvery = 5 * time.Second
)

func run(ctx context.Context, o Options) error {
	conn, err := dbus.SessionBus()
	if err != nil {
		// No session bus is the ordinary case over SSH, in CI and in a container. Reported
		// as unsupported rather than as a failure: the caller prints the widget's address
		// instead, which is the useful half of the answer.
		return fmt.Errorf("%w: no session bus (%v)", ErrUnsupported, err)
	}

	// A unique name per process, as the specification requires: several programs put items
	// in one status bar, and the pid is what keeps two pkgcaches apart.
	name := fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", os.Getpid())
	reply, err := conn.RequestName(name, dbus.NameFlagDoNotQueue)
	if err != nil {
		return fmt.Errorf("tray: claim %s: %w", name, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("tray: %s is already taken", name)
	}

	item := &statusItem{conn: conn, options: o, state: o.Read()}
	if err := item.export(); err != nil {
		return err
	}
	if err := conn.Object(watcherName, watcherPath).
		Call(watcherIface+".RegisterStatusNotifierItem", 0, name).Err; err != nil {
		// The desktop has no watcher — GNOME without an extension is the common case. The
		// item is exported and correct; nothing is drawing it, and saying so is better than
		// sitting silently in a process that appears to have worked.
		return fmt.Errorf(
			"%w: this desktop has no status bar host for pkgcache to appear in.\n"+
				"  On GNOME that is an extension away (AppIndicator support); everywhere\n"+
				"  else `pkgcache widget` opens the same window: %v", ErrUnsupported, err)
	}
	note(o, "pkgcache: in the status bar")

	ticker := time.NewTicker(refreshEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			item.refresh()
		}
	}
}

const watcherIface = "org.kde.StatusNotifierWatcher"

// statusItem is the exported object, plus the properties the host reads.
type statusItem struct {
	conn     *dbus.Conn
	options  Options
	state    State
	props    *prop.Properties
	revision uint32
}

// Activate is the primary click. Opening the window is the only sensible answer: it is
// what somebody put an icon in their status bar for.
func (s *statusItem) Activate(_ int32, _ int32) *dbus.Error {
	s.do(ActionWidget)
	return nil
}

// SecondaryActivate is the middle click, which the specification leaves to the item. The
// console, because it is the other thing worth one gesture.
func (s *statusItem) SecondaryActivate(_ int32, _ int32) *dbus.Error {
	s.do(ActionConsole)
	return nil
}

func (s *statusItem) Scroll(_ int32, _ string) *dbus.Error { return nil }

func (s *statusItem) do(action Action) {
	if err := s.options.Do(action); err != nil {
		note(s.options, "pkgcache: %v", err)
	}
	s.refresh()
}

// export publishes the item and its menu, with the introspection data a host expects.
func (s *statusItem) export() error {
	if err := s.conn.Export(s, itemPath, itemIface); err != nil {
		return fmt.Errorf("tray: export item: %w", err)
	}
	menu := &dbusMenu{item: s}
	if err := s.conn.Export(menu, menuPath, menuIface); err != nil {
		return fmt.Errorf("tray: export menu: %w", err)
	}
	// Introspection data as a string, which is what a host actually reads. The typed
	// builder in the library wants a parsed tree, and hand-writing the XML keeps the
	// signatures — a(iiay) for a pixmap, (ia{sv}av) for a menu layout — next to the Go
	// types that have to match them.
	for path, xml := range map[dbus.ObjectPath]string{
		itemPath: itemIntrospection,
		menuPath: menuIntrospection,
	} {
		if err := s.conn.Export(
			introspect.Introspectable(xml), path,
			"org.freedesktop.DBus.Introspectable"); err != nil {
			return err
		}
	}

	properties, err := prop.Export(s.conn, itemPath, map[string]map[string]*prop.Prop{
		itemIface: {
			"Category":   {Value: "SystemServices", Writable: false, Emit: prop.EmitTrue},
			"Id":         {Value: "pkgcache", Writable: false, Emit: prop.EmitTrue},
			"Title":      {Value: "pkgcache", Writable: false, Emit: prop.EmitTrue},
			"Status":     {Value: s.status(), Writable: false, Emit: prop.EmitTrue},
			"IconName":   {Value: "", Writable: false, Emit: prop.EmitTrue},
			"IconPixmap": {Value: iconPixmap(s.state), Writable: false, Emit: prop.EmitTrue},
			"ToolTip":    {Value: s.tooltip(), Writable: false, Emit: prop.EmitTrue},
			"Menu":       {Value: dbus.ObjectPath(menuPath), Writable: false, Emit: prop.EmitTrue},
			"ItemIsMenu": {Value: false, Writable: false, Emit: prop.EmitTrue},
		},
	})
	if err != nil {
		return fmt.Errorf("tray: export properties: %w", err)
	}
	s.props = properties
	return nil
}

// refresh re-reads the state and tells the host what changed.
//
// NewIcon and NewToolTip rather than a property-changed signal: StatusNotifierItem predates
// org.freedesktop.DBus.Properties conventions and its hosts listen for these.
func (s *statusItem) refresh() {
	next := s.options.Read()
	if next == s.state {
		return
	}
	s.state = next
	if s.props != nil {
		_ = s.props.Set(itemIface, "Status", dbus.MakeVariant(s.status()))
		_ = s.props.Set(itemIface, "IconPixmap", dbus.MakeVariant(iconPixmap(s.state)))
		_ = s.props.Set(itemIface, "ToolTip", dbus.MakeVariant(s.tooltip()))
	}
	_ = s.conn.Emit(itemPath, itemIface+".NewIcon")
	_ = s.conn.Emit(itemPath, itemIface+".NewToolTip")
	s.revision++
	_ = s.conn.Emit(menuPath, menuIface+".LayoutUpdated", s.revision, int32(0))
}

// status drives how the host draws the icon. NeedsAttention is what makes a full cache
// visible without anybody opening anything, which is the whole point of the icon.
func (s *statusItem) status() string {
	if s.state.Full {
		return "NeedsAttention"
	}
	return "Active"
}

// tooltip is the specification's struct: icon name, pixmap, title, description.
func (s *statusItem) tooltip() tooltipStruct {
	return tooltipStruct{Title: "pkgcache", Description: Tooltip(s.state)}
}

type tooltipStruct struct {
	IconName    string
	IconPixmap  []pixmap
	Title       string
	Description string
}

const itemIntrospection = `<node>
  <interface name="org.kde.StatusNotifierItem">
    <method name="Activate"><arg type="i" direction="in"/><arg type="i" direction="in"/></method>
    <method name="SecondaryActivate"><arg type="i" direction="in"/><arg type="i" direction="in"/></method>
    <method name="Scroll"><arg type="i" direction="in"/><arg type="s" direction="in"/></method>
    <signal name="NewIcon"/>
    <signal name="NewToolTip"/>
    <property name="Category" type="s" access="read"/>
    <property name="Id" type="s" access="read"/>
    <property name="Title" type="s" access="read"/>
    <property name="Status" type="s" access="read"/>
    <property name="IconName" type="s" access="read"/>
    <property name="IconPixmap" type="a(iiay)" access="read"/>
    <property name="ToolTip" type="(sa(iiay)ss)" access="read"/>
    <property name="Menu" type="o" access="read"/>
    <property name="ItemIsMenu" type="b" access="read"/>
  </interface>
</node>`
