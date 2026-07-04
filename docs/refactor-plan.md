# Refactor plan: layered backend + SPA frontend

Goal: keep the two-process split (React console ↔ Python control API) but make the
backend explicitly layered — controllers (HTTP) → services (domain) → gateways
(subprocess / files / pkgcache HTTP) — and move data ownership to where it belongs.
MVC reading: the console is the View, `app/api` the Controller, services+gateways
the Model. No server-side rendering; the console stays the only renderer.

Standing decisions:

1. webui stays stdlib-only; the layering is framework-agnostic (a later FastAPI
   swap would touch only `app/api/`).
2. The project registry stays a shared JSON file (`config/projects.json`) between
   webui (writer) and pkgcache (reader).
3. The `Operations`-generator-of-log-lines pattern shared with `scripts/pkgops.py`
   is kept unchanged.
4. Console API wire shapes stay frozen until Phase 5, so backend phases never
   break the running UI.

## Phase 0 — stabilize the baseline  ✅ (this commit series)

- 0.1 Commit the in-flight prefix-migration working tree; drop the stray
  `uv (1).lock`.
- 0.2 Bug: the console files-upload proxy (`server.py _files_proxy`) built the
  target URL without `role_prefix(project, "files")`, so named-project uploads
  landed in the global tree. Fixed + regression test.
- 0.3 Bug: checkpoint's git-mirror maintenance posted `/+maintain` without
  `role_prefix(project, "git")`, repacking the global mirrors instead of the
  project's. Fixed + regression test.
- 0.4 Bug: the DELETE `/api/projects/<name>` route regex predated the new name
  grammar (`.`/`_` now legal), so such projects could be created but not deleted.
  Fixed + regression test.
- 0.5 Test harness (see “Verification” below).

## Phase 1 — package-ify the backend (pure move)

Target tree:

    webui/
      server.py                # shim: from app.main import main; main()
      app/
        main.py                # wiring: gateways → services → Handler; serve
        settings.py            # HOST/PORT/ROOT/CACHE_REPO/ECOS map — constants only
        errors.py              # ApiError(status); ProjectError/OpError subclass it
        urls.py                # endpoints()/progress_sources() derivation
        api/                   # handler.py, routes.py, files_proxy.py
        services/              # operations, jobs, lockwarm, livefeed, reads,
                               #   projects, usage
        gateways/              # proc.py (run(), git trust env, log parsing),
                               #   registry.py, ledgers.py, pkgcache.py (ONE http
                               #   client owning ssl ctx + role_prefix URL building)
      tests/

Also: stop mutating `sys.path` to import `scripts/gen_manifest` (move the
ECOS/CACHES constants into `app/settings.py`; gen_manifest becomes a thin wrapper
like pkgops). Update the two external importers: `scripts/pkgops.py` and the tests.

## Phase 2 — controller layer

- One declarative route table (method, pattern, handler, scoped) replacing the
  four dispatch styles in `do_GET`/`do_POST`.
- One error contract: `ApiError(status)`; delete every blanket
  `except (ValueError, RuntimeError)` so real bugs become 500s, not silent 400s.
- Purge model leaks: `proxies()` composition → a service; `Reads.packages` takes
  typed kwargs, not a `parse_qs` dict; files proxy handler delegates to the
  pkgcache gateway.
- Delete the legacy single-file UI (`webui/index.html`) and its `/` route.
- Job log tailing: `GET /api/jobs/<id>?offset=N` returns the log suffix
  (back-compat when omitted); `useJob` appends. Kills O(n²) polling.

## Phase 3 — structured data out of the backend

- ✅ `docs/api.md`: hand-written contract table for the whole surface (the OpenAPI
  substitute while stdlib-only). Done.
- ⏸ `/api/endpoints` returns `{scheme, port, prefix, path, transport, hint}` per
  eco instead of pre-formatted display strings; EndpointsPanel renders them.
  DEFERRED — it's a breaking change requiring a coordinated EndpointsPanel + types.ts
  edit, and the console can't be typechecked/built in the current env (no node/npm).
  Do it together with Phase 5, once a console build is available.

## Phase 1 follow-up (done out of order)

