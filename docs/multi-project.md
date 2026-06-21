# Multi-project caches on one central instance

## Goal

Let the **single** central pkgcache instance serve multiple isolated *projects*,
each with:

- its **own set of URLs** (one port per ecosystem, dynamically allocated),
- its **own cache content** (separate on-disk trees),
- its **own version control** (a dedicated git + DVC repo) — checkpoint, rollback
  and history controlled independently from the web UI,
- its **own shuttle** (export/import carries only that project's blobs).

The existing **global** cache is unchanged: it keeps today's default URLs
(`5000/4873/3141/3142`) and its existing repo at `caches/`, and behaves exactly as
it does now. Everything below is purely additive.

## Locked decisions

1. **One process, always.** The central instance binds more sockets per project; it
   never forks a process or container per project.
2. **Port-set per project, dynamically allocated.** Each project gets one port per
   role (oci/npm/pip/apt). Ports are allocated **once at create time** from a pool,
   **persisted**, and never recomputed on boot (so a project's URLs are stable).
   No offset arithmetic.
3. **One git + DVC repo per project.** Separate cache trees per project — this is
   what makes a per-project shuttle fall out for free.
4. **The pool range is published once** in compose, so creating a project needs no
   container recreate; the process binds within the already-published range.
5. **Global project is implicit and reserved** — never allocated, never moved.

## Data model

### Project registry — `config/projects.json` (web-UI managed)

JSON, not YAML: the control UI is deliberately stdlib-only (no PyYAML), and both
the UI and the pkgcache process read the SAME file. Keyed by pkgcache **role**
names (`oci/npm/pypi/apt`); the reserved global ports are implicit (never stored).

```json
{
  "pool": {"start": 20000, "end": 20099},
  "projects": {
    "projA": {"oci": 20000, "npm": 20001, "pypi": 20002, "apt": 20003},
    "projB": {"oci": 20004, "npm": 20005, "pypi": 20006, "apt": 20007}
  }
}
```

Both processes locate it via `PKGCACHE_PROJECTS` (the webui defaults to
`config/projects.json` in the repo; pkgcache mounts `./config:/config:ro` and
reads `/config/projects.json`). It is host-specific state, so it is gitignored.

### On-disk layout

```
caches/                          # GLOBAL project — unchanged
  .git/ .dvc/                    #   its existing repo
  docker/ npm/ pip/ apt/         #   ledger.db + blobs per eco
caches/projects/<name>/          # one git + DVC repo PER project
  .git/ .dvc/
  docker/ npm/ pip/ apt/
```

## Allocator

Runs once, in the web UI's "create project" action:

1. in_use = `reserved` + every port already in `projects`.
2. Walk `pool.start..pool.end`, pick the 4 lowest free ports (skip in_use; probe the
   OS so we never hand out a port grabbed by something else).
3. Write the concrete `{oci,npm,pip,apt}` ports into the registry; persist.
4. Bootstrap `caches/projects/<name>/` (git init + dvc init on first checkpoint, same
   lazy bootstrap the global repo already uses).

Deleting a project frees its ports (returns them to the pool) and, optionally,
removes its tree after a confirmation.

## Implementation phases

Each phase is independently testable; the global project keeps working throughout.

### Phase 1 — Registry + allocator (no behavior change yet)
- New `webui/projects.py`: load/save `config/projects.yml`, `allocate(name)`,
  `free(name)`, `list_projects()`, `repo_dir(name)`, `ports(name)`.
- Unit-test allocation (next-free, reserved skipped, persistence, gaps reused).

### Phase 2 — Serving: bind per-project ports
- `pkgcache/src/pkgcache/core/config.py`: `load_all()` reads the registry and returns
  one `Config` per `(project, role)`; add `project: str` to `Config`; `cache_root =`
  `base/projects/<name>/<subdir>` for projects, `base/<subdir>` for global; `port`
  from the registry. No offset math.
- `pkgcache/src/pkgcache/app.py` / `__main__.py`: build and bind the full server set
  (global + all projects). Handlers in `handlers/` already take a `Config`, so each
  bound port serves its project with no per-request branching.
- Verify: a project's `pip --index-url https://host:<pip>/root/pypi/+simple/` caches
  into `caches/projects/<name>/pip/` and is isolated from global.

### Phase 3 — Live add (no container recreate)
- `app.py`: manage one server per `(project, role)` socket in the event loop; expose
  an internal "add project ports" action so a newly created project starts serving
  without restarting the process (in-flight downloads on other ports untouched).
- `docker-compose.yml`: publish the pool range once
  (`"20000-20999:20000-20999"`) alongside the four global ports.

### Phase 4 — Per-project version control (ops)
- `webui/ops.py`: replace module-level `CACHE_REPO` with a resolver
  `repo_for(project)` (global → `caches/`, else `caches/projects/<name>/`); thread a
  `project` param through `_checkpoint`, `_export`, `_import`, `_rollback`, `_mode`
  and `build()`. Default/global path stays byte-for-byte identical.
- `dvc add docker npm pip apt` runs inside the selected project's repo.

### Phase 5 — Web UI / API
- API: `GET /api/projects`, `POST /api/projects` (create → allocate + live add),
  `DELETE /api/projects/<name>`; scope existing checkpoint/history/shuttle endpoints
  by a `project` parameter.
- `webui/config.py`: derive `ENDPOINTS`, `PROGRESS_SOURCES`, `HEALTH_SOURCES` per
  project from its registry ports (instead of the hard-coded literals).
- Console: a project switcher; History / checkpoint / shuttle panels operate on the
  selected project; a "New project" form that shows the allocated URLs.

## Edge cases & decisions to honor

- **Stable URLs:** never re-allocate on boot — read persisted ports only.
- **Pool exhaustion:** allocator raises a clear error when no 4 free ports remain.
- **Port stolen externally:** OS probe at allocation time; on bind failure, surface
  per-port instead of failing the whole instance.
- **Project name validation:** restrict to `[a-z0-9-]` (path-safe, URL-safe).
- **TLS:** the in-process HTTPS roles reuse the same server cert; the cert's SANs
  must cover the host the projects are reached on (ports don't affect SAN). Document
  this; wildcard host names are out of scope (we chose ports, not vhosts).
- **Dedup (future, optional):** projects use separate trees, so no cross-project
  dedup. A shared DVC remote can be layered later without changing the topology.

## Implementation status

Backend implemented and unit/integration-tested (`webui/test_projects.py`,
`webui/test_multiproject.py`):

- **Registry + allocator** — `webui/projects.py` (next-free, persisted, reserved
  ports skipped, pool-exhaustion error, name validation).
- **Serving** — `pkgcache/core/config.py` `load_all()` emits global + per-project
  configs; `Config.project`; `/healthz` reports the project.
- **Live bind/unbind** — `pkgcache/__main__.py` supervisor polls the registry and
  starts/stops servers without a process restart; compose publishes the pool range
  (`20000-20099`) and mounts `./config`; Dockerfile updated.
- **Per-project VC + shuttle** — `webui/ops.py` (checkpoint/export/import/rollback)
  threaded by `project`; per-project export/import dirs; `project.json` travels in a
  named project's shuttle so import re-registers it; `gen_manifest.py` honors
  `PKGCACHE_MANIFEST_ROOT`; `scripts/pkgops.py --project`.
- **API + scoped reads** — `GET/POST /api/projects`, `DELETE /api/projects/<name>`;
  `?project=` on `/api/manifests`, `/api/history`, `/api/endpoints`, `/api/shuttle`,
  `/api/packages`; `config.endpoints/progress_sources/health_sources(project)`.

- **Live progress aggregation** — `webui/live.py` polls every project's ports
  (bounded thread pool, project list refreshed each cycle); `/api/proxies`,
  `/api/downloads`, `/api/recent` are now `?project=`-scoped.
- **React console** — `webui/console` has a TopBar project switcher (select +
  create + delete), a `pcc_project` selection persisted in localStorage, and all
  panels re-poll the selected project. `api.ts` carries the `project` query param
  (global omits it, so its requests are unchanged) and adds project CRUD; every
  cache op (`start`) is scoped to the selection. Build with the existing
  `webui/console` Node→nginx image (no new deps).

## Caveats

- **First checkpoint of an empty project:** `dvc add` over empty cache dirs may
  warn/skip; checkpoint after at least one artifact has been cached for the project.
- **Air-gapped port collisions:** a named project's import reuses the ports the
  online side assigned; if those are taken on the air-gapped host the bind is logged
  and skipped (edit the registry to relocate).

## Out of scope

- Path-prefix or hostname/vhost URL models (we chose port-set).
- Cross-project deduplication.
- Per-project auth (the console remains trusted-network only, as today).
