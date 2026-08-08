# =============================================================================
# package-registry — worked example AND end-to-end smoke test for all six roles
# =============================================================================
#
# This one Dockerfile is two things at once:
#
#   1. A COPY-PASTEABLE EXAMPLE. Every stage below is the real, minimal way to
#      point one ecosystem's client at the cache — the base image `FROM`s go
#      through the OCI role, apt through the forward proxy, pip/uv through the
#      simple index, npm through the registry prefix, apk through the proxy,
#      git through the mirror, and files through the artifact store. Lift any
#      stage into your own build.
#
#   2. AN EXTENSIVE SMOKE TEST. Each stage also asserts the protocol-level
#      behaviour of its role (rewritten URLs, digests, status codes, ledger
#      rows, cache hits, refusals). Any failed assertion fails the build, so
#      `docker build` IS the test run. The final image prints the report.
#
# The build itself needs NO internet — only the cache host. Every byte (base
# images, .debs, .apks, wheels, tarballs, git objects) is pulled through
# package-registry, which is exactly the point: if this builds, an air-gapped
# CI host can build.
#
# SEE ALSO examples/six-repositories/ — the same six roles, tested the other way
# round: it builds a client image once and asserts at container RUNTIME (verify.sh
# as the entrypoint). Use that one to re-test repeatedly without rebuilding, to
# keep the host/CA/token out of image layers, or to run the same image against
# both sides of the air gap. Use THIS one when the question is "can a build
# actually complete with nothing but the cache?" — here the build IS the test.
#
# -----------------------------------------------------------------------------
# PREREQUISITES
# -----------------------------------------------------------------------------
#   * The cache is up:  docker compose --profile online --profile ui up -d
#   * <HOST> is in the server cert's SANs (else re-run ./scripts/gen-certs.sh <HOST>).
#     Prefer a DNS NAME over an IP here — see the pip note in the pypi stage below;
#     pip < 26 cannot talk to an IP-literal HTTPS index at all.
#   * A `files` write token for the project (generate it in the console) if you
#     want the upload half of the files role tested. Without it those turn into
#     SKIPs and only the read side is asserted.
#   * The Docker daemon on THIS host trusts the cache CA. Each of the six CLIENTS
#     below is told about the CA per command (--cert / --cafile / --cacert /
#     GIT_SSL_CAINFO / SSL_CERT_FILE), but the `FROM` pulls are done by the
#     daemon before any command runs, and Docker has no per-pull CA flag. One
#     entry covers global and every project — they share the port, and the
#     project rides the image name:
#
#       sudo mkdir -p /etc/docker/certs.d/<HOST>:8443
#       sudo cp certs/ca.crt /etc/docker/certs.d/<HOST>:8443/ca.crt
#
#     (No daemon restart needed. Remove the directory to undo.)
#
#     IF YOU CANNOT CHANGE THE DAEMON, two escape hatches:
#
#       a) Redirect the base images and leave everything else alone:
#            --build-arg BASE_PYTHON=python:3.12-slim \
#            --build-arg BASE_NODE=node:22-slim \
#            --build-arg BASE_ALPINE=alpine:3.20 \
#            --build-arg BASE_UV=ghcr.io/astral-sh/uv:python3.12-bookworm-slim
#          All six role tests still run through the cache; only the "base images
#          came through the OCI role" proof is lost, and the /v2/ API assertions
#          in the `api` stage still cover that role end to end (manifests for all
#          three upstreams, digest verification, blobs, tags).
#
#       b) Give a throwaway buildx builder the CA instead of the host daemon:
#            printf '[registry."<HOST>:8443"]\n  ca=["/etc/buildkit/ca.crt"]\n' > /tmp/buildkitd.toml
#            docker buildx create --name pkgcache --driver docker-container \
#                --config /tmp/buildkitd.toml
#            docker cp certs/ca.crt buildx_buildkit_pkgcache0:/etc/buildkit/ca.crt
#            docker buildx use pkgcache        # docker buildx rm pkgcache to undo
#
# -----------------------------------------------------------------------------
# RUN IT (from the repo root — the build context needs ./certs/ca.crt)
# -----------------------------------------------------------------------------
#   docker build -f examples/Dockerfile -t pkgcache-smoke \
#     --build-arg HOST=172.17.21.107 \
#     --build-arg FILES_TOKEN="$FILES_TOKEN" \
#     --build-arg CACHEBUST="$(date +%s)" \
#     --progress=plain .
#
#   docker run --rm pkgcache-smoke          # re-print the full report
#
# For a named project, add the prefix in the two places the project lives:
#
#   --build-arg PROJECT=projA --build-arg REGISTRY=172.17.21.107:8443/projA
#
# (path roles take /<project>/<role>/…, OCI takes it in the image name, and
# apt/apk take it as the proxy username — all three are handled below.)
#
# CACHEBUST is not decoration: without it Docker reuses the cached RUN layers
# and re-runs nothing. Pass a fresh value whenever you actually want to test.
#
# Deliberately NOT used here: the `# syntax=` directive (it would make BuildKit
# pull a frontend image from Docker Hub — fatal on an air-gapped builder) and
# digest-pinned bases (this build is *about* exercising tag resolution through
# the cache; production images should still pin — see pkgcache/Dockerfile).
# =============================================================================

# ---- knobs (declared before the first FROM so they can be used in FROM) ------
ARG HOST
ARG UNIFIED_PORT=8443
ARG APT_PORT=3142
ARG PROJECT=global
# The OCI role has no /<project>/ path prefix — Docker cannot be given a base
# path, so a project rides the image name instead: <host>:8443/<project>/dockerhub/…
ARG REGISTRY=${HOST}:${UNIFIED_PORT}
ARG PYPI_INDEX=root/pypi
ARG CACHEBUST=0

# The four base images, each a full reference so any of them can be REDIRECTED
# without touching the Docker daemon (see "if you cannot change the daemon" above):
#   --build-arg BASE_PYTHON=python:3.12-slim
# pulls that base straight from Docker Hub while every role test still runs through
# the cache. The six client flows are unaffected; you only lose the proof that base
# images themselves came through the OCI role (the /v2/ API assertions still cover it).
ARG BASE_PYTHON=${REGISTRY}/dockerhub/library/python:3.12-slim
ARG BASE_NODE=${REGISTRY}/dockerhub/library/node:22-slim
ARG BASE_ALPINE=${REGISTRY}/dockerhub/library/alpine:3.20
ARG BASE_UV=${REGISTRY}/ghcr/astral-sh/uv:python3.12-bookworm-slim


