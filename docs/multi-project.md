# Multi-project caches on one central instance

## Goal

Let the **single** central pkgcache instance serve multiple isolated *projects*,
each with:

- its **own set of URLs** (a per-project prefix on the shared role ports),
- its **own cache content** (separate on-disk trees),
- its **own version control** (a dedicated git + DVC repo) — checkpoint, rollback
  and history controlled independently from the web UI,
- its **own shuttle** (export/import carries only that project's blobs).

The existing **global** cache is unchanged: it keeps today's default root URLs
(`5000/4873/3141/3142/3143/3144`) and its existing repo at `caches/`, and behaves
exactly as it does now. Everything below is purely additive.

## Locked decisions

1. **One process, one set of ports.** The central instance runs six role servers on
   the six default ports and never forks a process, container, **or extra socket**
   per project. Projects are distinguished by the request, not by a port.
2. **Project rides a URL prefix, not a port.** How the project is carried is dictated
   by each protocol (see *Routing* below): a path prefix where the protocol allows
   it, the image name for Docker, the proxy username for apt.
3. **One git + DVC repo per project.** Separate cache trees per project — this is
   what makes a per-project shuttle fall out for free.
4. **The registry is just names.** Creating a project is a name written to
   `config/projects.json`; there is nothing to allocate, probe, or persist beyond the
   name (+ its files write token). A supervisor picks it up on its next poll.
5. **Global project is implicit and reserved** — never stored, never prefixed.

## Routing — how the project is carried per protocol

Docker can't be given a base path (the registry API is pinned to `/v2/` at the
root), and apt is a *forward proxy* (the client sends absolute upstream URLs, so
there's no path to prefix). The other four take a clean path prefix. So:

| Role | Global | Named project `<p>` | Mechanism |
|---|---|---|---|
| npm / pypi / git / files | root URL | `/<p>/<role>/…` | leading path segment |
| oci | `/v2/dockerhub/…` | `/v2/<p>/dockerhub/…` | first segment of the **image name** |
| apt / apk | `http://HOST:3142` | `http://<p>@HOST:3142` | **proxy username** (password ignored) |

A `RoleServer` (`pkgcache/router.py`), one per role port, inspects each request,
selects the project (falling back to **global**), strips the marker, and hands the
request to that project's sub-app:

- **path roles** — if segment 1 is a registered project *and* segment 2 is the role
  name, strip `/<p>/<role>` and set `scope["root_path"]` to it (so `external_base()`
  re-emits project-scoped links in rewritten pypi indexes, npm packuments, git LFS
  and files URLs). Both segments must match, so a real npm package named `gamma` (a
  single-segment request) is never mistaken for a project.
- **oci** — for `/v2/<seg>/…`, if `<seg>` is a registered project, rewrite the path
  to `/v2/…` and stash the project so `tags/list` can re-prefix the echoed `name`.
  Response bodies are otherwise content-addressed by digest, so nothing else needs
  rewriting. `/v2/` (ping) and `/v2/_progress` stay global.
- **apt** — read the project from the `Proxy-Authorization` username. Internal
  progress/health pollers, which can't set a proxy user, may also use a
  `/<p>/apt/…` path form (a real proxied request arrives with an absolute-URL path,
  so the two never collide).

### Reserved names

Because the first path/image/`v2` segment now means "project", a project may not be
named anything that already has a meaning there: `global`, the six role names
(`oci/npm/pypi/apt/git/files`), the OCI upstream aliases
(`dockerhub/ghcr/quay`), `root` (the default pypi index prefix), or `v2`. Names are
validated to the Docker image-name grammar (lowercase alnum separated by single
`.`/`_`/`-`), which is the tightest of the clients.

## Data model

### Project registry — `config/projects.json` (web-UI managed)

JSON, not YAML: the control UI is deliberately stdlib-only (no PyYAML), and both the
UI and the pkgcache process read the SAME file. Project entries are **name-only**
objects (there are no ports to store):

```json
{
  "projects": {
    "projA": {},
    "projB": {}
  },
  "tokens": { "projA": "<files write token>", "global": "<token>" }
}
```

Both processes locate it via `PKGCACHE_PROJECTS` (the webui defaults to
`config/projects.json` in the repo; pkgcache mounts `./config:/config:ro` and reads
`/config/projects.json`). It is host-specific state, so it is gitignored. An older
registry that still carries a `pool` block and per-project port maps loads fine —
the stale ports are simply ignored.

### On-disk layout

```
caches/                          # GLOBAL project — unchanged
  .git/ .dvc/                    #   its existing repo
  docker/ npm/ pip/ apt/ git/ files/   #   ledger.db + blobs per eco
caches/projects/<name>/          # one git + DVC repo PER project
  .git/ .dvc/
  docker/ npm/ pip/ apt/ git/ files/
caches/.dvc-shared/              # OFFLINE only: one DVC object store shared by all
                                 #   repos so an artifact imported for one project
                                 #   is not re-copied for the next (see below)
```

## Lifecycle

Runs in the web UI's "create project" action (or `POST /api/projects`):

1. Validate the name (grammar + reserved set); reject duplicates.
2. Write `{"<name>": {}}` into the registry; persist (atomic temp→rename).
3. Bootstrap `caches/projects/<name>/<eco>/` dirs so the first checkpoint has
   something to `dvc add` and live reads don't 404 (git/dvc self-init lazily, as the
   global repo already does).

Deleting a project removes the registry entry (and its token); the on-disk tree is
left in place — deleting cached bytes is a separate, explicit operator step.

On the pkgcache side a supervisor in `__main__.py` re-reads the registry every few
seconds and calls `RoleServer.reconcile()` for each role: new names get a sub-app
(its own `Core` — storage/ledger/cache_root — and background flush task) built and
mounted live; removed names have their sub-app dropped and core closed. No restart,
no rebind, no container recreate.

