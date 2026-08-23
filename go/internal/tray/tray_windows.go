//go:build windows

package tray

import (
	"context"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Shell_NotifyIcon, which is how the Windows notification area works.
//
// No cgo: every call here is a syscall through golang.org/x/sys/windows, which this project
// already depends on. What it costs instead is Win32's shape — a window class, a
// message-only window, a message loop that owns its thread, and a callback message the
// shell sends when somebody clicks.
//
// Three details are load-bearing and easy to get wrong:
//
//   - The window and the loop must be on one OS thread. Go will otherwise move the
//     goroutine and the loop stops receiving anything, which looks like an icon that
//     ignores clicks.
//   - The refresh cannot be a Go ticker: this thread is blocked in GetMessage. It is a
//     Win32 timer, delivered as a message like everything else.
//   - TrackPopupMenu needs the window to be foreground first, or the menu appears and then
//     vanishes on the next click somewhere else.

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassEx     = user32.NewProc("RegisterClassExW")
	procCreateWindowEx      = user32.NewProc("CreateWindowExW")
	procDefWindowProc       = user32.NewProc("DefWindowProcW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procGetMessage          = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessage     = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procAppendMenu          = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procPostMessage         = user32.NewProc("PostMessageW")
	procSetTimer            = user32.NewProc("SetTimer")
	procKillTimer           = user32.NewProc("KillTimer")
	procLoadIcon            = user32.NewProc("LoadIconW")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procCreateIconIndirect  = user32.NewProc("CreateIconIndirect")
	procShellNotifyIcon     = shell32.NewProc("Shell_NotifyIconW")
	procCreateBitmap        = gdi32.NewProc("CreateBitmap")
	procDeleteObject        = gdi32.NewProc("DeleteObject")
	procGetModuleHandle     = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmDestroy      = 0x0002
	wmClose        = 0x0010
	wmCommand      = 0x0111
	wmTimer        = 0x0113
	wmRightUp      = 0x0205
	wmLeftUp       = 0x0202
	wmTrayCallback = 0x0400 + 1 // WM_APP + 1

	nimAdd    = 0x0000
	nimModify = 0x0001
	nimDelete = 0x0002

	nifMessage = 0x0001
	nifIcon    = 0x0002
	nifTip     = 0x0004
	nifInfo    = 0x0010

	mfString    = 0x0000
	mfSeparator = 0x0800
	mfGrayed    = 0x0001

	tpmLeftAlign   = 0x0000
	tpmRightButton = 0x0002

	idiApplication = 32512
	refreshTimerID = 1
)

// notifyIconData is NOTIFYICONDATAW. The struct is versioned by its size, so cbSize must be
// the size of exactly this layout.
type notifyIconData struct {
	Size            uint32
	Wnd             windows.Handle
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            windows.Handle
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        windows.GUID
	BalloonIcon     windows.Handle
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type msg struct {
	Wnd     windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

type point struct{ X, Y int32 }

type iconInfo struct {
	Icon     int32 // BOOL: an icon rather than a cursor
	XHotspot uint32
	YHotspot uint32
	Mask     windows.Handle
	Color    windows.Handle
}

// tray is the live item plus what it needs to answer a click.
type tray struct {
	options Options
	state   State
	wnd     windows.Handle
	icon    windows.Handle
	data    notifyIconData
}

func run(ctx context.Context, o Options) error {
	// The window and its loop belong to one thread for the life of the icon.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	t := &tray{options: o, state: o.Read()}
	if err := t.create(); err != nil {
		return err
	}
	defer t.destroy()
	note(o, "pkgcache: in the notification area")

	// Cancellation reaches a blocked message loop only as a message.
	go func() {
		<-ctx.Done()
		procPostMessage.Call(uintptr(t.wnd), wmClose, 0, 0)
	}()

	var message msg
	for {
		got, _, _ := procGetMessage.Call(
			uintptr(unsafe.Pointer(&message)), uintptr(t.wnd), 0, 0)
		switch int32(got) {
		case -1:
			return fmt.Errorf("tray: message loop failed")
		case 0:
			return nil // WM_QUIT
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
	}
}

func (t *tray) create() error {
	instance, _, _ := procGetModuleHandle.Call(0)
	className := windows.StringToUTF16Ptr("pkgcacheTray")
	class := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   windows.NewCallback(t.wndProc),
		Instance:  windows.Handle(instance),
		ClassName: className,
	}
	if atom, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		return fmt.Errorf("tray: register window class: %w", err)
	}
	// A message-only window: no pixels, no taskbar entry, and it still receives the
	// shell's callbacks. HWND_MESSAGE is (HWND)(-3).
	wnd, _, err := procCreateWindowEx.Call(
		0, uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("pkgcache"))),
		0, 0, 0, 0, 0, ^uintptr(2), 0, instance, 0)
	if wnd == 0 {
		return fmt.Errorf("tray: create window: %w", err)
	}
	t.wnd = windows.Handle(wnd)

	t.icon = t.loadIcon()
	t.data = notifyIconData{
		Size:            uint32(unsafe.Sizeof(notifyIconData{})),
		Wnd:             t.wnd,
		ID:              1,
		Flags:           nifMessage | nifIcon | nifTip,
		CallbackMessage: wmTrayCallback,
		Icon:            t.icon,
	}
	t.setTip(Tooltip(t.state))
	if ok, _, err := procShellNotifyIcon.Call(
		nimAdd, uintptr(unsafe.Pointer(&t.data))); ok == 0 {
		return fmt.Errorf("tray: add notification icon: %w", err)
	}
	// A Win32 timer, because this thread is blocked in GetMessage and a Go ticker would
	// never be read.
	procSetTimer.Call(uintptr(t.wnd), refreshTimerID, uintptr(refreshMillis), 0)
	return nil
}

