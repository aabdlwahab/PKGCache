# Go dependencies

Dependencies are kept deliberately small. Each direct dependency exists because the
standard library does not provide the required primitive.

## The main module — `pkgreg`, `pkgcache`, `pkgcache-docker`

Everything here builds with `CGO_ENABLED=0`, which is what makes one Linux host produce a
static binary for every supported target.

| Module | Reason |
|---|---|
| `github.com/prometheus/client_golang` | Prometheus collectors and exposition format |
| `golang.org/x/sync` | Shared concurrency primitives used by the cache engine |
| `golang.org/x/sys` | Platform calls with no standard-library form: process control, file locking, and the Windows-specific halves of the client |
| `gopkg.in/yaml.v3` | Strict YAML configuration decoding |
| `modernc.org/sqlite` | Pure-Go SQLite driver, preserving static `CGO_ENABLED=0` builds |
| `github.com/ProtonMail/go-crypto` | OpenPGP clear-signing for the apt repository the client feed is published as. `apt` will not accept an unsigned `InRelease`, and the standard library has no OpenPGP |

## The app module — `pkgcache-app`

The desktop app is [its own module](../go/cmd/pkgcache-app), and that separation is the
whole point: it needs cgo and a GUI toolchain, and keeping it out of the main module means
`go build ./...` and `go test ./...` still work on any machine — including the CI runners
with no GTK headers — and every other binary in the product stays `CGO_ENABLED=0`.

| Module | Reason |
|---|---|
| `github.com/wailsapp/wails/v3` | One window and one status bar item for three platforms: WebKitGTK on Linux, WebView2 on Windows, WKWebView and `NSStatusItem` on macOS |

## What is deliberately not a dependency

There is no frontend framework and no bundler. The console is checked-in HTML, CSS and ES
modules, embedded in every build, and the app's window loads that same console from the
loopback port rather than carrying a second copy of it.

The app is also the only place a toolkit is allowed. It was three programs before — a cgo
WebKitGTK window, a WebView2 window, and a signed Swift menu bar helper — one per platform,
with three status bar implementations behind them, all to avoid a cgo dependency in the
build. What that bought in build simplicity it spent three times over in code nobody could
test on the machine they were sitting at. One toolkit in one module, with the decisions it
makes kept in [`internal/appcore`](../go/internal/appcore) where they can be tested with no
display at all, was the better trade. See
[the desktop app plan](client-app-plan.md) for how that was reasoned about at the time.
