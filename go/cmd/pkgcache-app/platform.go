package main

import "runtime"

// isDarwin is runtime.GOOS in one place, so the tray setup reads as a choice about icons
// rather than as platform plumbing.
func isDarwin() bool { return runtime.GOOS == "darwin" }
