# Go dependencies

Dependencies are kept deliberately small. Each direct dependency exists because the
standard library does not provide the required primitive.

| Module | Reason |
|---|---|
| `github.com/prometheus/client_golang` | Prometheus collectors and exposition format |
| `golang.org/x/sync` | Shared concurrency primitives used by the cache engine |
| `gopkg.in/yaml.v3` | Strict YAML configuration decoding |
| `modernc.org/sqlite` | Pure-Go SQLite driver, preserving static `CGO_ENABLED=0` builds |
| `github.com/jchv/go-webview2` | The Windows application window. WebView2 is Edge's engine and reachable only through COM; this binding does it in pure Go — verified with `grep 'import \"C\"'` and by cross-compiling for Windows under `CGO_ENABLED=0` — so a native window on Windows costs nothing in the release model. The alternative, `webview/webview_go`, needs cgo on every platform and pins `webkit2gtk-4.0`, which Ubuntu 24.04 no longer ships |
| `github.com/godbus/dbus/v5` | The Linux status bar item. StatusNotifierItem is a D-Bus protocol, and reaching it needs SASL EXTERNAL authentication over a Unix socket plus the full binary marshalling layer — alignment rules, signatures, variants. The standard library has no D-Bus; hand-rolling it is several hundred lines of the kind of code that fails in ways nobody can debug. This keeps the alternative — cgo, and with it a build host per platform — off the table |

## What is deliberately not a dependency

A status bar icon or an application window is the obvious place to reach for a
cross-platform toolkit, and every one of them — Fyne, Wails, `webview`,
`getlantern/systray` — needs cgo. This project builds five
targets with `CGO_ENABLED=0` from one Linux host, so the icon is hand-written per platform
instead: `Shell_NotifyIcon` through `golang.org/x/sys/windows`, StatusNotifierItem through
the module above, and on macOS a separate signed Swift helper, because `NSStatusItem` is
Cocoa and has no pure-Go path at all. The window is the same shape: WebView2 through the pure-Go binding above on Windows,
WebKitGTK through the only cgo in the product on Linux — behind a `webkitgtk` build tag so
`go build ./...` never needs GTK headers — and WKWebView inside the Swift helper on macOS.
All three are separate helper binaries, which is what keeps `pkgcache` itself one static
`CGO_ENABLED=0` executable for five targets from one host. See
[`go/internal/tray`](../go/internal/tray), [`go/cmd/pkgcache-window`](../go/cmd/pkgcache-window)
and [`go/tools/menubar`](../go/tools/menubar).
