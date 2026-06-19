#!/usr/bin/env bash
# Apply a shuttle drive on the AIR-GAPPED host. First run clones; later runs
# fast-forward and pull only the new objects.
#
#   ./scripts/import-airgap.sh /media/shuttle
set -euo pipefail

DRIVE="${1:?usage: import-airgap.sh /path/to/mounted/drive}"
STORE="$DRIVE/dvcstore"
BUNDLE="$DRIVE/repo.bundle"
REPO="${REPO_DIR:-$HOME/package-cache}"

if [ ! -d "$REPO/.git" ]; then
  echo "==> first import: cloning from bundle"
  git clone "$BUNDLE" "$REPO"
  cd "$REPO"
else
  echo "==> incremental import: fast-forwarding from bundle"
  cd "$REPO"
  # The air-gapped side is a pure mirror — it never commits, so a fast-forward
  # is always possible. Fetch the bundle's branches into remote-tracking refs,
  # then ff-merge the current branch. (Pulling the literal ref "HEAD" was both
  # branch-name-fragile and would fail against a --all bundle.)
  BRANCH=$(git rev-parse --abbrev-ref HEAD)
  git fetch "$BUNDLE" "+refs/heads/*:refs/remotes/shuttle/*"
  git merge --ff-only "refs/remotes/shuttle/$BRANCH"
fi

# Point this side's DVC at the drive and materialize the caches.
dvc remote add -f shuttle "$STORE"
echo "==> fetching new DVC objects from shuttle (delta only)"
dvc pull -r shuttle

echo "==> materializing cache dirs (dvc checkout)"
dvc checkout

# TLS certs ride the shuttle (not git), so the offline tls-proxy serves HTTPS
# with the same CA as online. Without them the proxy can't start.
if [ -d "$DRIVE/certs" ]; then
  echo "==> installing TLS certs from shuttle (same CA as the online side)"
  mkdir -p certs
  cp -a "$DRIVE/certs/." certs/
else
  echo "WARNING: no certs/ on the shuttle — run scripts/gen-certs.sh on the online"
  echo "         side and re-export, or the HTTPS proxy won't have a certificate."
fi

echo "==> bringing up air-gapped proxies (serve-only)"
COMPOSE_PROFILE=offline docker compose --profile offline up -d

echo "==> done. Point air-gapped clients at (install certs/ca.crt to trust these):"
echo "    pip   ->  https://<host>:3141/root/pypi/+simple/"
echo "    npm   ->  https://<host>:4873/"
echo "    docker->  <host>:5000   (zot: pull <host>:5000/dockerhub/library/<img>, /ghcr/<org>/<img>, /quay/<org>/<img>)"
echo "    apt   ->  http://<host>:3142/   (plain HTTP proxy; apk too)"
