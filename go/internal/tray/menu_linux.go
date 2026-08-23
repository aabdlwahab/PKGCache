//go:build linux

package tray

import (
	"github.com/godbus/dbus/v5"
)

// DBusMenu, the second half of a status bar item on Linux.
//
// The host does not ask "what are your menu items"; it asks for a *layout* — a nested tree
// of (id, properties, children) inside a variant, with a revision number it compares
// against what it last drew. That shape is the reason this file exists separately: it is
// the only genuinely awkward part of the protocol, and keeping it flat and in one place is
// what stops it spreading.
//
// One level, no submenus. A status bar menu that needs a tree is a window.

// dbusMenu exports com.canonical.dbusmenu for one status item.
type dbusMenu struct {
	item *statusItem
}

// menuItemID offsets the action so that 0 stays the root, which the protocol reserves.
const menuItemID = 1

// GetLayout returns the whole menu. parentID and depth are honoured loosely on purpose:
// there is one level, so any request that is not for the root has no children to give.
func (m *dbusMenu) GetLayout(
	parentID int32, _ int32, _ []string,
) (uint32, menuLayout, *dbus.Error) {
	state := m.item.state
	separators := Separators()
	var children []dbus.Variant
	if parentID == 0 {
		for _, action := range Menu {
			if separators[action] {
				children = append(children, dbus.MakeVariant(menuLayout{
					ID: int32(action) + menuItemID + 1000,
					Properties: map[string]dbus.Variant{
						"type": dbus.MakeVariant("separator"),
					},
					Children: []dbus.Variant{},
				}))
			}
			children = append(children, dbus.MakeVariant(menuLayout{
				ID: int32(action) + menuItemID,
				Properties: map[string]dbus.Variant{
					"label":   dbus.MakeVariant(action.Label(state)),
					"enabled": dbus.MakeVariant(action.Enabled(state)),
					"visible": dbus.MakeVariant(true),
				},
				Children: []dbus.Variant{},
			}))
		}
	}
	return m.item.revision, menuLayout{
		ID:         0,
		Properties: map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")},
		Children:   children,
	}, nil
}

// menuLayout is the (ia{sv}av) the protocol wants. The children are variants of this same
// struct, which is why the field is []dbus.Variant and not []menuLayout.
type menuLayout struct {
	ID         int32
	Properties map[string]dbus.Variant
	Children   []dbus.Variant
}

// GetGroupProperties is how a host refreshes labels without re-reading the layout.
func (m *dbusMenu) GetGroupProperties(
	ids []int32, _ []string,
) ([]menuProperties, *dbus.Error) {
	state := m.item.state
	wanted := map[int32]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var out []menuProperties
	for _, action := range Menu {
		id := int32(action) + menuItemID
		if len(ids) > 0 && !wanted[id] {
			continue
		}
		out = append(out, menuProperties{
			ID: id,
			Properties: map[string]dbus.Variant{
				"label":   dbus.MakeVariant(action.Label(state)),
				"enabled": dbus.MakeVariant(action.Enabled(state)),
				"visible": dbus.MakeVariant(true),
			},
		})
	}
	return out, nil
}

type menuProperties struct {
	ID         int32
	Properties map[string]dbus.Variant
}

// GetProperty answers for one item, which some hosts prefer.
func (m *dbusMenu) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	action := Action(id - menuItemID)
	switch name {
	case "label":
		return dbus.MakeVariant(action.Label(m.item.state)), nil
	case "enabled":
		return dbus.MakeVariant(action.Enabled(m.item.state)), nil
	case "visible":
		return dbus.MakeVariant(true), nil
	}
	return dbus.MakeVariant(""), nil
}

// Event is a click. "clicked" is the only one that matters; hosts also send hover events,
// which are deliberately ignored rather than treated as intent.
func (m *dbusMenu) Event(id int32, eventID string, _ dbus.Variant, _ uint32) *dbus.Error {
	if eventID != "clicked" {
		return nil
	}
	action := Action(id - menuItemID)
	if !action.Enabled(m.item.state) {
		return nil
	}
	for _, known := range Menu {
		if known == action {
			m.item.do(action)
			return nil
		}
	}
	return nil
}

// AboutToShow is the host's chance to let us refresh before drawing. Taken: a menu that
// says "Stop the cache" for a cache that exited ten minutes ago is worse than a slow one.
func (m *dbusMenu) AboutToShow(_ int32) (bool, *dbus.Error) {
	m.item.refresh()
	return true, nil
}

func (m *dbusMenu) EventGroup(
	events []struct {
		ID        int32
		EventID   string
		Data      dbus.Variant
		Timestamp uint32
	},
) ([]int32, *dbus.Error) {
	for _, event := range events {
		_ = m.Event(event.ID, event.EventID, event.Data, event.Timestamp)
	}
	return nil, nil
}

const menuIntrospection = `<node>
  <interface name="com.canonical.dbusmenu">
    <method name="GetLayout">
      <arg type="i" direction="in"/><arg type="i" direction="in"/><arg type="as" direction="in"/>
      <arg type="u" direction="out"/><arg type="(ia{sv}av)" direction="out"/>
    </method>
    <method name="GetGroupProperties">
      <arg type="ai" direction="in"/><arg type="as" direction="in"/>
      <arg type="a(ia{sv})" direction="out"/>
    </method>
    <method name="GetProperty">
      <arg type="i" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="out"/>
    </method>
    <method name="Event">
      <arg type="i" direction="in"/><arg type="s" direction="in"/>
      <arg type="v" direction="in"/><arg type="u" direction="in"/>
    </method>
    <method name="EventGroup">
      <arg type="a(isvu)" direction="in"/><arg type="ai" direction="out"/>
    </method>
    <method name="AboutToShow">
      <arg type="i" direction="in"/><arg type="b" direction="out"/>
    </method>
    <signal name="LayoutUpdated"><arg type="u"/><arg type="i"/></signal>
    <property name="Version" type="u" access="read"/>
    <property name="Status" type="s" access="read"/>
  </interface>
</node>`
