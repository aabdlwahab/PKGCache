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

# sha256sum is GNU coreutils and is not on a Mac; macOS ships shasum instead. Three
# release targets below write a sums file, and all three run on both platforms — the
# macOS job builds pkgcache for five targets and then checksums them, which failed with
# "sha256sum: command not found" the first time that job ever got as far as running.
#
# The two agree on output format — "<digest>  <name>" — so `sha256sum -c` still verifies
# a file written by either, which is what internal/clientrelease parses.
SHA256SUM := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo shasum -a 256)

# The console is checked-in source and is always embedded. There is no build tag,
# no bundler and no Node: `go build` produces the whole shipped program.

.PHONY: all build release client-build client-release client-install client-publish bridge-release app app-windows shim shim-linux shim-windows shim-darwin pkgcache-linux pkgcache-windows windows-installer test race lint fmt tidy clean help

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
	cd bin && $(SHA256SUM) pkgreg-client-linux-* pkgreg-client-darwin-* \
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
	cd bin && $(SHA256SUM) pkgreg-bridge-linux-* pkgreg-bridge-darwin-* \
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

# Windows cross-compiles, and this is the one platform where it can.
#
# Wails' Windows backend reaches WebView2 through COM rather than a C wrapper, so it is
# pure Go: CGO_ENABLED=0 and the .exe builds on any host. Linux and macOS both need their
# platform's toolchain and are built natively.
#
# -H windowsgui, or the app flashes a console window every time it opens — including once
# per login, from the startup entry, which is exactly the sort of thing somebody turns off
# and never turns back on.
shim-windows: ## Cross-compile the docker shim for Windows
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
	  -ldflags '-s -w $(LDFLAGS)' \
	  -o bin/pkgcache-docker-windows-amd64.exe ./cmd/pkgcache-docker

app-windows: ## Cross-compile the desktop app for Windows
	cd cmd/pkgcache-app && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
	  -ldflags '-s -w -H windowsgui $(LDFLAGS)' \
	  -o ../../bin/pkgcache-app-windows-amd64.exe .

# The two Linux binaries the .deb is built from, as their own target so `make deb` can
# depend on them. Before this it only checked they existed, which meant `git pull && make
# deb` happily packaged yesterday's binary and the new command was missing from the
# package with nothing said about why.
# The docker-compatible shim, built for every target pkgcache itself is. It is pure Go
# and CGO_ENABLED=0 like the daemon: it drives docker, it does not link anything.
shim: ## Build the docker-compatible shim for this host
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' \
		-o bin/pkgcache-docker ./cmd/pkgcache-docker

# The names the .deb looks for, which is why this exists separately from `shim` above.
#
# It was a plain mismatch before: `shim` wrote bin/pkgcache-docker and the packaging
# looked for bin/pkgcache-docker-linux-<arch>, so the shim was never packaged and the
# installed .deb had no pkgcache-docker in it. Nothing said so, because the packaging
# treats a missing shim as "this host did not build one" — which is right for the app and
# wrong for this, since it is pure Go and cross-compiles anywhere.
shim-linux: ## Cross-compile the shim for Linux, both architectures
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgcache-docker-linux-amd64 ./cmd/pkgcache-docker
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgcache-docker-linux-arm64 ./cmd/pkgcache-docker

# The same shim for macOS, which the .pkg ships beside pkgcache itself.
#
# Its absence was the one real asymmetry between the three installers: Windows and the
# .deb both carried the shim and the .pkg did not, so `crate prepare --runtime
# pkgcache-docker` — the case this program was written for — had no Mac story at all.
# Pure Go like the other two, so it cross-compiles from here and needs no Mac.
shim-darwin: ## Cross-compile the shim for macOS, both architectures
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgcache-docker-darwin-amd64 ./cmd/pkgcache-docker
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgcache-docker-darwin-arm64 ./cmd/pkgcache-docker

pkgcache-linux: ## Cross-compile pkgcache for Linux, both architectures
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgcache-linux-amd64 ./cmd/pkgcache
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgcache-linux-arm64 ./cmd/pkgcache

pkgcache-windows: ## Cross-compile pkgcache itself for Windows
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
		-ldflags '-s -w $(LDFLAGS)' -o bin/pkgcache-windows-amd64.exe ./cmd/pkgcache

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
	cd bin && $(SHA256SUM) pkgcache-linux-* pkgcache-darwin-* \
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
# The Windows installer, which cross-builds completely: makensis runs on Linux and both
# binaries are pure Go for that target. Nothing about the Windows client needs a Windows
# machine until somebody wants to run it.
#
# LICENSE and NOTICE are staged as .txt: they are shown on the installer's licence page
# and installed beside the binaries, and an extensionless file on Windows is one that
# nothing opens when it is double-clicked.
windows-installer: pkgcache-windows app-windows shim-windows ## Build the Windows installer (needs makensis)
	@command -v makensis >/dev/null || { \
		echo "makensis is missing: dnf install mingw32-nsis, or apt install nsis" >&2; \
		exit 1; }
	cp bin/pkgcache-windows-amd64.exe ../packaging/windows/pkgcache.exe
	cp bin/pkgcache-app-windows-amd64.exe ../packaging/windows/pkgcache-app.exe
	cp bin/pkgcache-docker-windows-amd64.exe ../packaging/windows/pkgcache-docker.exe
	cp ../LICENSE ../packaging/windows/LICENSE.txt
	cp ../NOTICE ../packaging/windows/NOTICE.txt
	cd ../packaging/windows && makensis -DVERSION=$(VERSION) pkgcache.nsi
	rm -f ../packaging/windows/pkgcache.exe ../packaging/windows/pkgcache-app.exe \
		../packaging/windows/pkgcache-docker.exe \
		../packaging/windows/LICENSE.txt ../packaging/windows/NOTICE.txt
	mv ../packaging/windows/pkgcache-*-setup.exe bin/
	@echo "built bin/pkgcache-$(VERSION)-setup.exe"

deb: pkgcache-linux shim-linux ## Build Debian packages (pkgcache and pkgcache-desktop) for amd64 and arm64
	@for arch in amd64 arm64; do \
		app="bin/pkgcache-app-linux-$$arch"; \
		if test -f "$$app"; then \
			echo "note: packaging the desktop app from $$app"; \
			echo "      it is not rebuilt here — run \`make app\` if it is stale"; \
		else \
			echo "note: no $$app, so only the daemon package for $$arch"; \
			app=""; \
		fi; \
		shim="bin/pkgcache-docker-linux-$$arch"; \
		if test ! -f "$$shim"; then \
			echo "note: no $$shim, so the package has no docker shim"; \
			shim=""; \
		fi; \
		PKGCACHE_APP="$$app" PKGCACHE_SHIM="$$shim" sh ../packaging/deb/build.sh \
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
