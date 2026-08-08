# Image for the control UI. Unlike the six cache roles, this service versions the
# cache state, so it carries git and dvc. It has NO docker CLI and gets no docker
# socket: mode changes are registry writes the cache process applies live, and
# container lifecycle is a host concern. Build it on the online side; the
# air-gapped side just pulls the built image out of the docker cache like any
# other.
#
# The server itself is pure stdlib, so there's nothing to install for it.
FROM python:3.12-slim@sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de

COPY requirements.lock /tmp/requirements.lock
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && pip install --no-cache-dir --upgrade pip==26.1.2 \
 && pip install --no-cache-dir -r /tmp/requirements.lock \
 && pip check \
 && rm /tmp/requirements.lock \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY app ./webui/app
COPY server.py ./webui/server.py

# Runtime state is mounted separately under /app; application source stays
# immutable inside the image.
EXPOSE 8088
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8088/healthz', timeout=3)"
CMD ["python3", "webui/server.py"]
