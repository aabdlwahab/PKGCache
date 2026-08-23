// Command pkgcache-window shows a pkgcache page in a real application window.
//
// The window is the product and a browser tab is a poor container for it: no icon of its
// own in the Dock, the taskbar or alt-tab, an address bar nobody needs, and on a machine
// with no Chromium-family browser installed, no chromeless window at all.
//
// So this is the window, with the platform's own web engine inside it and nothing else:
//
//	linux    WebKitGTK, through cgo. Specialised to Ubuntu — 24.04's webkit2gtk-4.1.
//	windows  WebView2, through a pure-Go binding, so it still cross-compiles from one host.
//	darwin   not this binary. macOS already has a native helper for the menu bar item, and
//	         WKWebView belongs in that process rather than in a second one to sign.
//
// It is a separate binary from pkgcache for two reasons, and the first one is the important
// one: pkgcache stays CGO_ENABLED=0 on five targets from one host, which is the whole
// release story. The second is that a webview and a status bar item each want to own a
// message loop on the main thread, and two loops in one process is a threading problem
// nobody needs to have.
//
// It knows nothing. Given a URL it shows it; the page talks to the cache's API itself.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	title := flag.String("title", "pkgcache", "the window's title")
	width := flag.Int("width", 420, "the window's width in logical pixels")
	height := flag.Int("height", 660, "the window's height in logical pixels")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), `pkgcache-window — a pkgcache page in an application window

usage: pkgcache-window [flags] <url>

Shows one URL in a native window using this platform's own web engine. `+
			`pkgcache starts it for you; there is rarely a reason to run it by hand.

flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := show(flag.Arg(0), *title, *width, *height); err != nil {
		fmt.Fprintf(os.Stderr, "pkgcache-window: %v\n", err)
		os.Exit(1)
	}
}
