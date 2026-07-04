# One slim image for all six roles. The role is chosen at runtime via the
# PKGCACHE_ROLE env var (oci | npm | pypi | apt | git | files) — see docker-compose.yml.
FROM python:3.12-slim

# The git role is a mirror-and-serve smart-HTTP server: it shells out to the git
# binary (clone --mirror / fetch / upload-pack), which python:3.12-slim lacks.
# ca-certificates so git trusts public upstream hosts (github.com, …) by default.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# uvicorn[standard] pulls in uvloop/httptools wheels; no build toolchain needed.
WORKDIR /app
COPY pyproject.toml ./
COPY src ./src
RUN pip install --no-cache-dir .

# Default config baked in. With PKGCACHE_ROLE unset, the entrypoint serves ALL
# five roles in one process — each on its own port, HTTPS roles terminating TLS
# from PKGCACHE_TLS_CERT/KEY — caching under PKGCACHE_CACHE_ROOT/<eco>. Set
# PKGCACHE_ROLE to run a single role instead (dev / one-role-per-process).
COPY pkgcache.yaml /etc/pkgcache/pkgcache.yaml
ENV PKGCACHE_CONFIG=/etc/pkgcache/pkgcache.yaml \
    PKGCACHE_CACHE_ROOT=/caches \
    PKGCACHE_PROJECTS=/config/projects.json \
    PKGCACHE_TLS_CERT=/certs/server.crt \
    PKGCACHE_TLS_KEY=/certs/server.key \
    OFFLINE=0

# One single-worker process (the progress registry and single-flight state are
# in-process). Binds the global 5000/4873/3141/3143/3144 (TLS) + 3142 (HTTP), plus
# six ports per registered project from the published pool range (20000-20099).
EXPOSE 5000 4873 3141 3142 3143 3144 20000-20099
ENTRYPOINT ["python", "-m", "pkgcache"]