- ✅ Registry file I/O split into `app/gateways/registry.py` (load/save/LOCK, path
  resolved from the env per call); `services/projects.py` re-exports load_registry /
  save_registry so callers and tests are unchanged. This was the deferred Phase 1
  item — it completes the layering (no service does raw file I/O) and is fully
  unit-tested (corrupt-file → ApiError; env-driven path).

## Phase 4 — ledger reads behind pkgcache (independent after Phase 1)  ✅ DONE

Implemented: pkgcache serves `GET /+ledger/artifacts` (wraps `Ledger.query`, with
page_size<=0 → all rows) and `GET /+ledger/stats` (new `Ledger.stats()`), registered
before the greedy handler routes. The webui `pkgcache` gateway fetches these per
(project, role) — prefix-aware, concurrent for stats — with a last-good cache; the
reads service combines them and no longer opens `ledger.db` (the `ledgers` gateway is
deleted). Verified: pkgcache pytest (Ledger.query/stats + TestClient routes), webui
unit tests (mocked gateway + stats-combine), and a real-HTTP round-trip (webui gateway
↔ a live pkgcache role from the test venv). Original plan text below.


- pkgcache: per-(project, role) admin routes beside `/+progress`:
  `GET /+ledger/artifacts?q=&sort=&page=&full=` and `GET /+ledger/stats`
  (implemented on `core.ledger`, threadpool for sqlite). Tests beside
  `tests/test_router.py`.
- webui: `services/reads.py` keeps the cross-role aggregation but fetches rows
  via the pkgcache gateway (6-role fan-out, short TTL cache) — the sqlite
  duplication of `Ledger.query` is deleted.
- Degradation: pkgcache down → serve the last-cached snapshot with an `age`
  field (LiveFeed pattern).
- Stays file-based, deliberately: `gen_manifest.py` (checkpoint-time snapshot),
  `usage.py`'s disk walk, the registry JSON.

## Phase 5 — frontend restructure

STATUS: blocked on a console build environment (no node/npm in the dev sandbox where
this refactor ran, so nothing here can be typechecked/built/run). Done so far: the
safe, no-layout-risk bits — job-log tailing in useJob (Phase 2.5) and the hardcoded
"7 ecosystems" KPI now uses ECOS.length. Deferred until a build is available: 5.1
(TanStack Query — also adds a dependency not in package.json), 5.2 (the 15-file
feature-folder + context migration), 5.3 (global JobConsole — moves where the console
renders, a layout change that needs visual verification). Phase 3.1 (structured
/api/endpoints) is deferred for the same reason — it reshapes 4 console components
(EndpointsPanel, ArtifactsPanel, PackagesPanel, types) to render data instead of the
backend's preformatted strings, and can't be verified without a build.

Original plan below.

- Adopt TanStack Query: `refetchInterval` replaces `usePolling`,
  `invalidateQueries` replaces the `refreshKey` bump, `enabled:` replaces the
  stats-tab poll hack. Bundled at build time — air-gap unaffected.
- Feature folders (health / activity / inventory / transfer / lockwarm /
  artifacts / endpoints), each owning its queries; `App.tsx` becomes a ~60-line
  shell; `ProjectContext` + `JobContext` replace the prop drill.
- ONE global JobConsole drawer; panels stop filtering `job.action`.
- Small fixes: `ECOS.length` not "7 ecosystems"; inline errors instead of
  `window.alert`.

## Phase 6 — optional hardening

SSE job streaming; `/api/projects/<name>/…` nesting with aliases; revisit
stdlib-only; registry behind a pkgcache write API (parked).

## Order

    Phase 0 → 1 → 2 → 3 → 5
              └→ 4 (independent after 1)

## Verification (every phase)

    cd webui && python3 -m unittest test_projects test_multiproject test_lockwarm
    cd pkgcache && python3 -m venv .venv-test && .venv-test/bin/pip install -e . pytest httpx \
        && .venv-test/bin/python -m pytest tests/ -q
    docker compose --profile online --profile ui build
    cd webui/console && npm run build            # console phases

Manual smoke: create project → generate write token → upload artifact →
checkpoint → export → import → rollback; verify prefixed URLs
(`/<project>/<role>/…`, `/v2/<project>/…`, apt proxy-username) serve correctly.
