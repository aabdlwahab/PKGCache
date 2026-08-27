# A reusable integration-test client for all six pkgcache repository roles.
#
# Deliberately no `# syntax=` directive. It names the frontend BuildKit parses this
# file with, and that image is fetched from Docker Hub before the first instruction is
# read — fatal on an air-gapped builder, and absurd in a file whose point is that every
# image comes through the cache. Nothing here needs a frontend newer than the built-in
# one. A build that does can take its frontend from the cache too, without naming an
# address in the file:
#
#   docker build --build-arg BUILDKIT_SYNTAX=cache.example:8443/dockerhub/docker/dockerfile:1.7 …
#
# The registry prefix is deliberately configurable: leaving BASE_REGISTRY at its
# default builds directly from Docker Hub, while pointing it at pkgcache makes the
# FROM lines a real OCI pull-through example too:
#
#   docker build \
#     --build-arg BASE_REGISTRY=cache.example:8443/dockerhub \
#     -t pkgcache-six-repositories .
#
# The resulting image runs the protocol tests at container *runtime*. That keeps
# the cache address, public CA, and files write token out of image layers and lets
# the same image test online warm/hit behavior and offline replay.
ARG BASE_REGISTRY=docker.io
ARG ALPINE_IMAGE=library/alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
ARG NODE_IMAGE=library/node:20-bookworm-slim@sha256:2cf067cfed83d5ea958367df9f966191a942351a2df77d6f0193e162b5febfc0
ARG RUNNER_IMAGE=library/python:3.12-slim-bookworm@sha256:d50fb7611f86d04a3b0471b46d7557818d88983fc3136726336b2a4c657aa30b

# Keep an Alpine root filesystem in the runner so the apt role is tested through
# both supported clients (apt and apk) without requiring a Docker socket or daemon.
FROM ${BASE_REGISTRY}/${ALPINE_IMAGE} AS alpine-rootfs

# Debian's npm package drags a full web-development toolchain into the final
# image. Copy the official Node distribution instead; only node + npm land in the
# runner, not this stage's filesystem.
FROM ${BASE_REGISTRY}/${NODE_IMAGE} AS node-client

FROM ${BASE_REGISTRY}/${RUNNER_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        jq \
        libstdc++6 \
        skopeo \
    && rm -rf /var/lib/apt/lists/*

COPY --from=node-client /usr/local/bin/node /usr/local/bin/node
COPY --from=node-client /usr/local/lib/node_modules /usr/local/lib/node_modules
COPY --from=alpine-rootfs / /opt/alpine/
COPY verify.sh /usr/local/bin/pkgcache-verify

RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
    && node --version \
    && npm --version \
    && python --version \
    && chmod 0755 /usr/local/bin/pkgcache-verify \
    && mkdir -p /results

ENV CACHE_HOST=localhost \
    CACHE_HTTPS_PORT=8443 \
    CACHE_APT_PORT=3142 \
    PROJECT=global \
    TEST_PHASE=online \
    CA_CERT=/certs/ca.crt \
    FILES_TOKEN_FILE=/run/secrets/files_token \
    RESULTS_DIR=/results

VOLUME ["/results"]
ENTRYPOINT ["/usr/local/bin/pkgcache-verify"]
