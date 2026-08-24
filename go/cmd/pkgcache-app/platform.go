package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/local"
)

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

// daemonPath finds the pkgcache binary this app starts when the cache is not running.
//
// One line, because internal/local owns the answer: the app, the docker shim and anything
// else that is not pkgcache itself all need the same binary found the same way, and three
// copies of that search would eventually disagree about which one.
//
// An error rather than a fallback. local.Ensure's own default — re-execute whoever is
// calling — is silent, wrong for this binary, and costs thirty seconds of a spinning
// window before it admits anything is amiss.
func daemonPath() (string, error) {
	path, found := local.DaemonPath()
	if !found {
		return "", fmt.Errorf(
			"cannot find the pkgcache binary, which is what holds the cache.\n" +
				"  It is installed beside this app; if you are running from a build tree, put it\n" +
				"  on PATH or in this directory:  make pkgcache")
	}
	return path, nil
}
