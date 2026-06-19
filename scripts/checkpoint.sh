#!/usr/bin/env bash
# Take a consistent, versioned checkpoint of the whole cache.
#
#   ./scripts/checkpoint.sh "added numpy 2.1 + curl"
#
# The ONLY tricky part is consistency: a blob must never be committed without
# its index entry, or the air-gapped side gets a corrupt cache. We get atomicity
# the simple way — stop the proxies for the few seconds it takes to hash. The
# Docker registry is content-addressed/immutable so it's safe regardless; npm
# (verdaccio) and pip (devpi) keep mutable indexes, which is why we quiesce.
#
# Fancier alternative if downtime is unacceptable: put ./caches on btrfs/ZFS/LVM,
# take an instant filesystem snapshot, and `dvc add` the snapshot instead.
set -euo pipefail
cd "$(dirname "$0")/.."

MSG="${1:?usage: checkpoint.sh \"commit message\"}"
PROFILE="${COMPOSE_PROFILE:-online}"

echo "==> quiescing proxies for a consistent snapshot"
docker compose --profile "$PROFILE" stop

# zot (the docker cache) writes its OCI layout as root with 0700 dirs, so a
# host user running the steps below can't read it to hash or manifest it. The
# other proxies already write world-readable trees; normalize zot to match.
# a+rX adds read + dir-traverse only (no write bits); DVC tracks content, not
# mode, so this produces no spurious diff. Runs in a root container because the
# files are root-owned. Proxies are stopped here, so it's a safe quiesced moment.
echo "==> normalizing cache permissions (read-only) so they're hashable"
docker run --rm -v "$PWD/caches:/caches" alpine:3.20 sh -c 'chmod -R a+rX /caches/docker'

echo "==> regenerating the cross-ecosystem manifest"
python3 scripts/gen_manifest.py

echo "==> hashing caches into DVC (per-file dedup; only new files become objects)"
dvc add caches/docker caches/npm caches/pip caches/apt

echo "==> committing pointers + manifest to git (this is the audit ledger)"
# dvc add writes the .dvc pointers and its own caches/.gitignore; stage both,
# plus the manifests and the top-level .gitignore. -A keeps it robust if a glob
# would match nothing on a given run (set -e would otherwise abort `git add`).
git add -A caches/ manifests/ .gitignore
git commit -m "checkpoint: ${MSG}"

echo "==> restarting proxies"
docker compose --profile "$PROFILE" start

echo "==> done. Run scripts/export-shuttle.sh to stage the delta for transfer."
