#!/usr/bin/env bash
# Stage everything the air-gapped side needs onto the shuttle drive.
# Run this on the ONLINE host after one or more checkpoints.
#
#   ./scripts/export-shuttle.sh /media/shuttle
#
# Two things cross the gap:
#   1. The DVC objects (the actual package bytes)  -> `dvc push` to a local remote
#      on the drive. DVC only copies objects the drive doesn't already have, so
#      a re-export after adding three packages moves ~those three packages.
#   2. The git history (manifests + .dvc pointers)  -> a `git bundle`. We write a
#      FULL `--all` bundle every time. It's tempting to make this incremental too,
#      but a range bundle (`PREV..HEAD`) carries prerequisites, not full history —
#      so a brand-new air-gapped host couldn't `git clone` it. The history here is
#      just manifests + tiny pointer files (KBs), utterly dwarfed by the package
#      bytes DVC moves, so a full, always-cloneable bundle costs nothing real.
set -euo pipefail
cd "$(dirname "$0")/.."

DRIVE="${1:?usage: export-shuttle.sh /path/to/mounted/drive}"
STORE="$DRIVE/dvcstore"
BUNDLE="$DRIVE/repo.bundle"

mkdir -p "$STORE"

# Register the drive as a DVC remote named 'shuttle' (idempotent).
dvc remote add -f shuttle "$STORE"

echo "==> pushing DVC objects to shuttle (delta only)"
dvc push -r shuttle

echo "==> bundling full git history to shuttle (always self-contained/cloneable)"
# Write to a temp name and atomically swap, so an interrupted export never
# leaves a truncated bundle on the drive.
git bundle create "$BUNDLE.new" --all
mv "$BUNDLE.new" "$BUNDLE"

# The TLS server key/cert are git-ignored (keys don't belong in git), so they
# don't ride the bundle. Carry them on the trusted shuttle instead, so the
# air-gapped side serves HTTPS with the SAME CA — build hosts then trust both
# sides from one installed ca.crt.
if [ -d certs ]; then
  echo "==> copying TLS certs to shuttle (CA + server cert)"
  mkdir -p "$DRIVE/certs"
  cp -a certs/. "$DRIVE/certs/"
fi

echo "==> shuttle ready at $DRIVE — carry it to the air-gapped network."
