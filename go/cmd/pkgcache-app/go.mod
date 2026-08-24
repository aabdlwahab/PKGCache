// The desktop app is its own module, deliberately.
//
// Wails needs cgo and a platform GUI toolchain. Keeping it out of the main module means
// go.mod there stays free of it, `go build ./...` and `go test ./...` keep working on any
// machine — including the CI runners that have no GTK headers — and pkgcache itself stays
// one CGO_ENABLED=0 binary for five targets built from one host, which is the whole
// release story and not worth trading for a window.
//
// The replace directive points at the parent so the app shares internal/appcore and
// internal/tray with the CLI rather than reimplementing either.
module github.com/aabdlwahab/PKGCache/cmd/pkgcache-app

go 1.25.0

require (
	github.com/aabdlwahab/PKGCache v0.0.0
	github.com/wailsapp/wails/v3 v3.0.0-beta.12
)

require (
	git.sr.ht/~jackmordaunt/go-toast/v2 v2.0.3 // indirect
	github.com/ProtonMail/go-crypto v1.4.1 // indirect
	github.com/adrg/xdg v0.5.3 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jchv/go-winloader v0.0.0-20250406163304-c1995be93bd1 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/prometheus/client_golang v1.24.1 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/crypto v0.53.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.54.0 // indirect
)

replace github.com/aabdlwahab/PKGCache => ../..
