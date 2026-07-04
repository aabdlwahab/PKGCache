#!/usr/bin/env bash
# Launch the local control UI. Stdlib-only — no install step.
#
#   ./scripts/serve-ui.sh            # http://127.0.0.1:8088
#   UI_PORT=9000 ./scripts/serve-ui.sh
#
# Runs on the HOST (not in a container) because it drives git/dvc/docker compose,
# which live on the host. Binds to localhost only; it executes real commands.
set -euo pipefail
cd "$(dirname "$0")/.."
exec python3 webui/server.py
