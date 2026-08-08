# One slim image for all six roles. The role is chosen at runtime via the
# PKGCACHE_ROLE env var (oci | npm | pypi | apt | git | files) — see docker-compose.yml.
FROM python:3.12-slim@sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de

# The git role is a mirror-and-serve smart-HTTP server: it shells out to the git
# binary (clone --mirror / fetch / upload-pack), which python:3.12-slim lacks.
# ca-certificates so git trusts public upstream hosts (github.com, …) by default.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# uvicorn[standard] pulls in uvloop/httptools wheels; no build toolchain needed.
WORKDIR /app
COPY pyproject.toml requirements.lock ./
COPY src ./src
RUN pip install --no-cache-dir --upgrade pip==26.1.2 \
    && pip install --no-cache-dir -r requirements.lock \
    && pip install --no-cache-dir --no-deps . \
    && pip check

# Default config baked in. With PKGCACHE_ROLE unset, the entrypoint serves ALL
# six roles in one process on TWO ports: the unified HTTPS port (docker at /v2 +
# npm/pypi/git/files at /<project>/<role>, TLS from PKGCACHE_TLS_CERT/KEY) and the
# apt/apk plain-HTTP forward proxy — caching under PKGCACHE_CACHE_ROOT/<eco>. Set
# PKGCACHE_ROLE to run a single role instead (dev / one-role-per-process).
COPY pkgcache.yaml /etc/pkgcache/pkgcache.yaml
ENV PKGCACHE_CONFIG=/etc/pkgcache/pkgcache.yaml \
    PKGCACHE_CACHE_ROOT=/caches \
    PKGCACHE_PROJECTS=/config/projects.json \
    PKGCACHE_TLS_CERT=/certs/server.crt \
    PKGCACHE_TLS_KEY=/certs/server.key \
    OFFLINE=0

# One single-worker process (the progress registry and single-flight state are
# in-process). Binds the unified HTTPS port (8443, PKGCACHE_UNIFIED_PORT) + the
# apt/apk proxy (3142). Projects share these; no per-project ports.
EXPOSE 8443 3142
ENTRYPOINT ["python", "-m", "pkgcache"]
