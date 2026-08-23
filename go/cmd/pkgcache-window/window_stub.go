//go:build (linux && !(cgo && webkitgtk)) || (!linux && !windows)

package main

import (
	"errors"
	"fmt"
	"runtime"
)

// What this binary is without its platform's engine compiled in.
//
// Two different absences, one message each, because they are fixed differently: a Linux
// build made without the tag is a build problem, and macOS is not this binary at all.
func show(_, _ string, _, _ int) error {
	if runtime.GOOS == "linux" {
		return errors.New(
			"this build has no web engine compiled in.\n" +
				"  On Ubuntu: sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev\n" +
				"  then: go build -tags webkitgtk -o pkgcache-window ./cmd/pkgcache-window")
	}
	if runtime.GOOS == "darwin" {
		return errors.New(
			"macOS does not use this binary. Its window lives in pkgcache-menubar, which\n" +
				"  already owns a native process for the menu bar item — see tools/menubar")
	}
	return fmt.Errorf("no web engine for %s", runtime.GOOS)
}
