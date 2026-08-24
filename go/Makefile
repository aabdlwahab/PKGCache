# pkgreg — build, test, lint.
#
# Release builds are CGO-free so the result is a genuinely static binary that runs
# on any Linux with a compatible kernel — no libc, no OpenSSL, nothing to install.

BIN        := pkgreg
PKG        := github.com/aabdlwahab/PKGCache
BUILDINFO  := $(PKG)/internal/buildinfo
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE       ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X $(BUILDINFO).version=$(VERSION) \
           -X $(BUILDINFO).commit=$(COMMIT) \
           -X $(BUILDINFO).date=$(DATE)

# The console is checked-in source and is always embedded. There is no build tag,
# no bundler and no Node: `go build` produces the whole shipped program.

.PHONY: all build release client-build client-release client-install client-publish bridge-release app windows-installer test race lint fmt tidy clean help

all: build

build: ## Build the binary for the host platform
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/$(BIN) ./cmd/pkgreg

release: ## Build stripped, reproducible static binaries (amd64 + arm64)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/$(BIN)-linux-amd64 ./cmd/pkgreg
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/$(BIN)-linux-arm64 ./cmd/pkgreg

client-build: ## Build pkgreg-client for this host
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/pkgreg-client ./cmd/pkgreg-client

client-release: ## Build the client installer for Linux, macOS, and Windows
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgreg-client-linux-amd64 ./cmd/pkgreg-client
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgreg-client-linux-arm64 ./cmd/pkgreg-client
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgreg-client-darwin-amd64 ./cmd/pkgreg-client
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgreg-client-darwin-arm64 ./cmd/pkgreg-client
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgreg-client-windows-amd64.exe ./cmd/pkgreg-client
	cd bin && sha256sum pkgreg-client-linux-* pkgreg-client-darwin-* \
		pkgreg-client-windows-*.exe > pkgreg-client-SHA256SUMS

# pkgcache is the merged client. Published under its own names alongside the
# pkgreg-client ones, which still carry the forwarding shim until N7 retires it — see
# docs/client-merge-plan.md.
client-publish: client-release pkgcache-release client-install ## Build and publish clients into DATA_DIR/downloads

# Publishing is `pkgreg publish-client`, not a pile of install(1) and sha256sum(1)
# calls, because a production host has the binary and nothing else — no Makefile, no Go
# toolchain, no coreutils guarantees. This target is a convenience wrapper over the
# same command an operator runs there, so the two paths cannot diverge.
client-install: build ## Publish already-built clients into DATA_DIR/downloads
	@test -n "$(DATA_DIR)" || { \
		echo "DATA_DIR is required (example: make client-publish DATA_DIR=/var/lib/pkgreg)" >&2; \
		exit 2; \
	}
	./bin/$(BIN) publish-client -data-dir "$(DATA_DIR)" bin

# The bridge is the unprivileged alternative to installing the CA. It is a separate,
# stdlib-only program on purpose: nothing it does needs the cache's internals, and a
# developer laptop should not have to run a copy of the server to talk to one.
bridge-release: ## Build the loopback bridge for Linux, macOS, and Windows
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgreg-bridge-linux-amd64 ./cmd/pkgreg-bridge
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgreg-bridge-linux-arm64 ./cmd/pkgreg-bridge
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgreg-bridge-darwin-amd64 ./cmd/pkgreg-bridge
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgreg-bridge-darwin-arm64 ./cmd/pkgreg-bridge
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgreg-bridge-windows-amd64.exe ./cmd/pkgreg-bridge
	cd bin && sha256sum pkgreg-bridge-linux-* pkgreg-bridge-darwin-* \
		pkgreg-bridge-windows-*.exe > pkgreg-bridge-SHA256SUMS

# pkgcache is the same engine composed for one machine: one loopback port, no
# certificate, no accounts. It ships as its own binary rather than a subcommand so a
# laptop does not carry the control plane, the PKI and the migration code.
pkgcache: ## Build pkgcache for this host
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/pkgcache ./cmd/pkgcache

# The desktop app. One program for all three platforms, replacing the WebKitGTK helper,
# the WebView2 helper and the Swift menu bar item that used to live here.
#
# A separate binary from pkgcache, and a separate module: pkgcache stays CGO_ENABLED=0 on
# five targets from one host, which is the whole release story, and the app is where cgo
# is allowed.
# The app is built natively, so its name carries the host's architecture rather than a
# cross-compilation target.
GOARCH ?= $(shell go env GOARCH)

# The macOS floor handed to cgo, which is noise reduction and nothing more.
#
# The linked binary declares minos 11.0 — matching the LSMinimumSystemVersion that
# build-pkg.sh writes — so nothing here is load-bearing. What it fixes is a wall of
# linker warnings: Wails sets -mmacosx-version-min in most of its darwin #cgo CFLAGS
# but six files pass only -x objective-c, so those compile against whatever SDK the Mac
# has (macOS 26 on a current one) and the linker complains about each. Twenty-five
# warnings become one, and a build whose output is legible is a build whose real warnings
# get read.
#
# The survivor is Go's own runtime object, built for 13.0, which no flag here reaches.
# Raising the floor to silence it would narrow the Macs this runs on to buy tidier
# output, which is the wrong trade.
#
# MACOSX_DEPLOYMENT_TARGET does not work for this: clang ignores it wherever
# -mmacosx-version-min is already given, which for Wails is nearly everywhere.
MACOS_MIN ?= 11.0

