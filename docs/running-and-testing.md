# Running and testing pkgreg, with and without the client bridge

Written 2026-07-29. Every command below was executed against a live instance while
writing this page; the outputs quoted are the ones observed, not illustrations.

There are two ways a client reaches the cache, and this page covers both end to end:

| | **Direct** | **Via the bridge** |
|---|---|---|
| Client talks to | `https://cache:8443` | `http://127.0.0.1:41999` |
| Needs the CA in a trust store | yes | no |
| Needs `--trusted-host` | no | no |
| Needs root | yes, for the system store and `/etc/docker/certs.d` | no |
| Works inside a container | yes | no — see [Containers](#5-containers) |
| Extra process to supervise | none | one per developer machine |

The bridge is an alternative, not a replacement. If it does not fit, the direct path is
the fallback and nothing needs undoing first — the bridge never writes to the machine.

---

## 1. Build

```sh
cd go
make build                       # bin/pkgreg
make client-build                # bin/pkgreg-client for this host
make client-release              # five pkgreg-client binaries + checksums
```

The console is checked-in source and is always compiled in, so `make build` already
produces the whole program. There is no Node, no bundler and no build tag.

For stripped, reproducible release binaries:

```sh
make release                     # linux amd64 + arm64
make client-release              # linux/macOS/windows + SHA256SUMS
```

Both are `CGO_ENABLED=0`, so the results are static and need nothing installed on the
target host.

## 2. Start a cache

```sh
export DATA=/tmp/pkgreg-demo
./bin/pkgreg init -data-dir "$DATA" -hostnames localhost,127.0.0.1
make client-publish DATA_DIR="$DATA"
```

`init` mints the CA and a server certificate, and writes `$DATA/pkgreg.yaml`.
`client-publish` builds all five supported client targets and then runs
`pkgreg publish-client`, which copies them into `$DATA/downloads` and records their
digests; building into `go/bin` alone does not publish them. On a host with no Go
toolchain, run `pkgreg publish-client` directly against copied release files.
Point the server at test ports so it cannot collide with a real deployment:

```sh
./bin/pkgreg serve -config "$DATA/pkgreg.yaml" \
  -unified-addr 127.0.0.1:48443 -single-port
```

`single_port` is the default: one address carries TLS (npm, PyPI, OCI, git, files) and
plain HTTP (the apt/apk forward proxy), separated by sniffing the first byte. So the
apt proxy is the *same* port, not `proxy_addr`.

Confirm it is up. Everything from here needs the CA, which is why the rest of this page
exists:

```sh
curl --cacert "$DATA/certs/ca.crt" https://127.0.0.1:48443/readyz
# {"checks":{"blobs":"ok","catalog":"ok","control":"ok","listeners":"ok"},"ready":true}
```

Useful extras:

```sh
./bin/pkgreg doctor -config "$DATA/pkgreg.yaml"   # config, DBs, TLS, git, ulimits
openssl x509 -in "$DATA/certs/ca.crt" -noout -fingerprint -sha256
```

`doctor` reports how many console files are embedded; on any successful build that is
the whole console, because there is no longer a way to build without it. To run the
server with no browser surface at all, pass `--headless` — the API, metrics, health and
the data plane stay up, and every console path answers 404.

That fingerprint is what the bridge pins, and it is the value the console shows beside
the CA download.

---

## 3. Default path — a temporary pkgreg-client shell

Download `pkgreg-client` from the tutorial or Console → Connect and compare the CA
fingerprint through a trusted channel. The normal command starts a verified localhost
bridge and a configured child shell:

```sh
chmod +x ./pkgreg-client
./pkgreg-client -server https://cache:8443 -ca-sha256 "$FP"
# run package commands, then type:
exit
```

The child shell points pip, uv and npm at an ephemeral `127.0.0.1` listener and
provides temporary addresses for Git, files, and the apt/apk proxy. The bridge
verifies the remote TLS connection against the fingerprint-pinned CA. Exiting stops
the listener and discards the environment; no system trust, hosts file, or package
configuration changes. Docker is daemon-side: localhost works only where that daemon
can reach the host loopback (normally native Linux), not reliably with Docker Desktop
or a remote builder.

For a project whose data plane requires a token, save the one-time read token in a
protected file and let the temporary bridge attach it:

```sh
printf '%s\n' "$PKGREG_TOKEN" > ./pkgreg.token
chmod 600 ./pkgreg.token
./pkgreg-client -server https://cache:8443 -project team-a \
  -ca-sha256 "$FP" -token-file ./pkgreg.token
```

Do not pass the token itself as a command-line argument; command lines may be visible
to other processes. Persistent mode intentionally does not store project tokens.

For a managed build host that must keep the setup, select it explicitly:

```sh
./pkgreg-client --persist -server https://cache:8443 -ca-sha256 "$FP" -dry-run
sudo ./pkgreg-client --persist -server https://cache:8443 -ca-sha256 "$FP"
source /etc/pkgreg/projects/global/env.sh
```

Only `--persist` downloads and runs the readable setup script. The generated
`setup.sh` and `setup.ps1` remain available from Connect for audit.

To exercise the same thing locally without touching the machine, set the per-tool
variables by hand. Each tool has its own way of being told about a CA:

```sh
export CA="$DATA/certs/ca.crt"
export BASE=https://127.0.0.1:48443
```

### pypi — pip

```sh
PIP_CERT="$CA" pip install --index-url $BASE/global/pypi/root/pypi/+simple/ six
# Successfully installed six-1.17.0
```

Without the CA it fails closed, which is the behaviour to expect:

```sh
pip install --index-url $BASE/global/pypi/root/pypi/+simple/ six
# SSLError: [SSL: CERTIFICATE_VERIFY_FAILED] unable to get local issuer certificate
```

`--trusted-host` is **not** needed here. It marks a host as trusted and can disable
transport checks; the CA is the correct way to authenticate a private HTTPS index.

### pypi — uv

```sh
SSL_CERT_FILE="$CA" uv pip install --native-tls \
  --index-url $BASE/global/pypi/root/pypi/+simple/ six
# + six==1.17.0
```

### npm

```sh
npm install --registry $BASE/global/npm/ --cafile "$CA" left-pad
```

### git

```sh
GIT_SSL_CAINFO="$CA" git clone $BASE/global/git/github.com/octocat/Hello-World.git
# without it: SSL certificate problem: unable to get local issuer certificate
```

### files

```sh
TOKEN=$(curl -s --cacert "$CA" -X POST $BASE/api/v1/tokens \
  -H 'Content-Type: application/json' \
  -d '{"project":"global","eco":"files","scope":"write","label":"demo"}' \
  | python3 -c 'import json,sys;print(json.load(sys.stdin)["secret"])')

curl --cacert "$CA" -X PUT -H "Authorization: Bearer $TOKEN" \
  --data-binary @hello.txt $BASE/global/files/docs/hello.txt     # 201
curl --cacert "$CA" $BASE/global/files/docs/hello.txt            # 200
```

Uploads are write-once; add `?overwrite=1` to replace.

### docker

Docker cannot be given a CA per command. Temporary mode avoids that by using its
loopback bridge. With `pkgreg-client --persist`, the client writes the `certs.d` file.
The equivalent manual check is:

```sh
sudo install -Dm644 "$CA" /etc/docker/certs.d/cache:8443/ca.crt
docker pull cache:8443/dockerhub/library/alpine:3.20
```

Docker Engine reads registry CAs from this directory without a daemon restart.
Docker Desktop users should restart Docker Desktop after changing the host trust
store. This is the single most awkward step in client onboarding, and the main thing
the bridge removes.

### apt and apk

These need no CA in either mode: the forward proxy is plain HTTP by protocol.

```sh
echo 'Acquire::http::Proxy "http://127.0.0.1:48443";' > /etc/apt/apt.conf.d/01proxy
apt-get update && apt-get install -y jq
# Fetched 9359 kB in 8s ... jq-1.6
```

For a named project the project rides the proxy username, so with the single-port
default that is `http://myproject@127.0.0.1:48443`. With explicit listeners it is
`proxy_addr` instead, default `:3142`.

---

## 4. Path B — via the bridge, no CA anywhere

Start the bridge as an ordinary user. Nothing here is privileged and nothing is
written to the machine:

```sh
FP=$(openssl x509 -in "$DATA/certs/ca.crt" -noout -fingerprint -sha256 | cut -d= -f2)

./bin/pkgreg-bridge -server https://127.0.0.1:48443 -ca-sha256 "$FP" -check
#   readiness      ok    HTTP 200
#   pypi index     ok    HTTP 404
#   npm root       ok    HTTP 404
#   OCI registry   ok    HTTP 200

./bin/pkgreg-bridge -server https://127.0.0.1:48443 -ca-sha256 "$FP"
```

`-check` exits non-zero and prints the setup-script command when the bridge cannot
work. `-ca "$DATA/certs/ca.crt"` is equivalent if you already have the file. A wrong
fingerprint fails closed:

```text
CA fingerprint mismatch: the cache offered 17:EB:…:F5, you pinned 28:FC:…:06.
Either the fingerprint is wrong or this is not the cache you meant to reach
```

Apply the settings it prints:

```sh
eval "$(./bin/pkgreg-bridge -server https://127.0.0.1:48443 -print-env)"
# export PKGREG_SESSION=temporary
# export PKGREG_BRIDGE_URL=http://127.0.0.1:41999
# export PKGREG_DOCKER_REGISTRY=127.0.0.1:41999
# export PKGREG_GIT_URL=http://127.0.0.1:41999/global/git
# export PIP_INDEX_URL=http://127.0.0.1:41999/global/pypi/root/pypi/+simple/
# export UV_DEFAULT_INDEX=http://127.0.0.1:41999/global/pypi/root/pypi/+simple/
# export NPM_CONFIG_REGISTRY=http://127.0.0.1:41999/global/npm/
# export PKGREG_FILES_URL=http://127.0.0.1:41999/global/files/
```

Note what is absent: no `PIP_CERT`, no `NODE_EXTRA_CA_CERTS`, no `GIT_SSL_CAINFO`, no
`--trusted-host`. Use `-shell powershell` for the PowerShell form.

Now every client works with **no certificate configuration at all**:

```sh
B=http://127.0.0.1:41999

pip install --index-url $B/global/pypi/root/pypi/+simple/ six
# Successfully installed six-1.17.0

uv pip install --index-url $B/global/pypi/root/pypi/+simple/ six
# + six==1.17.0

npm install --registry $B/global/npm/ left-pad
# added 1 package

git clone $B/global/git/github.com/octocat/Hello-World.git
# no TLS flags of any kind

docker pull 127.0.0.1:41999/dockerhub/library/alpine:3.20
docker run --rm 127.0.0.1:41999/dockerhub/library/alpine:3.20 cat /etc/alpine-release
# 3.20.10
```

On the native Linux Docker engine used for this test, `docker pull` needs no
`certs.d`, `daemon.json`, or daemon restart because `127.0.0.0/8` is in that
daemon's default insecure-registry list. This does not apply to Docker Desktop or a
remote daemon. Check a native daemon with:

```sh
docker info | grep -A3 'Insecure Registries'
```

apt and apk keep using the forward proxy exactly as in Path A. The bridge does not
serve them and does not need to.

### Letting the bridge hold the token

For a token-gated project, give the credential to the bridge instead of copying it into
`.npmrc`, `pip.conf` and a CI variable separately:

```sh
echo "$TOKEN" > ~/.pkgreg-token && chmod 600 ~/.pkgreg-token
./bin/pkgreg-bridge -server https://127.0.0.1:48443 -ca-sha256 "$FP" \
  -token-file ~/.pkgreg-token
```

The client then needs no credentials at all:

```sh
curl -X PUT --data-binary @hello.txt $B/global/files/docs/via-bridge.txt   # 201
```

Without `-token-file` the same request is a 401, and a caller that supplies its own
`Authorization` header always wins over the bridge's.

### Reverting

Stop the process and unset the variables. There is nothing to uninstall.

---

## 5. Containers

The bridge listens on the host's loopback, and `localhost` inside a container is the
container. Measured:

```sh
docker run --rm debian bash -c 'exec 3<>/dev/tcp/127.0.0.1/41999 && echo REACHABLE'
# bash: /dev/tcp/127.0.0.1/41999: Connection refused

docker run --rm --network host debian bash -c 'exec 3<>/dev/tcp/127.0.0.1/41999 && echo REACHABLE'
# REACHABLE
```

So for containers and `docker build`, either use `--network host`, or use the direct
path: copy the public CA into the image, refresh the image trust store, and pass the
client-created package index settings as build arguments. The public tutorial shows a
complete Dockerfile example. CI is the case where the direct path is genuinely
easier.

---

## 6. Automated tests

From `go/`:

```sh
make test                # go test ./...
make race                # go test -race ./...   mandatory before merge
go vet ./...
make lint                # golangci-lint, must be 0 issues
```

Both suites are quick because almost everything is in-process: on a 112-core host
`go test ./...` took 9.0 s and `go test -race ./...` 13.4 s from a cold cache. If they
take minutes, something is reaching the network that should not be.

The console has no separate toolchain, so it has no separate gates. Its two checks run
inside `go test ./internal/web`: that every module is present in the embedded tree, and
that every import in the HTML and in the modules resolves to a file that is actually
embedded — which is what a blank console page would otherwise look like.

### What the suite actually covers

| Suite | Path | Notes |
|---|---|---|
| Unit and integration | alongside each package | synthetic upstream with failure injection |
| `catalog.Store` contract | `internal/catalog/storetest` | runs through the interface, not the SQLite type |
| Client acceptance | `test/acceptance` | real `pip`, `uv`, `npm`, `docker`, `apt`, `apk`, `wget` |
| Bridge | `cmd/pkgreg-bridge` | rewrite rules, HEAD, digest-committed bodies, pinning |
| Differential | `test/differential` | opt-in; needs a live Python deployment, which this repository no longer contains — useful only against one still running |
| Load | `test/load` | opt-in; 2 GiB / 20 readers |
| Privileged onboarding | `test/onboardingos` | opt-in; installs a CA, so it needs root |

Skips are silent by design when a client is missing. Check what actually ran:

```sh
go test ./test/acceptance/ -v | grep -E '^(---|===) '
```

Opt-in suites:

```sh
go test ./test/load -run TestM2 -args -m2-2gb
sudo env PKGREG_ONBOARDING_OS_ACCEPTANCE=1 go test -count=1 ./test/onboardingos
go test ./test/differential -python https://… -go https://… -var …
```

### Fuzzing

Four targets, corresponding to the four parsers that read untrusted input:

```sh
go test ./internal/blob      -run FuzzDigestPathSafety   -fuzz FuzzDigestPathSafety   -fuzztime 60s
go test ./internal/router    -run FuzzPathResolution     -fuzz FuzzPathResolution     -fuzztime 60s
go test ./internal/snapshot  -run FuzzManifestParsing    -fuzz FuzzManifestParsing    -fuzztime 60s
go test ./internal/eco/pypi  -run FuzzParseSimpleHTML    -fuzz FuzzParseSimpleHTML    -fuzztime 60s
```

The PyPI target found a real panic on its first run: a stray non-UTF-8 byte in an index
desynchronised the HTML scanner's offsets. Run these when touching a parser.

---

## 7. Verifying the bridge is faithful

The bridge must not alter what the cache serves. These are the checks worth repeating
after changing it:

```sh
# byte-identical artifacts
curl -s -o via-bridge.whl  $B/global/pypi/root/pypi/+f/six/six-1.17.0-py2.py3-none-any.whl
curl -s -o via-direct.whl  --cacert "$CA" \
  $BASE/global/pypi/root/pypi/+f/six/six-1.17.0-py2.py3-none-any.whl
sha256sum via-*.whl | awk '{print $1}' | uniq -c      # expect: 2 <one hash>

# ranges survive
curl -s -r 100-199 -o /dev/null -w '%{http_code} %{size_download}\n' \
  $B/global/pypi/root/pypi/+f/six/six-1.17.0-py2.py3-none-any.whl
# 206 100

# large artifacts stream rather than buffer
PID=$(ss -lptn 'sport = :41999' | grep -oP 'pid=\K[0-9]+' | head -1)
awk '/VmRSS/{print $2}' /proc/$PID/status      # before
curl -s -o /dev/null $B/global/files/big.bin   # a 400 MB object
awk '/VmRSS/{print $2}' /proc/$PID/status      # after: 9.9 MB -> 12.2 MB observed
```

If that last figure tracks the artifact size, something has started buffering and the
content-type rules in `rewriteBody` need looking at.

---

## 8. Teardown

```sh
kill $(ss -lptn 'sport = :41999' | grep -oP 'pid=\K[0-9]+')   # bridge
kill $(ss -lptn 'sport = :48443' | grep -oP 'pid=\K[0-9]+')   # cache
rm -rf /tmp/pkgreg-demo
docker image rm -f 127.0.0.1:41999/dockerhub/library/alpine:3.20
```

The default client shell and standalone bridge leave nothing. If you explicitly used
the persistent mode, preview and remove it with:

```sh
./pkgreg-client --persist -server https://cache:8443 -ca-sha256 "$FP" \
  -uninstall -dry-run
sudo ./pkgreg-client --persist -server https://cache:8443 -ca-sha256 "$FP" \
  -uninstall
```

---

## Related

- [client-bridge.md](client-bridge.md) — how the bridge works and when not to use it
- [client-onboarding.md](client-onboarding.md) — temporary and persistent client design
