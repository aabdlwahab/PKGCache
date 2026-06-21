# One slim image for all four roles. The role is chosen at runtime via the
# PKGCACHE_ROLE env var (oci | npm | pypi | apt) — see docker-compose.yml.
FROM python:3.12-slim

# uvicorn[standard] pulls in uvloop/httptools wheels; no build toolchain needed.
WORKDIR /app
COPY pyproject.toml ./
COPY src ./src
RUN pip install --no-cache-dir .

# Default config baked in. With PKGCACHE_ROLE unset, the entrypoint serves ALL
# four roles in one process — each on its own port, HTTPS roles terminating TLS
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
# in-process). Binds the global 5000/4873/3141 (TLS) + 3142 (HTTP), plus four
# ports per registered project from the published pool range (20000-20099).
EXPOSE 5000 4873 3141 3142 20000-20099
ENTRYPOINT ["python", "-m", "pkgcache"]
