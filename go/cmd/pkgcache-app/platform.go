package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
// Beside this program first, because that is what every installer here produces: the .deb
// puts both in /usr/bin, the .pkg puts both under /usr/local/bin, and somebody who copied
// two files onto a machine has them in one directory. PATH second, for a development tree
// where the two are built to different places.
//
// An error rather than a fallback, and deliberately so. The fallback that local.Ensure
// applies on its own — re-execute whoever is calling — is silent, wrong for this binary,
// and costs thirty seconds of a spinning window before it admits anything is amiss.
func daemonPath() (string, error) {
	name := "pkgcache"
	if runtime.GOOS == "windows" {
		name = "pkgcache.exe"
	}
	if self, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(self), name)
		if info, statErr := os.Stat(beside); statErr == nil && !info.IsDir() {
			return beside, nil
		}
	}
	found, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf(
			"cannot find the %s binary, which is what holds the cache.\n"+
				"  It is installed beside this app; if you are running from a build tree, put it\n"+
				"  on PATH or in this directory:  make pkgcache", name)
	}
	return found, nil
}
