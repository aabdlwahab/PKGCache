# Porting an existing Dockerfile to the cache

The same walkthrough as a scroll-through page — the file assembling itself one
package source at a time — is served by the console at **`/tutorial`**
(source: [`go/internal/web/dist/tutorial.html`](../../go/internal/web/dist/tutorial.html)).

Two files, one diff:

```bash
diff examples/porting/Dockerfile.before examples/porting/Dockerfile
```

[`Dockerfile.before`](Dockerfile.before) is an ordinary image that pulls from Docker
Hub, deb.debian.org, pypi.org, registry.npmjs.org and github.com.
[`Dockerfile`](Dockerfile) is the same file pointed at package-registry.

The point of the diff: **your build steps do not change.** You add a preamble — a
base-image prefix, four env vars, an apt proxy line — and `pip install`,
`npm install` and `git clone` keep working exactly as written.

## Try it

```bash
docker build -t myapp --build-arg HOST=<cache-host> examples/porting
docker run --rm myapp
```

Nothing to copy into the build context. `<cache-host>` can be a name or a bare IP.

## The four changes

| # | What | Change |
|---|---|---|
| 1 | base images | `FROM python:3.12-slim` → `FROM HOST:8443/dockerhub/library/python:3.12-slim` |
| 2 | pip + uv + npm | `PIP_INDEX_URL` + `PIP_TRUSTED_HOST`, `UV_INDEX_URL` + `UV_INSECURE_HOST`, `NPM_CONFIG_REGISTRY` + `NPM_CONFIG_STRICT_SSL=false` |
| 3 | apt | write `Acquire::http::Proxy "http://global:x@HOST:3142"`, force `http` mirror lines |
| 4 | git | `http.<cache-url>.sslVerify false`, plus `url.insteadOf` so hardcoded `github.com` URLs redirect themselves |

Not used by this example, but the same idea:

| Ecosystem | Point it at |
|---|---|
| apk (Alpine) | `http_proxy=http://global:x@HOST:3142` and `sed -i 's\|https\|http\|' /etc/apk/repositories` |
| PyTorch wheels | swap the index: `.../pypi/root/pytorch-cu124/+simple/` |
| files (artifacts) | `wget --no-check-certificate https://HOST:8443/global/files/<path>` |

## No root, no `ca.crt`, no Dockerfile

Everything above writes to `/etc`, names a cert and lives in a file you can edit.
None of the three is required — inside an image build you are root anyway, and on a
locked-down machine every client also takes per-user config or a bare env var:

| Source | The `/etc` form | The same thing, unprivileged |
|---|---|---|
| base images | daemon config **(root)** | rootless Docker: `~/.config/docker/daemon.json` · podman: `--tls-verify=false` |
| pip | *never needed root* | `PIP_INDEX_URL` + `PIP_TRUSTED_HOST`, or `pip config --user set global.index-url …` |
| uv | *never needed root* | `UV_INDEX_URL` + `UV_INSECURE_HOST`, or `~/.config/uv/uv.toml` |
| npm | *never needed root* | `NPM_CONFIG_REGISTRY`, or `npm config set registry …` (`~/.npmrc`) |
| apt | `/etc/apt/apt.conf.d/01pkgcache` **(root)** | `APT_CONFIG=~/apt.conf`, `-o Acquire::http::Proxy=…`, or `http_proxy=` |
| apk | `/etc/apk/repositories` **(root)** | `apk --repositories-file ~/repositories` + `http_proxy=` |
| git | `git config --system` **(root)** | `--global`, `git -c …`, or `GIT_CONFIG_COUNT` + `GIT_CONFIG_KEY_n` (git ≥ 2.31) |
| files | *never needed root* | `wget --no-check-certificate`, `curl -k` |

Two caveats: apt and apk still need root to **install** — that is the package manager,
not the cache, and the table only moves its *configuration*. And `http_proxy` (never
`https_proxy`) is the right variable: `:3142` is plain HTTP and does not tunnel TLS.

**No `ca.crt`?** You can fetch it yourself — the console serves the CA anonymously
(it is public trust material, not a credential), so you never need access to the cache
host:

```bash
curl -fLo pkgcache-ca.crt http://<console-host>:8088/api/ca.crt
```

The skip-verify route below is still the right answer in one case: when the cache is
reached by a **bare IP**, where pip before v26 cannot verify the connection no matter
which CA you give it.

**Can't edit the Dockerfile?** Docker resolves `FROM` against the local image store
first, so build the tag it already names, with the preamble baked in — and inside
*that* build you are root, so it can write the `/etc` files the real build inherits:

