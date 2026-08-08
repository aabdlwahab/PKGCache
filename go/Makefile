# pkgreg — build, test, lint.
#
# Release builds are CGO-free so the result is a genuinely static binary that runs
# on any Linux with a compatible kernel — no libc, no OpenSSL, nothing to install.

BIN        := pkgreg
PKG        := github.com/brightskies/pkgreg
BUILDINFO  := $(PKG)/internal/buildinfo
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE       ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X $(BUILDINFO).version=$(VERSION) \
           -X $(BUILDINFO).commit=$(COMMIT) \
           -X $(BUILDINFO).date=$(DATE)

# The console is checked-in source and is always embedded. There is no build tag,
# no bundler and no Node: `go build` produces the whole shipped program.

.PHONY: all build release client-build client-release client-install client-publish bridge-release test race lint fmt tidy clean help

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