const refreshMillis = 5000

func (t *tray) destroy() {
	procKillTimer.Call(uintptr(t.wnd), refreshTimerID)
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&t.data)))
	if t.icon != 0 {
		procDestroyIcon.Call(uintptr(t.icon))
	}
	if t.wnd != 0 {
		procDestroyWindow.Call(uintptr(t.wnd))
	}
}

// wndProc is the window's message handler, called from the shell's thread — which is this
// thread, because the loop is here.
func (t *tray) wndProc(wnd windows.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmTrayCallback:
		switch uint32(lParam) {
		case wmLeftUp:
			// The primary click opens the window, which is what the icon is for.
			t.do(ActionWidget)
		case wmRightUp:
			t.showMenu()
		}
		return 0
	case wmCommand:
		// The menu item id is the low word, offset so that zero stays "nothing chosen".
		if id := int32(wParam & 0xffff); id > 0 {
			t.do(Action(id - 1))
		}
		return 0
	case wmTimer:
		t.refresh()
		return 0
	case wmClose:
		procPostQuitMessage.Call(0)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	result, _, _ := procDefWindowProc.Call(
		uintptr(wnd), uintptr(message), wParam, lParam)
	return result
}

func (t *tray) showMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	separators := Separators()
	for _, action := range Menu {
		if separators[action] {
			procAppendMenu.Call(menu, mfSeparator, 0, 0)
		}
		flags := uintptr(mfString)
		if !action.Enabled(t.state) {
			flags |= mfGrayed
		}
		label := windows.StringToUTF16Ptr(action.Label(t.state))
		procAppendMenu.Call(menu, flags, uintptr(int32(action)+1), uintptr(unsafe.Pointer(label)))
	}

	var cursor point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
	// Foreground first, or the menu closes the moment focus moves.
	procSetForegroundWindow.Call(uintptr(t.wnd))
	procTrackPopupMenu.Call(
		menu, tpmLeftAlign|tpmRightButton,
		uintptr(cursor.X), uintptr(cursor.Y), 0, uintptr(t.wnd), 0)
}