```bash
docker build -t python:3.12-slim - <<'EOF'
FROM python:3.12-slim
ENV PIP_INDEX_URL=https://<cache-host>:8443/global/pypi/root/pypi/+simple/ \
    PIP_TRUSTED_HOST=<cache-host>:8443 \
    NPM_CONFIG_REGISTRY=https://<cache-host>:8443/global/npm/ \
    NPM_CONFIG_STRICT_SSL=false
RUN git config --system url."https://<cache-host>:8443/global/git/github.com/".insteadOf "https://github.com/"
EOF

docker build -t myapp .          # their file, untouched, now on the cache
```

The base image still comes from Hub — that is what makes it work with no daemon change
and no cert. `docker build --pull` discards the shadow, so rebuild it in the same CI
job as the real build. The console's `/tutorial` page has the full version of all
three, including the `GIT_CONFIG_*` env block.

## TLS: skip-verify vs. the CA

The cache terminates HTTPS with a private CA, so every client either has to trust
that CA — `certs/ca.crt` in this repo, or `GET /api/ca.crt` from the console for
anyone who does not have the repo — or be told to trust this one host without
verifying. This example
takes the second route, because it is one env var per client, needs no file in the
build context, and works when the cache is reached by IP. It suits the trusted,
isolated networks this stack is built for — the same posture as the no-auth console
and the open apt proxy.

If you want verification on instead, swap the flags for the CA equivalents and copy
`certs/ca.crt` into your build context:

```dockerfile
COPY ca.crt /usr/local/share/ca-certificates/pkgcache.crt
RUN update-ca-certificates                      # covers wget/curl/uv, not pip/npm/git
ENV PIP_CERT=/usr/local/share/ca-certificates/pkgcache.crt \
    NPM_CONFIG_CAFILE=/usr/local/share/ca-certificates/pkgcache.crt \
    GIT_SSL_CAINFO=/usr/local/share/ca-certificates/pkgcache.crt
```

…and drop `PIP_TRUSTED_HOST`, `NPM_CONFIG_STRICT_SSL` and the `sslVerify` line.
One catch, and the reason this example does not lead with the CA: **pip before v26
cannot use an IP-literal HTTPS index at all**, CA or no CA. Its vendored urllib3
omits SNI for IP addresses and pip's trust backend then refuses the socket:

```
ValueError: check_hostname requires server_hostname
```

`PIP_CERT` does not help — it is not a trust failure. With the CA route you need a
DNS name in the cert, pip ≥ 26, or `PIP_TRUSTED_HOST` anyway.

## Named projects

Everything above assumes the default `global` project. For a named project the
address changes in three different ways, which is why `PROJECT` is a build arg:

- **path roles** (pip, npm, git, files): `/<project>/<role>/…`
- **OCI**: the project rides the image name — `HOST:8443/<project>/dockerhub/…`
- **apt/apk**: the project is the proxy username — `http://<project>:x@HOST:3142`

```bash
docker build -t myapp --build-arg HOST=<cache-host> --build-arg PROJECT=projA \
  --build-arg BASE_IMAGE=<cache-host>:8443/projA/dockerhub/library/python:3.12-slim \
  examples/porting
```

## The one gotcha you cannot flag your way out of

**The `FROM` line is pulled by the Docker daemon, not by your build**, so no
build-time flag or env var reaches it. Once per build host, either trust the CA:

```bash
curl -fLo ca.crt http://<console-host>:8088/api/ca.crt   # or use certs/ca.crt
sudo mkdir -p /etc/docker/certs.d/<cache-host>:8443
sudo cp ca.crt /etc/docker/certs.d/<cache-host>:8443/ca.crt        # no restart needed
```

Rootless Docker keeps that folder in your home directory instead
(`~/.config/docker/certs.d/…`), so it needs no `sudo`.

**Docker Desktop (macOS/Windows) ignores `/etc/docker/certs.d` entirely** — the daemon
lives in a Linux VM, so that path on your own machine is never read, and `docker pull`
then fails with `x509: certificate signed by unknown authority` exactly as if you had
installed nothing. Trust the CA in the host's certificate store instead, and restart
Docker Desktop so it rebuilds the VM's trust list:

```bash
# macOS
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain ca.crt
osascript -e 'quit app "Docker"'; sleep 5; open -a Docker
```

Or skip the certificate for the daemon only: Docker Desktop → Settings → Docker Engine
→ `"insecure-registries": ["<cache-host>:8443"]` → Apply & Restart.

or add `"insecure-registries": ["<cache-host>:8443"]` to `/etc/docker/daemon.json`
(that one needs a daemon restart). Either way, one entry covers every project —
they share the port. If you cannot change the daemon at all, build with
`--build-arg BASE_IMAGE=python:3.12-slim` to pull that single image from Hub while
everything else still comes from the cache.

## Related

- [`examples/Dockerfile`](../Dockerfile) — the same six roles as an extensive smoke
  test where the build *is* the test suite. It deliberately uses the CA route (and
  asserts it works), since verifying that path is part of what it tests.
- [`examples/six-repositories/`](../six-repositories/) — a reusable client image that
  asserts at container runtime.