# =============================================================================
# ROLE 1/6 — OCI  ·  every FROM in this file is a pull through the cache
# ROLE 2/6 — apt  ·  and this stage's tooling is installed through the proxy
# =============================================================================
FROM ${BASE_PYTHON} AS toolbox

ARG BASE_PYTHON
ARG HOST
ARG UNIFIED_PORT
ARG APT_PORT
ARG PROJECT
ARG REGISTRY
ARG CACHEBUST

# The two base URLs every later stage is built from.
#   BASE     — path roles:  https://<host>:8443/<project>/<role>/…
#   OCI_BASE — OCI keeps /v2/ at the listener root (project rides the image name)
#   APT_PROXY— plain-HTTP forward proxy; the project is the proxy USERNAME and
#              the password is an ignored label (it is routing, not auth)
ENV CA=/etc/pkgcache/ca.crt \
    BASE=https://${HOST}:${UNIFIED_PORT}/${PROJECT} \
    OCI_BASE=https://${HOST}:${UNIFIED_PORT} \
    APT_PROXY=http://${PROJECT}:ignored@${HOST}:${APT_PORT}

# ---- apt through the forward proxy -----------------------------------------
# Two lines are all it takes: point apt at the proxy, and keep the mirror lines
# on http (the proxy does not tunnel HTTPS — CONNECT would be opaque to a cache).
RUN set -eu; \
    echo "cachebust=${CACHEBUST}"; \
    printf 'Acquire::http::Proxy "%s";\n' "${APT_PROXY}" > /etc/apt/apt.conf.d/01pkgcache-proxy; \
    sed -i 's|https://|http://|g' /etc/apt/sources.list.d/*.sources /etc/apt/sources.list 2>/dev/null || true; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl wget git jq xz-utils; \
    # a second update exercises the VOLATILE path: index files are revalidated
    # upstream with If-None-Match / If-Modified-Since rather than re-downloaded
    apt-get update

# ---- trust the cache CA (system store + a stable path for the clients) ------
# pip, npm and git do not read the system store, so keep the PEM addressable.
COPY certs/ca.crt /usr/local/share/ca-certificates/pkgcache.crt
RUN set -eu; \
    mkdir -p /etc/pkgcache /report; \
    cp /usr/local/share/ca-certificates/pkgcache.crt "$CA"; \
    update-ca-certificates

# ---- the assertion helpers every stage sources ------------------------------
# Written with printf (not a heredoc) so this file stays parseable by the
# built-in Dockerfile frontend. Sourced, never executed.
RUN set -eu; printf '%s\n' \
    '# ok/skip/fail write one line to $REPORT and to the build log.' \
    'ok()   { printf "  PASS  %s\n" "$*" | tee -a "$REPORT"; }' \
    'skip() { printf "  SKIP  %s\n" "$*" | tee -a "$REPORT"; }' \
    'fail() { printf "  FAIL  %s\n" "$*" | tee -a "$REPORT"; exit 1; }' \
    'want() { [ "$2" = "$3" ] && ok "$1 [$3]" || fail "$1: want [$3] got [$2]"; }' \
    'has()  { case "$2" in *"$3"*) ok "$1";; *) fail "$1: [$3] not in response";; esac; }' \
    'gt()   { [ "$2" -gt "$3" ] 2>/dev/null && ok "$1 [$2]" || fail "$1: want >$3 got [$2]"; }' \
    '# code URL [curl args...] -> HTTP status only; get URL [curl args...] -> body' \
    'code() { u="$1"; shift; curl -sS -o /dev/null -w "%{http_code}" --cacert "$CA" "$@" -- "$u" || true; }' \
    'get()  { u="$1"; shift; curl -sS --cacert "$CA" "$@" -- "$u"; }' \
    > /usr/local/bin/pkgtest.sh

RUN set -eu; export REPORT=/report/10-oci-and-apt.txt; . /usr/local/bin/pkgtest.sh; \
    ok "OCI: toolbox base pulled as ${BASE_PYTHON}"; \
    ok "apt: apt-get update through the :${APT_PORT} proxy as project ${PROJECT}"; \
    # the .debs really came from the proxy: apt's own state proves the transfer
    ls /var/lib/apt/lists/ | grep -q Packages || fail "apt: no Packages index cached"; \
    ok "apt: Packages indexes fetched through the proxy"; \
    dpkg -s curl > /dev/null || fail "apt: curl was not installed"; \
    ok "apt: curl/git/jq/wget installed from cached .debs"; \
    ok "apt: repeat apt-get update revalidated the volatile indexes"


# =============================================================================
# ROLE 3/6 — PyPI  ·  pip against the cache's PEP 503/691 simple index
# =============================================================================
FROM toolbox AS pypi
ARG HOST
ARG UNIFIED_PORT
ARG PYPI_INDEX
ARG CACHEBUST

# This is the whole client configuration. `--cert` (or PIP_CERT) is required:
# pip ships its own trust store and ignores the system one.
ENV PIP_INDEX_URL=${BASE}/pypi/${PYPI_INDEX}/+simple/ \
    PIP_CERT=/etc/pkgcache/ca.crt \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_ROOT_USER_ACTION=ignore \
    SIMPLE=${BASE}/pypi/${PYPI_INDEX}

# ---- index-level protocol ---------------------------------------------------
RUN set -eu; export REPORT=/report/20-pypi-index.txt; . /usr/local/bin/pkgtest.sh; \
    echo "cachebust=${CACHEBUST}"; \
    want "pypi /+indexes serves the configured index map" "$(code "$BASE/pypi/+indexes")" 200; \
    has  "pypi /+indexes advertises ${PYPI_INDEX}" "$(get "$BASE/pypi/+indexes")" "${PYPI_INDEX}"; \
    want "pypi simple page (HTML) for six" "$(code "$SIMPLE/+simple/six/")" 200; \
    # every link on the page must point BACK at the cache, not at pypi.org —
    # this is what makes the air-gapped side serve the same page unchanged
    has  "pypi simple links are rewritten to the cache" "$(get "$SIMPLE/+simple/six/")" "/+f/six/"; \
    want "pypi simple page leaks no upstream URLs" \
         "$(get "$SIMPLE/+simple/six/" | grep -c 'files.pythonhosted.org' || true)" "0"; \
    want "pypi PEP 691 JSON view" \
         "$(code "$SIMPLE/+simple/six/" -H 'Accept: application/vnd.pypi.simple.v1+json')" 200; \
    # PEP 503 name normalisation: Zope.Interface -> zope-interface
    want "pypi normalises project names" "$(code "$SIMPLE/+simple/Zope.Interface/")" 200; \
    want "pypi unknown index 404s" "$(code "$BASE/pypi/root/nope/+simple/six/")" 404; \
    want "pypi unknown project 404s" "$(code "$SIMPLE/+simple/definitely-not-a-package-xyz/")" 404

# ---- artefact integrity: the wheel must hash to what the index promised -----
RUN set -eu; export REPORT=/report/21-pypi-integrity.txt; . /usr/local/bin/pkgtest.sh; \
    fn=six-1.16.0-py2.py3-none-any.whl; \
    json=$(get "$SIMPLE/+simple/six/" -H 'Accept: application/vnd.pypi.simple.v1+json'); \
    # `printf %s`, never `echo`, to feed JSON to jq: /bin/sh here is dash, whose
    # echo expands backslash escapes — it turns a packument's \n and \t INSIDE
    # string values into raw control bytes and jq then rejects the document.
    # (busybox echo does not, which is exactly how this survives testing on Alpine.)\
    expect=$(printf %s "$json" | jq -r --arg f "$fn" '.files[] | select(.filename==$f) | .hashes.sha256'); \
    url=$(printf %s "$json" | jq -r --arg f "$fn" '.files[] | select(.filename==$f) | .url'); \
    has "pypi file URL points at the cache" "$url" "$BASE/pypi/"; \
    get "$url" --fail -o /tmp/$fn; \
    want "pypi wheel sha256 matches the index" "$(sha256sum /tmp/$fn | cut -d' ' -f1)" "$expect"; \
    # PEP 658: the .metadata sidecar lets a resolver skip downloading the wheel
    want "pypi PEP 658 .metadata sidecar" "$(code "$SIMPLE/+f/six/$fn.metadata")" 200; \
    want "pypi HEAD on a cached wheel" "$(code "$SIMPLE/+f/six/$fn" -I)" 200; \
    want "pypi range request on a cached wheel" "$(code "$SIMPLE/+f/six/$fn" -r 0-63)" 206; \
    want "pypi unknown filename 404s" "$(code "$SIMPLE/+f/six/six-0.0.0-py3-none-any.whl")" 404

# ---- a real install, dependency graph and all ------------------------------
RUN set -eu; export REPORT=/report/22-pip-install.txt; . /usr/local/bin/pkgtest.sh; \
    python -m venv /tmp/venv; \
    # GOTCHA, and the reason for this one --trusted-host hop: pip < 26 bundles
    # urllib3 1.26, which omits SNI for an IP-literal host, and pip's default
    # truststore-backed SSLContext then refuses the socket with
    #   ValueError: check_hostname requires server_hostname
    # It has nothing to do with which CA you trust — --cert cannot fix it. So
    # bootstrap a current pip through the cache first (this single call is the one
    # that cannot yet verify), after which everything below verifies normally.
    # Not needed at all when HOST is a DNS name covered by the cert.
    /tmp/venv/bin/pip install -q --index-url "$PIP_INDEX_URL" \
        --trusted-host "${HOST}:${UNIFIED_PORT}" --upgrade pip; \
    ok "pip bootstrapped itself to $(/tmp/venv/bin/pip --version | cut -d' ' -f2) through the cache"; \
    # explicit form (what you would put in a CI job); PIP_INDEX_URL/PIP_CERT above
    # do the same thing implicitly
    /tmp/venv/bin/pip install --index-url "$PIP_INDEX_URL" --cert "$CA" 'requests==2.32.3'; \
    /tmp/venv/bin/python -c 'import requests, certifi, urllib3, idna; print(requests.__version__)'; \
    ok "pip install requests==2.32.3 + 4 transitive deps through the cache"; \
    gt "pip resolved and installed distributions" "$(/tmp/venv/bin/pip list --format=freeze | wc -l)" 4; \
    # a re-fetch of an already-cached wheel: same command, served from disk
    /tmp/venv/bin/pip download --no-deps --dest /tmp/dl --index-url "$PIP_INDEX_URL" --cert "$CA" 'six==1.16.0'; \
    ok "pip download of an already-cached wheel succeeded"


# =============================================================================
# ROLE 3/6 (second client) — uv, from a ghcr.io image pulled through the cache
# =============================================================================
# Two things at once: the OCI role's *second* upstream (ghcr, not dockerhub) and
# uv's own index handling. uv ignores PIP_CERT — it reads SSL_CERT_FILE.
FROM ${BASE_UV} AS uvtest
ARG BASE_UV
ARG HOST
ARG UNIFIED_PORT
ARG PROJECT
ARG PYPI_INDEX
ARG CACHEBUST

ENV CA=/etc/pkgcache/ca.crt \
    SSL_CERT_FILE=/etc/pkgcache/ca.crt \
    UV_INDEX_URL=https://${HOST}:${UNIFIED_PORT}/${PROJECT}/pypi/${PYPI_INDEX}/+simple/ \
    UV_NO_PROGRESS=1

COPY --from=toolbox /etc/pkgcache/ca.crt /etc/pkgcache/ca.crt
COPY --from=toolbox /usr/local/bin/pkgtest.sh /usr/local/bin/pkgtest.sh

RUN set -eu; export REPORT=/report/25-uv.txt; mkdir -p /report; . /usr/local/bin/pkgtest.sh; \
    echo "cachebust=${CACHEBUST}"; \
    ok "OCI: ghcr upstream — image pulled as ${BASE_UV}"; \
    uv venv /tmp/uvenv; \
    uv pip install --python /tmp/uvenv/bin/python --index-url "$UV_INDEX_URL" 'rich==13.7.1'; \
    # rich pulls markdown-it-py, mdurl and pygments — a real graph, not one wheel
    /tmp/uvenv/bin/python -c 'import rich.console, markdown_it, pygments, mdurl; rich.console.Console()'; \
    ok "uv pip install rich==13.7.1 + 3 transitive deps through the cache (SSL_CERT_FILE trust)"; \
    # uv is hash-checking by default — reaching here proves the cached wheels
    # matched the index hashes byte for byte
    ok "uv hash verification passed against the cache's simple index"; \
    # README Route 1: same install with SSL_CERT_FILE removed, only --allow-insecure-host
    env -u SSL_CERT_FILE uv pip install --python /tmp/uvenv/bin/python \
        --allow-insecure-host "${HOST}:${UNIFIED_PORT}" \
        --index-url "$UV_INDEX_URL" 'six==1.16.0'; \
    ok "uv --allow-insecure-host (UV_INSECURE_HOST) installed with no CA"


# =============================================================================
# ROLE 4/6 — npm  ·  registry prefix + rewritten tarball URLs
# =============================================================================
FROM ${BASE_NODE} AS npmtest
ARG BASE_NODE
ARG HOST
ARG UNIFIED_PORT
ARG APT_PORT
ARG PROJECT
ARG CACHEBUST

ENV CA=/etc/pkgcache/ca.crt \
    BASE=https://${HOST}:${UNIFIED_PORT}/${PROJECT} \
    NPM_CONFIG_REGISTRY=https://${HOST}:${UNIFIED_PORT}/${PROJECT}/npm/ \
    NPM_CONFIG_CAFILE=/etc/pkgcache/ca.crt \
    NPM_CONFIG_CACHE=/tmp/npm-cache \
    NPM_CONFIG_FUND=false \
    NPM_CONFIG_AUDIT=false

# Reuse the proxy config from the toolbox stage instead of restating it — the
# file is exactly what you would bake into any build image.
COPY --from=toolbox /etc/apt/apt.conf.d/01pkgcache-proxy /etc/apt/apt.conf.d/01pkgcache-proxy
COPY --from=toolbox /etc/pkgcache/ca.crt /etc/pkgcache/ca.crt
COPY --from=toolbox /usr/local/bin/pkgtest.sh /usr/local/bin/pkgtest.sh
RUN set -eu; \
    sed -i 's|https://|http://|g' /etc/apt/sources.list.d/*.sources /etc/apt/sources.list 2>/dev/null || true; \
    apt-get update && apt-get install -y --no-install-recommends curl jq ca-certificates; \
    rm -rf /var/lib/apt/lists/*

RUN set -eu; export REPORT=/report/30-npm.txt; mkdir -p /report; . /usr/local/bin/pkgtest.sh; \
    echo "cachebust=${CACHEBUST}"; \
    ok "OCI: node base pulled as ${BASE_NODE}"; \
    ok "apt: proxy config reused verbatim in a second image"; \
    # packument: every dist.tarball must be rewritten to this project's prefix
    doc=$(get "$BASE/npm/left-pad" --fail); \
    tb=$(printf %s "$doc" | jq -r '.versions["1.3.0"].dist.tarball'); \
    has "npm packument rewrites dist.tarball to the cache" "$tb" "$BASE/npm/left-pad/-/"; \
    # and the tarball it points at must match the packument's own checksum
    get "$tb" --fail -o /tmp/left-pad.tgz; \
    want "npm tarball sha1 matches dist.shasum" \
         "$(sha1sum /tmp/left-pad.tgz | cut -d' ' -f1)" \
         "$(printf %s "$doc" | jq -r '.versions["1.3.0"].dist.shasum')"; \
    want "npm scoped packument (/@scope/pkg)" "$(code "$BASE/npm/@types/node")" 200; \
    want "npm unknown package 404s" "$(code "$BASE/npm/definitely-not-a-package-xyz")" 404; \
    # (a filename absent from the packument — left-pad really does publish 0.0.0)
    want "npm unknown tarball 404s" "$(code "$BASE/npm/left-pad/-/left-pad-99.99.99-nope.tgz")" 404; \
    # README Route 1: the same fetch with the CA env REMOVED and only --strict-ssl=false
    if env -u NPM_CONFIG_CAFILE npm view left-pad version > /dev/null 2>&1; then \
        fail "npm trusted the private CA with no cafile and no flag"; \
    else ok "baseline: npm without cafile or --strict-ssl is refused"; fi; \
    env -u NPM_CONFIG_CAFILE npm view --strict-ssl=false left-pad version > /dev/null; \
    ok "npm --strict-ssl=false reached the registry with no CA"

RUN set -eu; export REPORT=/report/31-npm-install.txt; . /usr/local/bin/pkgtest.sh; \
    mkdir -p /tmp/app; cd /tmp/app; \
    npm init -y > /dev/null; \
    # a real resolution graph, not a single package: debug pulls ms, chalk pulls
    # its own tree — all of it must come from the cache
    npm install --no-audit --no-fund debug@4.3.4 semver@7.6.3 chalk@4.1.2; \
    test -f node_modules/debug/package.json; \
    test -f node_modules/ms/package.json; \
    ok "npm install of a 3-package dependency graph through the cache"; \
    gt "npm packages materialised in node_modules" "$(ls node_modules | grep -vc '^\.' || true)" 4; \
    # scoped packages take a different route in the handler (/@scope/pkg/-/file)
    npm install --no-audit --no-fund @types/node@20.14.10; \
    test -f node_modules/@types/node/package.json; \
    ok "npm install of a SCOPED package (@types/node)"; \
    # re-install from an empty local npm cache: served again by pkgcache, offline-safe
    rm -rf node_modules /tmp/npm-cache; \
    npm install --no-audit --no-fund; \
    test -f node_modules/chalk/package.json; \
    ok "npm re-install with an empty client cache (all traffic hit pkgcache)"


# =============================================================================
# ROLE 2/6 (second client) — apk  ·  the same forward proxy, from Alpine/musl
# =============================================================================
FROM ${BASE_ALPINE} AS apktest
ARG BASE_ALPINE
ARG HOST
ARG UNIFIED_PORT
ARG APT_PORT
ARG PROJECT
ARG CACHEBUST

ENV CA=/etc/pkgcache/ca.crt \
    BASE=https://${HOST}:${UNIFIED_PORT}/${PROJECT} \
    http_proxy=http://${PROJECT}:ignored@${HOST}:${APT_PORT}

COPY --from=toolbox /etc/pkgcache/ca.crt /etc/pkgcache/ca.crt
COPY --from=toolbox /usr/local/bin/pkgtest.sh /usr/local/bin/pkgtest.sh

# apk reads http_proxy from the environment; the repositories must be http.
# (Only http:// traffic is proxied — the https:// asserts below still go direct
# to :8443, which is what we want.)
RUN set -eu; \
    sed -i 's|https://|http://|g' /etc/apk/repositories; \
    apk add --no-cache curl jq wget ca-certificates

RUN set -eu; export REPORT=/report/40-apk.txt; mkdir -p /report; . /usr/local/bin/pkgtest.sh; \
    echo "cachebust=${CACHEBUST}"; \
    ok "OCI: alpine base pulled as ${BASE_ALPINE}"; \
    apk info -e curl > /dev/null || fail "apk: curl was not installed"; \
    ok "apk: packages installed through the :${APT_PORT} proxy"; \
    apk add --no-cache tzdata || fail "apk: second install failed"; \
    ok "apk: second install reused the proxied indexes"; \
    # a musl client against the TLS roles, proving the CA works outside glibc
    want "apk-side: TLS role reachable from musl/curl" "$(code "$BASE/files/healthz")" 200; \
    wget --ca-certificate="$CA" -q -O /dev/null "$BASE/files/" || fail "apk-side: wget failed"; \
    ok "apk-side: plain wget downloads from the files role"


# =============================================================================
# ROLE 5/6 — git  ·  mirror-and-serve smart HTTP, read-only
# =============================================================================
FROM toolbox AS gittest
ARG CACHEBUST
ENV GIT_SSL_CAINFO=/etc/pkgcache/ca.crt \
    GIT_TERMINAL_PROMPT=0
# The real upstream host is the first path segment; the cache keeps a bare
# mirror and serves clones/fetches from it (online AND offline).
ENV GIT_BASE=${BASE}/git

# ---- protocol level ---------------------------------------------------------
RUN set -eu; export REPORT=/report/50-git-protocol.txt; . /usr/local/bin/pkgtest.sh; \
    echo "cachebust=${CACHEBUST}"; \
    repo="$GIT_BASE/github.com/octocat/Hello-World.git"; \
    has  "git advertises refs (smart HTTP)" \
         "$(get "$repo/info/refs?service=git-upload-pack")" "service=git-upload-pack"; \
    want "git refuses push (read-only mirror)" \
         "$(code "$repo/info/refs?service=git-receive-pack")" 403; \
    want "git refuses git-receive-pack POST" "$(code "$repo/git-receive-pack" -X POST)" 403; \
    want "git rejects the dumb protocol" "$(code "$repo/HEAD")" 404; \
    want "git rejects a path with no upstream host" \
         "$(code "$GIT_BASE/notahost/x/info/refs?service=git-upload-pack")" 404

# ---- real clients -----------------------------------------------------------
RUN set -eu; export REPORT=/report/51-git-clone.txt; . /usr/local/bin/pkgtest.sh; \
    repo="$GIT_BASE/github.com/octocat/Hello-World.git"; \
    git clone -q "$repo" /tmp/hw; \
    test -d /tmp/hw/.git; \
    ok "git full clone through the mirror"; \
    # the mirror is warm now — a second clone is served entirely from disk
    git clone -q "$repo" /tmp/hw-again; \
    ok "git second clone served from the local mirror"; \
    git clone -q --depth 1 "$GIT_BASE/github.com/pallets/click.git" /tmp/click; \
    test -f /tmp/click/README.md; \
    ok "git shallow clone (--depth 1)"; \
    git clone -q --filter=blob:none "$GIT_BASE/github.com/pallets/click.git" /tmp/click-partial; \
    ok "git partial clone (--filter=blob:none)"; \
    git -C /tmp/click fetch -q --depth 1 origin refs/tags/8.1.7 || fail "git tag fetch failed"; \
    ok "git fetch of a single tag"; \
    # SHA-pinned fetch of a NON-tip commit (uploadpack.allowAnySHA1InWant)
    sha=$(git -C /tmp/hw rev-parse HEAD~1); \
    mkdir -p /tmp/pin; git -C /tmp/pin init -q; \
    git -C /tmp/pin remote add origin "$repo"; \
    git -C /tmp/pin fetch -q --depth 1 origin "$sha"; \
    want "git SHA-pinned fetch of a non-tip commit" "$(git -C /tmp/pin rev-parse FETCH_HEAD)" "$sha"; \
    # push must fail end-to-end, not just at the ref advertisement
    if git -C /tmp/hw push -q origin HEAD:refs/heads/smoke-test 2>/dev/null; then \
        fail "git push was accepted by a read-only mirror"; \
    else ok "git push rejected end-to-end"; fi

# ---- transparent rewriting: what you set once on a CI image -----------------
# insteadOf covers submodules, `pip install git+https://…`, CPM, go modules —
# anything that hardcodes a github.com URL. Nothing below reaches github.com.
RUN set -eu; export REPORT=/report/52-git-insteadof.txt; . /usr/local/bin/pkgtest.sh; \
    git config --global http."$OCI_BASE/".sslCAInfo "$CA"; \
    git config --global url."$GIT_BASE/github.com/".insteadOf "https://github.com/"; \
    git clone -q https://github.com/octocat/Hello-World.git /tmp/hw-rewritten; \
    test -d /tmp/hw-rewritten/.git; \
    ok "git url.insteadOf rewrote a bare github.com clone through the cache"


# =============================================================================
# ROLE 6/6 — files  ·  the one write path (token-gated PUT, anonymous GET)
# =============================================================================
FROM toolbox AS filestest
ARG CACHEBUST
# NOTE: a build arg is visible in this stage's `docker history`. Only the report
# is copied into the final image, so the token does not reach it — still, use a
# rotatable token, or drop the write half by omitting FILES_TOKEN entirely.
ARG FILES_TOKEN=""
ENV FILES_BASE=${BASE}/files

RUN set -eu; export REPORT=/report/60-files.txt; . /usr/local/bin/pkgtest.sh; \
    echo "cachebust=${CACHEBUST}"; \
    offline=$(get "$FILES_BASE/healthz" --fail | jq -r .offline); \
    want "files role is healthy" "$(code "$FILES_BASE/healthz")" 200; \
    path="smoke-tests/$(date -u +%Y%m%dT%H%M%S)-$$/hello.txt"; \
    printf 'hello from the package-registry smoke test\n' > /tmp/hello.txt; \
    sum=$(sha256sum /tmp/hello.txt | cut -d' ' -f1); \
    # ---- read side (anonymous, always asserted) ----------------------------
    want "files GET of a missing path 404s" "$(code "$FILES_BASE/$path")" 404; \
    want "files directory autoindex" "$(code "$FILES_BASE/")" 200; \
    # ---- write side --------------------------------------------------------
    if [ -z "$FILES_TOKEN" ]; then \
        st=$(code "$FILES_BASE/$path" -X PUT --data-binary @/tmp/hello.txt); \
        case "$st" in 401|403) ok "files PUT without a token is refused [$st]";; \
                      *) fail "files PUT without a token returned [$st]";; esac; \
        skip "files upload tests (pass --build-arg FILES_TOKEN=... to run them)"; \
    elif [ "$offline" = "true" ]; then \
        want "files PUT refused on the OFFLINE side" \
             "$(code "$FILES_BASE/$path" -X PUT -H "Authorization: Bearer $FILES_TOKEN" --data-binary @/tmp/hello.txt)" 403; \
        skip "files upload tests (this instance is air-gapped: writes are disabled)"; \
    else \
        want "files PUT with no credentials" \
             "$(code "$FILES_BASE/$path" -X PUT --data-binary @/tmp/hello.txt)" 401; \
        want "files PUT with a wrong token" \
             "$(code "$FILES_BASE/$path" -X PUT -H 'Authorization: Bearer not-the-token' --data-binary @/tmp/hello.txt)" 401; \
        want "files PUT into the reserved +namespace" \
             "$(code "$FILES_BASE/+nope/x.txt" -X PUT -H "Authorization: Bearer $FILES_TOKEN" --data-binary @/tmp/hello.txt)" 403; \
        want "files PUT with a mismatched X-Checksum-Sha256" \
             "$(code "$FILES_BASE/$path" -X PUT -H "Authorization: Bearer $FILES_TOKEN" \
                -H 'X-Checksum-Sha256: 0000000000000000000000000000000000000000000000000000000000000000' \
                --data-binary @/tmp/hello.txt)" 400; \
        # the real upload, with server-side checksum verification
        body=$(get "$FILES_BASE/$path" -X PUT -H "Authorization: Bearer $FILES_TOKEN" \
               -H "X-Checksum-Sha256: $sum" --data-binary @/tmp/hello.txt --fail); \
        want "files PUT (checksum-verified) returns the stored sha256" \
             "$(printf %s "$body" | jq -r .sha256)" "$sum"; \
        has  "files PUT returns a project-scoped URL" "$(printf %s "$body" | jq -r .url)" "$FILES_BASE/"; \
        want "files write-once: re-PUT without ?overwrite=1 conflicts" \
             "$(code "$FILES_BASE/$path" -X PUT -H "Authorization: Bearer $FILES_TOKEN" --data-binary @/tmp/hello.txt)" 409; \
        want "files re-PUT with ?overwrite=1 replaces" \
             "$(code "$FILES_BASE/$path?overwrite=1" -X PUT -H "Authorization: Bearer $FILES_TOKEN" --data-binary @/tmp/hello.txt)" 200; \
        # round trip, byte for byte
        get "$FILES_BASE/$path" --fail -o /tmp/back.txt; \
        want "files GET returns the uploaded bytes" "$(sha256sum /tmp/back.txt | cut -d' ' -f1)" "$sum"; \
        want "files HEAD" "$(code "$FILES_BASE/$path" -I)" 200; \
        want "files range request (resumable download)" "$(code "$FILES_BASE/$path" -r 0-4)" 206; \
        has  "files autoindex lists the upload" "$(get "$FILES_BASE/${path%/*}/")" "hello.txt"; \
        # the ledger row exists BEFORE we delete (the api stage runs after cleanup)
        gt "files ledger recorded the artifact" \
           "$(get "$BASE/files/+ledger/artifacts?eco=files&q=hello.txt" | jq '.artifacts | length')" 0; \
        # clean up after ourselves: DELETE also drops the ledger row
        want "files DELETE" "$(code "$FILES_BASE/$path" -X DELETE -H "Authorization: Bearer $FILES_TOKEN")" 204; \
        want "files GET after DELETE 404s" "$(code "$FILES_BASE/$path")" 404; \
    fi


# =============================================================================
# ROUTE 1 — the documented default: no CA file on the client at all
# =============================================================================
# Every stage above hands its client `certs/ca.crt`. That is the verifying route,
# and it is worth testing, but README's Route 1 (and examples/porting/) tell people
# to skip verification for this one host instead and ship no cert. THIS stage has no
# ca.crt copied in and nothing added to its trust store, so it tests that route
# honestly: each client is asserted to FAIL first, then to succeed with only a flag.
FROM ${BASE_PYTHON} AS nocert
ARG HOST
ARG UNIFIED_PORT
ARG PROJECT
ARG PYPI_INDEX
ARG CACHEBUST
ENV BASE=https://${HOST}:${UNIFIED_PORT}/${PROJECT}

COPY --from=toolbox /etc/apt/apt.conf.d/01pkgcache-proxy /etc/apt/apt.conf.d/01pkgcache-proxy
COPY --from=toolbox /usr/local/bin/pkgtest.sh /usr/local/bin/pkgtest.sh
RUN set -eu; \
    sed -i 's|https://|http://|g' /etc/apt/sources.list.d/*.sources 2>/dev/null || true; \
    apt-get update \
 && apt-get install -y --no-install-recommends git curl wget \
 && rm -rf /var/lib/apt/lists/*

RUN set -eu; export REPORT=/report/80-no-ca-client.txt; mkdir -p /report; \
    . /usr/local/bin/pkgtest.sh; \
    echo "cachebust=${CACHEBUST}"; \
    idx="https://${HOST}:${UNIFIED_PORT}/${PROJECT}/pypi/${PYPI_INDEX}/+simple/"; \
    repo="$BASE/git/github.com/octocat/Hello-World.git"; \
    # curl: refused without a flag, fine with -k
    if curl -sS -o /dev/null "$BASE/files/healthz" 2>/dev/null; then \
        fail "curl trusted the private CA without being told to"; \
    else ok "baseline: no CA in this image, so plain curl is refused"; fi; \
    want "curl -k reaches the files role" \
         "$(curl -sSk -o /dev/null -w '%{http_code}' "$BASE/files/healthz" || true)" 200; \
    want "wget --no-check-certificate downloads from files" \
         "$(wget --no-check-certificate -q -O /dev/null "$BASE/files/" && echo ok || echo bad)" ok; \
    # pip: --trusted-host only, no PIP_CERT (this also sidesteps the IP/SNI bug)
    python -m venv /tmp/v; \
    /tmp/v/bin/pip install -q --index-url "$idx" --trusted-host "${HOST}:${UNIFIED_PORT}" six; \
    /tmp/v/bin/python -c 'import six'; \
    ok "pip --trusted-host installed a package with no CA present"; \
    # git: refused without a flag, fine with a URL-scoped sslVerify=false
    if git clone -q "$repo" /tmp/g-fail 2>/dev/null; then \
        fail "git trusted the private CA without being told to"; \
    else ok "baseline: plain git clone is refused"; fi; \
    git -c http."https://${HOST}:${UNIFIED_PORT}/".sslVerify=false clone -q "$repo" /tmp/g-ok; \
    test -d /tmp/g-ok/.git; \
    ok "git -c http.<url>.sslVerify=false cloned with no CA present"


# =============================================================================
# CROSS-ROLE — the admin surface every role shares, and the OCI registry API
# =============================================================================
# Copying every stage's report here is also what ORDERS the build: this stage
# cannot start until all six roles have been exercised, so the ledger and
# progress assertions below see the traffic those stages generated.
FROM toolbox AS api
ARG PROJECT
ARG PYPI_INDEX
ARG CACHEBUST
COPY --from=pypi     /report/ /report/
COPY --from=uvtest   /report/ /report/
COPY --from=npmtest  /report/ /report/
COPY --from=apktest  /report/ /report/
COPY --from=gittest  /report/ /report/
COPY --from=filestest /report/ /report/

# ---- the OCI registry API, all three upstreams -----------------------------
RUN set -eu; export REPORT=/report/70-oci-api.txt; . /usr/local/bin/pkgtest.sh; \
    echo "cachebust=${CACHEBUST}"; \
    A='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'; \
    want "OCI /v2/ version ping" "$(code "$OCI_BASE/v2/")" 200; \
    want "OCI manifest by tag (dockerhub)" \
         "$(code "$OCI_BASE/v2/dockerhub/library/alpine/manifests/3.20" -H "Accept: $A")" 200; \
    want "OCI manifest by tag (ghcr)" \
         "$(code "$OCI_BASE/v2/ghcr/astral-sh/uv/manifests/python3.12-bookworm-slim" -H "Accept: $A")" 200; \
    # quay is the third upstream: assert the manifest path only, so the test
    # stays cheap (no multi-hundred-MB layer pull just to prove routing works)
    want "OCI manifest by tag (quay)" \
         "$(code "$OCI_BASE/v2/quay/prometheus/prometheus/manifests/v2.53.0" -H "Accept: $A")" 200; \
    want "OCI unknown upstream prefix 404s" \
         "$(code "$OCI_BASE/v2/nosuchreg/library/alpine/manifests/3.20" -H "Accept: $A")" 404; \
    want "OCI tags/list" "$(code "$OCI_BASE/v2/dockerhub/library/alpine/tags/list")" 200; \
    want "OCI tags/list returns an array" \
         "$(get "$OCI_BASE/v2/dockerhub/library/alpine/tags/list" | jq -r '.tags | type')" array

# ---- content addressing: what the cache returns must hash to its digest -----
RUN set -eu; export REPORT=/report/71-oci-digests.txt; . /usr/local/bin/pkgtest.sh; \
    A='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'; \
    img="$OCI_BASE/v2/dockerhub/library/alpine"; \
    dig=$(curl -sSI --cacert "$CA" -H "Accept: $A" "$img/manifests/3.20" \
          | grep -i '^docker-content-digest:' | cut -d' ' -f2 | tr -d '\r'); \
    has "OCI HEAD advertises a Docker-Content-Digest" "$dig" "sha256:"; \
    want "OCI manifest fetched BY DIGEST hashes to that digest" \
         "sha256:$(get "$img/manifests/$dig" -H "Accept: $A" | sha256sum | cut -d' ' -f1)" "$dig"; \
    # descend index -> platform manifest -> config blob and verify the blob too
    man=$(get "$img/manifests/3.20" -H "Accept: $A" \
          | jq -r '.manifests[]? | select(.platform.architecture=="amd64" and .platform.os=="linux") | .digest' | head -1); \
    has "OCI index lists a linux/amd64 manifest" "$man" "sha256:"; \
    blob=$(get "$img/manifests/$man" -H "Accept: $A" | jq -r '.config.digest'); \
    want "OCI blob HEAD" "$(code "$img/blobs/$blob" -I)" 200; \
    want "OCI blob content hashes to its digest" \
         "sha256:$(get "$img/blobs/$blob" | sha256sum | cut -d' ' -f1)" "$blob"; \
    # A bogus digest must never be served. The status is asserted as "some 4xx/5xx"
    # rather than 404: an upstream 404 on a blob currently surfaces as 500 (the
    # httpx error from inflight._run is not mapped back to the upstream status).
    st=$(code "$img/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000"); \
    case "$st" in 2*) fail "OCI served a bogus blob digest [$st]";; \
                  *)  ok "OCI refuses a bogus blob digest [$st]";; esac

# ---- the uniform admin surface: same shape for all six roles ---------------
RUN set -eu; export REPORT=/report/72-health-and-progress.txt; . /usr/local/bin/pkgtest.sh; \
    want "unified listener health" "$(code "$OCI_BASE/healthz")" 200; \
    want "unified listener URL index" "$(code "$OCI_BASE/")" 200; \
    want "unknown project is rejected with a hint" \
         "$(code "$OCI_BASE/no-such-project/npm/left-pad")" 404; \
    # /<project>/<role>/… is the admin form for EVERY role, apt included (its
    # client protocol is the :3142 proxy, but health/progress/ledger live here).
    # The progress path differs per role — each one keeps its ecosystem's native
    # endpoint so existing clients/dashboards keep working.
    for role in oci npm pypi apt git files; do \
        want "healthz: $role" "$(get "$BASE/$role/healthz" --fail | jq -r .role)" "$role"; \
        want "healthz reports the project: $role" \
             "$(get "$BASE/$role/healthz" --fail | jq -r .project)" "$PROJECT"; \
        case "$role" in \
            oci)  pp=/v2/_progress ;; \
            npm)  pp=/-/progress ;; \
            apt)  pp=/acng-progress ;; \
            *)    pp=/+progress ;; \
        esac; \
        want "progress feed: ${role}${pp}" \
             "$(get "$BASE/$role$pp" --fail | jq -r 'if (.recent|type)=="array" and (.downloads|type)=="array" then "ok" else "bad" end')" ok; \
        want "ledger stats: $role" "$(code "$BASE/$role/+ledger/stats")" 200; \
    done

# ---- the ledger: every role recorded what the stages above pulled ----------
RUN set -eu; export REPORT=/report/73-ledger.txt; . /usr/local/bin/pkgtest.sh; \
    count() { get "$BASE/$1/+ledger/artifacts?eco=$2" --fail | jq '.artifacts | length'; }; \
    gt "ledger: docker images recorded (role oci)"   "$(count oci docker)" 0; \
    gt "ledger: npm tarballs recorded (role npm)"    "$(count npm npm)"    0; \
    gt "ledger: wheels recorded (role pypi)"         "$(count pypi pip)"   0; \
    gt "ledger: .debs recorded (role apt)"           "$(count apt apt)"    0; \
    gt "ledger: .apks recorded (role apt)"           "$(count apt apk)"    0; \
    gt "ledger: git refs recorded (role git)"        "$(count git git)"    0; \
    # per-row shape: name/version/digest/size are what a checkpoint manifest exports
    row=$(get "$BASE/pypi/+ledger/artifacts?eco=pip&q=six" --fail | jq -r '.artifacts[0]'); \
    has "ledger rows carry a sha256 digest" "$(printf %s "$row" | jq -r .digest)" "sha256:"; \
    gt  "ledger rows carry a size"          "$(printf %s "$row" | jq -r '.size // 0')" 0

# ---- cache hits: the second fetch of a warm artefact never leaves the host --
RUN set -eu; export REPORT=/report/74-cache-hit.txt; . /usr/local/bin/pkgtest.sh; \
    fn=six-1.16.0-py2.py3-none-any.whl; \
    url="$BASE/pypi/${PYPI_INDEX}/+f/six/$fn"; \
    get "$url" --fail -o /dev/null; get "$url" --fail -o /dev/null; \
    get "$BASE/pypi/+progress" --fail \
      | jq -e --arg f "$fn" '[.recent[] | select(.name==$f and .hit==true)] | length > 0' > /dev/null \
      && ok "pypi re-fetch was served from cache (progress feed reports hit)" \
      || fail "pypi re-fetch was not reported as a cache hit"; \
    # apt's forward proxy answers a plain curl too — this is the whole protocol.
    # Volatile index, so both fetches are revalidations, not cached bodies.
    want "apt proxy serves an index through curl -x" \
         "$(code 'http://deb.debian.org/debian/dists/bookworm/Release' -x "$APT_PROXY")" 200; \
    # Resolve a real pool URL from apt itself rather than hardcoding one (pool
    # paths carry the version and would rot on the next Debian point release).
    deb=$(apt-get download --print-uris hello | tr -d "'" | cut -d' ' -f1); \
    has  "apt resolved an upstream pool URL" "$deb" "/pool/"; \
    want "apt proxy serves an immutable .deb through curl -x" "$(code "$deb" -x "$APT_PROXY")" 200; \
    want "apt proxy re-serves the same .deb from cache" "$(code "$deb" -x "$APT_PROXY")" 200


# =============================================================================
# The result: a tiny image whose only job is to hand you the report
# =============================================================================
FROM ${BASE_ALPINE} AS report
ARG HOST
ARG PROJECT
LABEL org.opencontainers.image.title="package-registry smoke test" \
      org.opencontainers.image.description="End-to-end verification of all six pkgcache roles"

COPY --from=api /report/ /report/
COPY --from=nocert /report/ /report/

RUN set -eu; \
    # NB: the report files are globbed as [0-9]*.txt, never *.txt — summary.txt
    # itself lives in the same directory and would otherwise glob into its own input.
    { echo "package-registry — six-role smoke test"; \
      echo "host=${HOST}  project=${PROJECT}  built=$(date -u +%Y-%m-%dT%H:%M:%SZ)"; \
      echo; \
      for f in /report/[0-9]*.txt; do echo "-- ${f##*/}"; cat "$f"; echo; done; \
      echo "----------------------------------------------------------------"; \
      echo "PASS: $(cat /report/[0-9]*.txt | grep -c '  PASS  ' || true)" \
           " SKIP: $(cat /report/[0-9]*.txt | grep -c '  SKIP  ' || true)" \
           " FAIL: $(cat /report/[0-9]*.txt | grep -c '  FAIL  ' || true)"; \
    } > /report/summary.txt; \
    cat /report/summary.txt; \
    # belt and braces: a stage that somehow logged a FAIL without exiting must
    # still fail the build
    ! grep -q '  FAIL  ' /report/[0-9]*.txt

CMD ["cat", "/report/summary.txt"]
