# Image for the control UI. Unlike the six cache roles, this service versions the
# cache state, so it carries git and dvc. It has NO docker CLI and gets no docker
# socket: mode changes are registry writes the cache process applies live, and
# container lifecycle is a host concern. Build it on the online side; the
# air-gapped side just pulls the built image out of the docker cache like any
# other.
#
# The server itself is pure stdlib, so there's nothing to install for it.
FROM python:3.12-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && pip install --no-cache-dir dvc \
 && rm -rf /var/lib/apt/lists/*

# Source is bind-mounted at runtime at /repo (see the compose service); the backend
# resolves every path relative to its own source tree. Nothing is COPYed in.
EXPOSE 8088
CMD ["python3", "webui/server.py"]