## Implementation status

Backend implemented and unit/integration-tested
(`webui/test_projects.py`, `webui/test_multiproject.py`, `pkgcache/tests/test_router.py`):

- **Registry** — `webui/projects.py`: name-only CRUD, name grammar + reserved names,
  `role_prefix(project, role)`, files write tokens; legacy port-carrying registries
  read without error.
- **Routing** — `pkgcache/router.py`: a `RoleServer` per role port with the
  path / oci / apt selection strategies above; `external_base()` honors the
  stripped `root_path`; `handlers/oci.py` re-prefixes the `tags/list` name.
- **Serving** — `pkgcache/core/config.py` `load_roles()` returns one `Config` per
  `(role, project)` all sharing the role's default port; `Config.project`;
  `/healthz` reports the project.
- **Live add/drop** — `pkgcache/__main__.py` supervisor polls the registry and
  reconciles each role's project sub-apps without a process restart; six long-lived
  role servers own their projects' core lifecycles.
- **Per-project VC + shuttle** — `webui/ops.py` (checkpoint/export/import/rollback)
  threaded by `project`; per-project export/import dirs; a name-only `project.json`
  travels in a named project's shuttle so import re-registers it;
  `gen_manifest.py` honors `PKGCACHE_MANIFEST_ROOT`; `scripts/pkgops.py --project`.
- **API + scoped reads** — `GET/POST /api/projects`, `DELETE /api/projects/<name>`;
  `?project=` on `/api/manifests`, `/api/history`, `/api/endpoints`, `/api/shuttle`,
  `/api/packages`; `config.endpoints/progress_sources/health_sources(project)` build
  the prefixed URLs.
- **Live progress aggregation** — `webui/live.py` polls every project's
  prefixed progress endpoints (bounded thread pool, project list refreshed each
  cycle); health is per-server (projects share a process per role) so it's polled
  once. `/api/proxies`, `/api/downloads`, `/api/recent` are `?project=`-scoped.
- **React console** — a top-bar project switcher (select + create + delete),
  `pcc_project` persisted in localStorage, all panels re-poll the selected project.
  `api.ts` carries the `project` query param (global omits it). Node→nginx image,
  no new deps.

## Edge cases & decisions to honor

- **Stable, self-describing URLs:** the URL *is* the project name in the path, so it
  never drifts and needs no allocation/boot recomputation.
- **No shadowing:** path roles require BOTH the project segment and the role segment
  to match, so real single-segment package names can't be captured; reserved names
  keep the oci/apt first segment unambiguous. Residual operator-created edges (a
  global `files` path literally shaped `<project>/files/…`, or a pypi index named
  `<project>/pypi`) are documented, not engineered around.
- **apt username is a label, not auth:** it selects the cache tree; no password is
  checked (same open-proxy trust model as before — trusted networks only).
- **Isolation over sharing:** separate trees + repos mean no cross-project dedup. A
  shared DVC remote could be layered later without changing the topology.
- **TLS:** the in-process HTTPS roles reuse the same server cert; since every project
  shares the role ports, one cert (and one Docker `certs.d/<host>:5000` entry) covers
  all projects. The cert's SANs must still cover the host clients reach it by (ports
  and prefixes don't affect SANs).

## Caveats

- **First checkpoint of an empty project:** `dvc add` over empty cache dirs may
  warn/skip; checkpoint after at least one artifact has been cached.
- **Live project drop:** removing a project closes its cores immediately; an in-flight
  request to a just-deleted project may tear (deletion is a deliberate operator step).

## Offline import dedup — the shared DVC object store

On the air-gapped host every project's cache repo shares **one** DVC object store,
`caches/.dvc-shared`, so a package imported for one project is not copied again for
the next: one physical copy per unique file, however many projects reference it.

- **Where it is wired:** the import path only (`webui/ops.py` `apply()` →
  `_use_shared_dvc_cache`). It writes `cache.dir` + `cache.type = reflink,hardlink,copy`
  to each repo's `.dvc/config.local` (which DVC git-ignores), so it never collides
  with the `.dvc/config` that arrives in the shuttle bundle and the offline repo
  stays a pure fast-forward mirror. `reflink→hardlink→copy` means dedup where the
  filesystem supports it, correct import (just no dedup) where it does not.
- **Same-filesystem requirement:** the store lives under `caches/` (the one mounted
  volume) so links to every repo resolve on one filesystem.
- **`ledger.db` is detached after checkout** (`_unshare_ledgers`): it lives inside a
  DVC-tracked role dir, so a hardlink checkout would link it into the shared store,
  and the always-on proxy (its single writer) folds the WAL into it in place. We copy
  it to a private inode so a proxy write can never corrupt the shared object.
- **Never run bare `dvc gc`** against a repo using the shared store — it would prune
  objects other projects reference. Don't gc offline (consistent with "deleting bytes
  is an explicit step"), or gc with `--projects <every repo>`.
- **Opt out** with `PKGCACHE_SHARED_DVC_CACHE=0` to fall back to per-repo import.
- The online side is unchanged: it keeps its default per-repo cache, so
  checkpoints/exports remain byte-for-byte identical. This deliberately does not
  dedup the shuttle *media* — project B's transfer still carries bytes the offline
  host may already hold; dedup happens on arrival.

## Out of scope

- Hostname/vhost URL models (we chose path/name prefixes on shared ports).
- **Online** cross-project dedup at the live-cache / checkpoint layer (planned as a
  shared content-addressed store; the offline import dedup above is shipped).
- Per-project auth (the console remains trusted-network only, as today).
