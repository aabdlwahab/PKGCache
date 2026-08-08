#!/usr/bin/env bash
# Qualify all Phase-3 build-resolution mechanisms against an isolated Compose
# project and buildx builder. Requires a working Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/.."

CACHE_NAME="${CACHE_NAME:-pkgcache.internal}"
CACHE_IP="${CACHE_IP:-10.20.30.40}"
RUN_ID="pkgreg-phase3-$$"
BUILDER="${RUN_ID}-builder"
NETWORK="${RUN_ID}_default"
BUILDKIT_CONFIG="$(mktemp)"

cleanup() {
  docker buildx rm "$BUILDER" >/dev/null 2>&1 || true
  docker compose -p "$RUN_ID" --profile dns down --remove-orphans >/dev/null 2>&1 || true
  docker image rm -f \
    "${RUN_ID}-add-host" \
    "${RUN_ID}-compose-resolution-check" \
    "${RUN_ID}-buildkit-dns" >/dev/null 2>&1 || true
  rm -f "$BUILDKIT_CONFIG"
}
trap cleanup EXIT

echo "==> mechanism A: docker build --add-host"
docker build \
  --add-host "${CACHE_NAME}=${CACHE_IP}" \
  --build-arg "CACHE_NAME=${CACHE_NAME}" \
  --tag "${RUN_ID}-add-host" \
  examples/build-resolution

echo "==> mechanism A: Compose build.extra_hosts"
CACHE_NAME="$CACHE_NAME" CACHE_IP="$CACHE_IP" \
  docker compose -p "${RUN_ID}-compose" \
    -f examples/build-resolution/compose.yaml build

echo "==> mechanism B: forwarding DNS profile"
PKGCACHE_DNS_NAME="$CACHE_NAME" PKGCACHE_DNS_IP="$CACHE_IP" \
  PKGCACHE_DNS_BIND=127.0.0.1 \
  docker compose -p "$RUN_ID" --profile dns up -d --build --wait dns

DNS_CONTAINER="$(
  docker compose -p "$RUN_ID" --profile dns ps -q dns
)"
DNS_IP="$(
  docker inspect --format "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}" \
    "$DNS_CONTAINER"
)"
test -n "$DNS_IP"

docker run --rm --network "$NETWORK" --dns "$DNS_IP" alpine:3.22 \
  sh -ec "nslookup '$CACHE_NAME'; nslookup example.com"

echo "==> mechanism B: docker-container BuildKit [dns]"
printf '[dns]\n  nameservers = ["%s"]\n' "$DNS_IP" >"$BUILDKIT_CONFIG"
docker buildx create \
  --name "$BUILDER" \
  --driver docker-container \
  --driver-opt "network=$NETWORK" \
  --buildkitd-config "$BUILDKIT_CONFIG" \
  --bootstrap
docker buildx build \
  --builder "$BUILDER" \
  --no-cache \
  --build-arg "CACHE_NAME=${CACHE_NAME}" \
  --build-arg "EXPECT_DNS=1" \
  --tag "${RUN_ID}-buildkit-dns" \
  --load \
  examples/build-resolution

echo "Phase 3 build resolution: PASS"