func (t *tray) do(action Action) {
	if !action.Enabled(t.state) {
		return
	}
	if action == ActionQuit {
		procPostMessage.Call(uintptr(t.wnd), wmClose, 0, 0)
		return
	}
	if err := t.options.Do(action); err != nil {
		note(t.options, "pkgcache: %v", err)
	}
	t.refresh()
}

func (t *tray) refresh() {
	next := t.options.Read()
	if next == t.state {
		return
	}
	t.state = next
	if icon := t.loadIcon(); icon != 0 {
		previous := t.icon
		t.icon, t.data.Icon = icon, icon
		if previous != 0 {
			procDestroyIcon.Call(uintptr(previous))
		}
	}
	t.setTip(Tooltip(t.state))
	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&t.data)))
}

func (t *tray) setTip(text string) {
	encoded := windows.StringToUTF16(text)
	if len(encoded) > len(t.data.Tip) {
		encoded = encoded[:len(t.data.Tip)-1]
		encoded = append(encoded, 0)
	}
	t.data.Tip = [128]uint16{}
	copy(t.data.Tip[:], encoded)
}

// loadIcon draws the mark, falling back to the stock application icon.
//
// The fallback is deliberate rather than defensive: a generic icon in the right place is a
// working feature, and a nil icon handle is an item the shell refuses to add at all.
func (t *tray) loadIcon() windows.Handle {
	if icon := t.drawIcon(); icon != 0 {
		return icon
	}
	handle, _, _ := procLoadIcon.Call(0, idiApplication)
	return windows.Handle(handle)
}

// drawIcon builds a 16×16 colour icon from the same bracket mark the rest of the product
// uses, through a pair of bitmaps and CreateIconIndirect.
func (t *tray) drawIcon() windows.Handle {
	const size = 16
	// BGRA, which is what CreateBitmap with 32 bits per pixel expects on Windows — the
	// opposite order from the Linux pixmap, and the reason these are separate files.
	pixels := make([]byte, size*size*4)
	blue, green, red := byte(0xf0), byte(0x80), byte(0x4a)
	if t.state.Full {
		blue, green, red = 0x3b, 0x3b, 0xd0
	}
	alpha := byte(0xff)
	if !t.state.Running {
		alpha = 0x66
	}
	set := func(x, y int) {
		if x < 0 || y < 0 || x >= size || y >= size {
			return
		}
		i := (y*size + x) * 4
		pixels[i], pixels[i+1], pixels[i+2], pixels[i+3] = blue, green, red, alpha
	}
	const pad = 3
	for y := pad; y < size-pad; y++ {
		set(pad, y)
		set(size-pad-1, y)
	}
	for x := pad; x < pad+4; x++ {
		set(x, pad)
		set(x, size-pad-1)
		set(size-1-x, pad)
		set(size-1-x, size-pad-1)
	}
	for y := size/2 - 1; y <= size/2; y++ {
		set(size/2-1, y)
		set(size/2, y)
	}

	colour, _, _ := procCreateBitmap.Call(size, size, 1, 32, uintptr(unsafe.Pointer(&pixels[0])))
	if colour == 0 {
		return 0
	}
	defer procDeleteObject.Call(colour)
	// A 1-bit mask of zeroes: the alpha channel above does the shaping, and a mask of ones
	// would punch the whole icon out.
	mask := make([]byte, size*size/8)
	monochrome, _, _ := procCreateBitmap.Call(size, size, 1, 1, uintptr(unsafe.Pointer(&mask[0])))
	if monochrome == 0 {
		return 0
	}
	defer procDeleteObject.Call(monochrome)

	info := iconInfo{Icon: 1, Mask: windows.Handle(monochrome), Color: windows.Handle(colour)}
	handle, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&info)))
	return windows.Handle(handle)
}
