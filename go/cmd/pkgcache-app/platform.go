package main

import (
	"os"
	"runtime"
	"strings"
)

// isDarwin is runtime.GOOS in one place, so the tray setup reads as a choice about icons
// rather than as platform plumbing.
func isDarwin() bool { return runtime.GOOS == "darwin" }

// notificationsAvailable reports whether this process can post system notifications.
//
// macOS routes them through UNUserNotificationCenter, which refuses to work without a
// bundle identifier — so a bare binary, which is what `make app` produces and what anybody
// developing this runs, cannot have them. Wails treats a service that fails to start as
// fatal, which turns "no notifications" into "the app does not launch".
//
// That trade is wrong the way round. The notification is the least important thing here:
// the tooltip carries the same fact, and a person running the binary from a terminal is
// watching its output anyway. So this is checked first and the service is simply not
// registered when it cannot work.
//
// The check is the executable's path rather than a Core Foundation call, because reading
// the real bundle identifier needs cgo in a file whose whole purpose is to avoid deciding
// anything platform-specific in Go. Inside a bundle the binary lives at
// Foo.app/Contents/MacOS/foo, and that is what the installed app is.
func notificationsAvailable() bool {
	if !isDarwin() {
		return true
	}
	executable, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.Contains(executable, ".app/Contents/MacOS/")
}
