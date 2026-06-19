# Image for the control UI. Unlike the four proxies, this service is not a
# package cache — it drives git, dvc and `docker compose` against the host, so
# it carries those CLIs and talks to the host's Docker socket (mounted at
# runtime). Build it on the online side; the air-gapped side just pulls the
# built image out of the docker cache like any other.
#
# The server itself is pure stdlib, so there's nothing to install for it.
FROM python:3.12-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      git ca-certificates curl gnupg \
 && install -m 0755 -d /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
 && chmod a+r /etc/apt/keyrings/docker.asc \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
      > /etc/apt/sources.list.d/docker.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin \
 && pip install --no-cache-dir dvc \
 && apt-get purge -y curl gnupg && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*

# Source is bind-mounted at runtime at the host repo's own path (see the compose
# service), so the UI's `docker compose` resolves the same ./caches paths the
# host daemon expects. Nothing is COPYed in.
EXPOSE 8088
CMD ["python3", "webui/server.py"]
