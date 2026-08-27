# The same Dockerfile, pointed at package-registry. See Dockerfile.before for the
# original and README.md for the walkthrough.
#
#   docker build -t myapp --build-arg HOST=<cache-host> examples/porting
#
# The shape of the port: you add a PREAMBLE (a base-image prefix and some env vars)
# and then leave your actual build steps ALONE. Every `pip`, `npm` and `git` line
# below the marker is byte-for-byte what it was before.
#
# TLS: the cache serves HTTPS from a private CA, so each client has to either be
# given certs/ca.crt or be told to trust this one host without verifying. This file
# does the latter — one env var per client, nothing to copy into the build context,
# and it works when <cache-host> is a bare IP (where pip's CA path does not, see
# README). That is the same posture as the rest of this stack: trusted, isolated
# networks. README.md has the verifying variant if you need it.

ARG HOST=cache.example.com
ARG PORT=8443
ARG PROJECT=global

# 1. BASE IMAGES — prefix the registry and the upstream. `dockerhub` selects Docker
#    Hub and official images keep `library/`; any other registry names itself —
#    `ghcr`, `quay`, `nvcr.io`, `gcr.io`, `public.ecr.aws` — with nothing configured
#    on the cache first. Kept as an ARG so CI can point it elsewhere without editing
#    this file.
#
#    A `FROM` is not the only line that fetches an image, and the other two are easy
#    to miss. A `# syntax=` directive names the frontend BuildKit parses this file
#    with, fetched before the first instruction is read; a `COPY --from=<image>` or a
#    `RUN --mount=from=<image>` borrows a published image rather than an earlier
#    stage. Both take this same prefix. Neither accepts a variable, so a borrowed
#    image that has to stay configurable goes through a stage — `ARG UV_IMAGE` before
#    the first `FROM`, `FROM ${UV_IMAGE} AS uv`, then `COPY --from=uv` — and the
#    directive is overridden with `--build-arg BUILDKIT_SYNTAX=…` instead.
ARG BASE_IMAGE=${HOST}:${PORT}/dockerhub/library/python:3.12-slim
FROM ${BASE_IMAGE}

ARG HOST
ARG PORT
ARG PROJECT

# 2. pip, uv and npm read their configuration from the environment, so setting it
#    once here is what lets the build commands further down stay untouched.
#    PIP_TRUSTED_HOST is per-host; npm's strict-ssl switch is global to npm.
#    uv ignores every PIP_* var and ships its own CA roots, so it needs the twin
#    pair (UV_INSECURE_HOST is uv >= 0.4.16). This example installs with pip; the
#    uv vars cost nothing and cover `uv pip install` / `uv sync` if you switch.
#    With a uv.lock, sync with `--locked` and never `--frozen`: --frozen installs
#    the URLs baked into the lock and ignores UV_INDEX_URL. See docs/docker-builds.md.
ENV PIP_INDEX_URL=https://${HOST}:${PORT}/${PROJECT}/pypi/root/pypi/+simple/ \
    PIP_TRUSTED_HOST=${HOST}:${PORT} \
    UV_INDEX_URL=https://${HOST}:${PORT}/${PROJECT}/pypi/root/pypi/+simple/ \
    UV_INSECURE_HOST=${HOST}:${PORT} \
    NPM_CONFIG_REGISTRY=https://${HOST}:${PORT}/${PROJECT}/npm/ \
    NPM_CONFIG_STRICT_SSL=false

# 3. apt — a forward proxy, so there is no URL to rewrite and no TLS involved: tell
#    apt to use it and keep the mirror lines on http (it does not tunnel HTTPS).
#    The project rides the proxy username; the password is an ignored label.
RUN printf 'Acquire::http::Proxy "http://%s:x@%s:3142";\n' "$PROJECT" "$HOST" \
        > /etc/apt/apt.conf.d/01pkgcache \
 && sed -i 's|https://|http://|g' /etc/apt/sources.list.d/*.sources 2>/dev/null || true; \
    apt-get update \
 && apt-get install -y --no-install-recommends git nodejs npm ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# 4. git — the second line is the highest-leverage one here: it redirects every
#    hardcoded github.com URL (submodules, `pip install git+https://…`, go modules,
#    CMake FetchContent) through the mirror without touching the URLs themselves.
#    sslVerify is scoped to the cache's URL, so github.com itself stays verified.
RUN git config --system http."https://${HOST}:${PORT}/".sslVerify false \
 && git config --system \
        url."https://${HOST}:${PORT}/${PROJECT}/git/github.com/".insteadOf \
        "https://github.com/"

# ---- from here down, NOTHING changed from Dockerfile.before ------------------
WORKDIR /app

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY package.json ./
RUN npm install --omit=dev

COPY app.py ./
CMD ["python", "app.py"]
