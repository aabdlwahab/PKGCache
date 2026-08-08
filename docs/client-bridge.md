# pkgreg-bridge — the standalone temporary localhost transport

Status: implemented 2026-07-29. Verified end to end against a live cache with real
`pip`, `npm` and native Linux `docker` clients.

`pkgreg-client` now uses this bridge internally by default: it starts an ephemeral
loopback listener, opens a configured child shell, and stops the listener when that
shell exits. Most developers should use that simpler flow.

`pkgreg-bridge` remains available as a standalone program for users who want to choose
a fixed port, manage the process lifetime themselves, or attach a project token. It
listens on `127.0.0.1` and carries requests to the cache over verified TLS. Tools are
pointed at `http://127.0.0.1:41999` instead of `https://cache:8443`, which removes two
pieces of privileged setup:

- pip's `--trusted-host`, and
- the CA in the machine trust store.

On native Linux, the same bridge can also avoid `/etc/docker/certs.d`. Docker Desktop
and remote daemons do not share the terminal's loopback, so that is not a portable
Docker setup.

When loopback does not fit, use explicit `pkgreg-client --persist`. Because the bridge
never writes to the machine, there is nothing to undo first.

## Why loopback is the trick

The shell clients in this system accept loopback as a local origin. Native Linux
Docker does too, but Docker Desktop and remote daemons have a different loopback.
Measured against the clients themselves:

| Client | plain HTTP to a named host | plain HTTP to `127.0.0.1` |
|---|---|---|
| `pip` | repository **ignored** unless `--trusted-host` is passed | works, no flag, no warning |
| native Linux `docker` | needs `/etc/docker/certs.d/<host>/ca.crt` | `127.0.0.0/8` is in the daemon's default insecure-registry list |
| Docker Desktop / remote daemon | needs managed direct-registry trust | terminal loopback is not the daemon's loopback |
| `npm` | works | works |
| `uv` | works | works |
| `apt` / `apk` | already plain HTTP through the proxy | unchanged |

The bridge is essential for an unprivileged, verified pip session and can also serve
native Linux Docker. For npm, uv, git and apt it mainly provides one consistent
temporary session.

## What it does not do

It does not remove the certificate. It moves the trust anchor out of the system store
and into one process an ordinary user controls. The benefit is that no step needs root
and that rotating the cache's certificate no longer touches clients at all — not that
there is one less thing to verify.

## Use

```sh
# one-off: does the bridge work against this cache, from this machine?
pkgreg-bridge -server https://cache.internal:8443 -ca-sha256 <fingerprint> -check

# run it, then apply the settings it prints
pkgreg-bridge -server https://cache.internal:8443 -ca-sha256 <fingerprint>
eval "$(pkgreg-bridge -server https://cache.internal:8443 -print-env)"
```

The fingerprint is the SHA-256 the console shows beside the CA download, and is the
same value as `openssl x509 -in ca.crt -noout -fingerprint -sha256`. Pass `-ca ca.crt`
instead if you already have the file.

Trust is established once, at startup: the bridge fetches `/api/ca.crt` over an
unverified connection, refuses to continue unless it matches the pinned fingerprint,
and then verifies every real request against it normally — signature, dates and host
name included. That exchange carries no credential and a certificate is public, so an
attacker in the middle can only serve a CA whose fingerprint will not match.

For a token-gated project, `-token-file` keeps the credential in the bridge and
attaches it to each request, so it stops being copied into `.npmrc`, `pip.conf` and a
CI variable separately.

## When to fall back

`-check` exits non-zero when the cache is unreachable or the pin does not match. Do
not bypass a failed fingerprint check. Use `pkgreg-client --persist` deliberately for
these environments:

- **Containers and `docker build`.** `localhost` inside a container is not the host, so
  the bridge is invisible there. CI images should install the public CA, refresh the
  image trust store, and receive the client-created package settings as build
  arguments. The public tutorial points this case to the explicit persistent setup.
- **Docker Desktop on macOS and Windows**, where the daemon runs in a VM and loopback
  does not mean the same thing. Use the direct registry with managed CA trust.
- **Shared or multi-user hosts.** The loopback socket is not authenticated, so any
  local user can drive the bridge and use its token. Binding to `127.0.0.1` limits the
  network, not the machine.
- **Unattended machines**, where the configuration must survive process exits and reboots.

## How it works

The bridge sends the client's `Host: 127.0.0.1:41999` upstream unchanged. The cache
builds every URL it advertises from that header, so a PyPI index, an npm packument,
a files listing and a Git LFS action all come back pointing at the bridge rather than
at a server name this machine has no reason to trust. That is what lets the bridge stay
free of ecosystem knowledge.

One thing does have to be adjusted. The cache answers over TLS, so it writes `https`
into those URLs; the bridge speaks plain HTTP on loopback. It therefore rewrites
exactly one literal token — its own origin — in textual responses. This is a string
substitution, not an understanding of any package format.

Three rules keep that from doing damage:

- **Only textual content types.** A wheel or an image layer streams through untouched.
  A 400 MB artifact was measured moving the bridge's RSS from 9.9 MB to 12.2 MB.
- **Never a HEAD.** Its `Content-Length` is the answer, not a description of a body.
  Docker asks for a manifest by HEAD to learn its size, and recomputing that from an
  empty body reports zero and fails the pull.
- **Never a digest-committed body.** A response carrying `Docker-Content-Digest` is
  verified byte-for-byte by the client, so it is not the bridge's to touch.

## Verified

Against a live cache, with no CA installed anywhere on the machine and no
`--trusted-host`:

```text
pip install six                    Successfully installed six-1.17.0
npm install left-pad               added 1 package
docker pull …/library/alpine:3.20  Downloaded, runs, reports 3.20.10
wheel via bridge vs direct         byte-identical (sha256 match)
Range: bytes=100-199               206, 100 bytes
400 MB artifact                    streamed; bridge RSS 9.9 MB -> 12.2 MB
```
