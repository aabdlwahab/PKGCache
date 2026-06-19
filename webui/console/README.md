# pkgcache console

A full React + TypeScript + Vite frontend for the air-gap package cache — the
operator dashboard recreated from the hi-fi handoff (`design_handoff_pkgcache_console/`).
It replaces the old single-file `webui/index.html`.

## Architecture

```
browser ──▶ console (nginx, :8088)
                ├── /            → static SPA bundle (this app)
                └── /api/*       → reverse-proxied to webui:8088 (Python control UI)
```

- **Frontend:** React 18 + TypeScript, built with Vite into static assets.
- **Backend:** unchanged Python control UI (`webui/`), now an API-only service split
  into thin modules (`config`, `reads`, `live`, `jobs`, `server`). It reads the
  per-ecosystem SQLite ledgers and shells out to the checkpoint/export/import scripts.
- **Serving:** a separate `console` container (multi-stage Node→nginx image). No Node
  is needed at runtime or on the air-gapped host — only at build time on the online side.

## Develop

Needs Node 20+. The dev server proxies `/api` to a running Python webui:

```bash
cd webui/console
npm install
# point at a reachable webui (default http://127.0.0.1:8088)
PKGCACHE_WEBUI=http://127.0.0.1:8088 npm run dev   # http://localhost:5173
```

Run the backend separately for live data: `python3 webui/server.py`.

Other scripts: `npm run build` (typecheck + bundle to `dist/`), `npm run typecheck`,
`npm run preview`.

## Build & run in the stack

```bash
docker compose --profile online --profile ui up -d --build console webui
# open http://<host>:8088
```

`webui` no longer publishes a host port; the `console` container is the public entry
on `:8088` and proxies `/api` to it over the compose network.

## Fonts (air-gap)

The design specifies self-hosted IBM Plex. Drop the woff2 files in
`public/fonts/` (see `public/fonts/README.md`) before building. If absent, the app
falls back to system mono/sans — functional, just not pixel-identical.

## Project layout

```
src/
  main.tsx              entry
  App.tsx               layout + polling + theme/mode state
  lib/
    types.ts            API response types
    api.ts              typed fetch wrappers (/api/*)
    format.ts           fmtBytes / relTime / ecosystem OKLCH colors
    uiState.ts          Theme | Mode | SortKey
  hooks/
    usePolling.ts       interval poller (abortable, keeps last good data)
    useLocalStorage.ts  persisted theme/mode
    useClock.ts         1s tick for relative times
    useJob.ts           POST a job, then poll /api/jobs/<id> to completion
  components/
    ui.tsx              Panel / Segmented / LiveDot / EcoChip
    Chrome.tsx          TopBar / OfflineBanner / Footer
    PackagesPanel.tsx   centerpiece: filter / sort / group by ecosystem
    DownloadsPanel.tsx  live in-flight downloads
    RecentPanel.tsx     HIT / MISS feed
    ActionsPanel.tsx    checkpoint / export / import + streaming job console
    HistoryPanel.tsx    git checkpoints + rollback
    EndpointsPanel.tsx  copy-paste pull endpoints
  styles/
    tokens.css          OKLCH themes (dark/light) + fixed skin (mono type, blue accent) + font
    app.css             layout + component styles
```

## Live integrations

The three formerly-missing backend pieces are now implemented end to end:

- **Online/offline switch** — the top-bar toggle runs a real `mode` job that
  recreates the `pkgcache` container under the target profile (with `OFFLINE` set),
  streamed in the job console. The indicator reflects the *actual* state from
  `/api/proxies` (per-role `/healthz`), optimistic only during the restart window.
- **"N proxies up"** — a real count from per-role `/healthz` probes the webui caches
  (`roles_health()` → `/api/proxies` `up`/`offline`/`roles`), not a guess.
- **FAIL feed** — the proxies record offline cache-misses and upstream failures with
  a `failed` flag (`progress.record_recent(..., failed=True)`); the feed renders
  these as red `FAIL` rows, distinct from a normal upstream `MISS`.