# Built from inside cmd/pkgcache-app, because it is its own module: the main module does
# not contain that package, so `go build ./cmd/pkgcache-app` from here cannot find it.
# The output path is relative to that directory for the same reason.
app: ## Build the desktop app for this platform
	@case "$$(uname)" in \
	  Linux) \
	    pkg-config --exists gtk4 webkitgtk-6.0 || { \
	      echo "the app needs GTK4 and WebKitGTK 6.0:"; \
	      echo "  sudo apt install libgtk-4-dev libwebkitgtk-6.0-dev"; \
	      echo "For a GTK3 build instead, add -tags gtk3 and install"; \
	      echo "  libgtk-3-dev libwebkit2gtk-4.1-dev"; \
	      exit 1; }; \
	    cd cmd/pkgcache-app && go build -trimpath -ldflags '-s -w $(LDFLAGS)' \
	      -o ../../bin/pkgcache-app-linux-$(GOARCH) . ;; \
	  Darwin) \
	    cd cmd/pkgcache-app && \
	    CGO_CFLAGS="-mmacosx-version-min=$(MACOS_MIN)" \
	      go build -trimpath -ldflags '-s -w $(LDFLAGS)' \
	      -o ../../bin/pkgcache-app-darwin-$(GOARCH) . ;; \
	  *) echo "build the app on the platform it is for; see docs/client-app-plan.md"; \
	     exit 1 ;; \
	esac

pkgcache-release: ## Build pkgcache for Linux, macOS, and Windows
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgcache-linux-amd64 ./cmd/pkgcache
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgcache-linux-arm64 ./cmd/pkgcache
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgcache-darwin-amd64 ./cmd/pkgcache
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgcache-darwin-arm64 ./cmd/pkgcache
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgcache-windows-amd64.exe ./cmd/pkgcache
	cd bin && sha256sum pkgcache-linux-* pkgcache-darwin-* \
		pkgcache-windows-*.exe > pkgcache-SHA256SUMS

# Installers. The two scripts are copied rather than generated: they are the artefact a
# person is asked to run, and one that a build step assembles is one nobody can read
# before running it.
installers: pkgcache-release deb ## Build the .deb packages and stage the install scripts
	install -m 0755 ../packaging/install.sh bin/install.sh
	install -m 0644 ../packaging/install.ps1 bin/install.ps1
	@echo
	@echo "staged in bin/:"
	@echo "  install.sh    macOS and Linux"
	@echo "  install.ps1   Windows"
	@echo "  *.deb         Ubuntu and Debian"

# Two packages per architecture: pkgcache for any machine, pkgcache-desktop for one with
# a screen. The desktop half is built only where an app binary for that architecture
# exists — the app needs cgo, so a host that cannot build it still produces a complete
# daemon package rather than failing.
# The Windows installer. makensis runs on Linux, so this cross-builds like everything
# else — but it needs both Windows binaries, and the app cannot be cross-compiled: Wails
# needs cgo, so pkgcache-app-windows-amd64.exe comes from a Windows runner.
windows-installer: ## Build the Windows installer (needs makensis and both .exe files)
	@command -v makensis >/dev/null || { \
		echo "makensis is missing: dnf install mingw32-nsis, or apt install nsis" >&2; \
		exit 1; }
	@test -f bin/pkgcache-windows-amd64.exe || { \
		echo "build it first: make pkgcache-release" >&2; exit 1; }
	@test -f bin/pkgcache-app-windows-amd64.exe || { \
		echo "no bin/pkgcache-app-windows-amd64.exe — build it on a Windows runner" >&2; \
		exit 1; }
	cp bin/pkgcache-windows-amd64.exe ../packaging/windows/pkgcache.exe
	cp bin/pkgcache-app-windows-amd64.exe ../packaging/windows/pkgcache-app.exe
	cd ../packaging/windows && makensis -DVERSION=$(VERSION) pkgcache.nsi
	rm -f ../packaging/windows/pkgcache.exe ../packaging/windows/pkgcache-app.exe
	mv ../packaging/windows/pkgcache-*-setup.exe bin/
	@echo "built bin/pkgcache-$(VERSION)-setup.exe"

deb: ## Build Debian packages (pkgcache and pkgcache-desktop) for amd64 and arm64
	@test -f bin/pkgcache-linux-amd64 || { \
		echo "build the binaries first: make pkgcache-release" >&2; exit 1; }
	@for arch in amd64 arm64; do \
		app="bin/pkgcache-app-linux-$$arch"; \
		if test ! -f "$$app"; then \
			echo "note: no $$app, so only the daemon package for $$arch"; \
			app=""; \
		fi; \
		PKGCACHE_APP="$$app" sh ../packaging/deb/build.sh \
			"bin/pkgcache-linux-$$arch" "$$arch" "$(VERSION)" bin; \
	done

test: ## Run tests
	go test ./...

race: ## Run tests under the race detector (mandatory before merge)
	go test -race ./...

lint: ## Run golangci-lint
	golangci-lint run

fmt: ## Format
	gofmt -s -w .

tidy: ## Tidy and verify modules
	go mod tidy && go mod verify

clean:
	rm -rf bin

help:
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
