# pkgcache-app

The desktop app: a window onto this machine's cache, and an icon in the status bar that
watches it. It replaces three things — `cmd/pkgcache-window` on Linux and Windows, and
`tools/menubar/main.swift` on macOS — with one program.

## Why this is its own module

Wails needs cgo and a platform GUI toolchain. Keeping it in a separate module means the
main module's `go.mod` never learns about either, so `go build ./...` and `go test ./...`
keep working on any machine — including CI runners with no GTK headers — and `pkgcache`
itself stays one `CGO_ENABLED=0` binary for five targets built from one host.

The `replace` directive points at the parent, so the app shares `internal/appcore` and
`internal/tray` with the CLI rather than reimplementing either.

## Building

```sh
# Ubuntu 24.04 and newer
sudo apt install libgtk-4-dev libwebkitgtk-6.0-dev
go build -o ../../bin/pkgcache-app-linux-amd64 .

# macOS — needs the Command Line Tools
CGO_CFLAGS="-mmacosx-version-min=11.0" go build -o ../../bin/pkgcache-app-darwin-arm64 .

# Windows — needs the WebView2 runtime at run time, nothing extra to build
go build -o ../../bin/pkgcache-app-windows-amd64.exe .
```

`make app` from `go/` does the same thing with the dependency check attached.

For a GTK3 build — WebKit2GTK 4.1, which is what the helper this replaces linked against —
add `-tags gtk3`. Whether that or GTK4 is the right default is still open; see
[the plan](../../../docs/client-app-plan.md).

## Where the logic is

Almost nothing is in this package. `internal/appcore` decides what the cache is doing,
what a menu item should say, and whether something has changed enough to interrupt
somebody — and it has no toolkit in it, so it is tested on machines with no display. What
is left here is the wiring.

Two things in that wiring are load-bearing and easy to undo by accident:

- **Menu updates go through `application.InvokeSync`.** `SystemTray.SetTooltip` marshals
  itself; `MenuItem.SetLabel` and `SetEnabled` do not — they reach into the platform menu
  object on whatever goroutine calls them. The ticker is not that thread.
- **`showWindow` runs off the UI thread.** It may start a cold daemon, which the client
  waits up to thirty seconds for. On the UI thread that is a frozen menu.
- **`CGO_CFLAGS=-mmacosx-version-min=11.0` on macOS is cosmetic, not correctness.** The
  linked binary declares `minos 11.0` either way, matching what `build-pkg.sh` puts in
  `LSMinimumSystemVersion`. The flag exists because six of Wails' darwin files pass only
  `-x objective-c` and so compile against the host SDK, and the linker then prints
  twenty-five "built for newer macOS version" warnings that bury anything real. With it,
  one remains — Go's own runtime object at 13.0, which no flag here reaches.
  `MACOSX_DEPLOYMENT_TARGET` does nothing: clang ignores it wherever
  `-mmacosx-version-min` is already given.

## The rule

The app never keeps the daemon alive. The cache exits when nothing has used it for a
while; an app that held it up would quietly remove the idle exit that makes this polite to
leave installed. Every poll reads state without starting anything. Opening the window is
the single exception, because that is somebody asking to look.
