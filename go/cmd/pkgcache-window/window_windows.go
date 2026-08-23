//go:build windows

package main

import (
	"fmt"

	"github.com/jchv/go-webview2"
)

// WebView2, which is Edge's engine and part of Windows.
//
// The binding is pure Go — it reaches the runtime through COM rather than a C wrapper — so
// this cross-compiles for Windows from the same Linux host as everything else, and the
// release model does not change to gain a window.
//
// The runtime it needs ships with Windows 11 and arrives with Edge on 10, so it is almost
// always present. When it is not, the error says so rather than reporting a nil pointer:
// that is a 1.5 MB download, not a mystery.
func show(url, title string, width, height int) error {
	view := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug: false,
		WindowOptions: webview2.WindowOptions{
			Title:  title,
			Width:  uint(width),
			Height: uint(height),
			// Centered, because a window somebody opens to glance at should not appear
			// wherever the last one happened to be.
			Center: true,
		},
	})
	if view == nil {
		return fmt.Errorf(
			"this machine has no WebView2 runtime.\n" +
				"  It ships with Windows 11 and with Edge on Windows 10; install the\n" +
				"  Evergreen runtime from Microsoft, or use `pkgcache widget -tab`")
	}
	defer view.Destroy()
	view.Navigate(url)
	view.Run()
	return nil
}
