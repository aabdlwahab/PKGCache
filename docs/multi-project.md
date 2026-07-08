# Multi-project caches on one central instance

## Goal

Let the **single** central pkgcache instance serve multiple isolated *projects*,
each with:

- its **own set of URLs** (a per-project prefix on the two shared ports),
- its **own cache content** (separate on-disk trees),
- its **own version control** (a dedicated git + DVC repo) — checkpoint, rollback
  and history controlled independently from the web UI,
- its **own shuttle** (export/import carries only that project's blobs).

The **global** cache is simply the reserved project named `global` — same URL
shapes as any other project, same repo at `caches/`.

## Locked decisions

1. **One process, TWO ports.** Everything HTTPS lives on the ONE unified port
   (default `8443`; see `pkgcache/unified.py`); the apt/apk forward proxy keeps its
   own plain-HTTP port (`3142`) because proxy clients (busybox wget, apt < 1.6)
   can't speak to a TLS proxy. No process, container, or socket per project.
2. **Project rides a URL prefix, not a port.** How the project is carried is dictated
   by each protocol (see *Routing* below): a fully qualified path prefix where the
   protocol allows it, the image name for Docker, the proxy username for apt.
3. **One git + DVC repo per project.** Separate cache trees per project — this is
   what makes a per-project shuttle fall out for free.
4. **The registry is just names.** Creating a project is a name written to
   `config/projects.json`; there is nothing to allocate, probe, or persist beyond the
   name (+ its files write token). A supervisor picks it up on its next poll.
5. **Global is a reserved name, not a special case.** It is never stored in the
   registry, but on the wire it is addressed exactly like any project
   (`/global/<role>/…`); only docker (no image-name prefix) and apt (no proxy
   username) treat it as the unprefixed default.

## Routing — how the project is carried per protocol

Docker can't be given a base path (the registry API is pinned to `/v2/` at the
listener root), and apt is a *forward proxy* (the client sends absolute upstream
URLs, so there's no path to prefix). The other four take a clean path prefix — and
because every one of their URLs starts with `/<project>/<role>/`, the five HTTPS
roles coexist on one port with no ambiguity:

| Role | Global | Named project `<p>` | Mechanism |
|---|---|---|---|
| npm / pypi / git / files | `:8443/global/<role>/…` | `:8443/<p>/<role>/…` | leading path segments |
| oci | `:8443/dockerhub/…` (image name) | `:8443/<p>/dockerhub/…` | first segment of the **image name** under `/v2` |
| apt / apk | `http://HOST:3142` | `http://<p>@HOST:3142` | **proxy username** (password ignored) |

A `UnifiedServer` (`pkgcache/unified.py`) owns the HTTPS port: `/v2/…` goes to the
oci RoleServer, `/<project>/<role>/…` to that role's RoleServer, and anything else
is a helpful 404 (unknown projects are named in the error). Each `RoleServer`
(`pkgcache/router.py`) then selects the project, strips the marker, and hands the
request to that project's sub-app:

- **path roles** — if segment 1 is a registered project *and* segment 2 is the role
  name, strip `/<p>/<role>` and set `scope["root_path"]` to it (so `external_base()`
  re-emits project-scoped links in rewritten pypi indexes, npm packuments, git LFS
  and files URLs).
- **oci** — for `/v2/<seg>/…`, if `<seg>` is a registered project, rewrite the path
  to `/v2/…` and stash the project so `tags/list` can re-prefix the echoed `name`.
  Response bodies are otherwise content-addressed by digest, so nothing else needs
  rewriting. `/v2/` (ping) stays global. The uniform `/<p>/oci/…` path form is also
  accepted so internal admin URLs (healthz, +ledger, progress) look the same for
  every role.
- **apt** — read the project from the `Proxy-Authorization` username. Internal
  progress/health pollers, which can't set a proxy user, use the uniform
  `/<p>/apt/…` path form (a real proxied request arrives with an absolute-URL
  target, so the two never collide).

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
  "tokens": { "projA": "<files write token>", "global": "<token>" },
  "offline": { "projA": true },
  "owners": { "projA": "alice" }
}
```

`offline` holds per-project **soft** offline flags (global included; absent =
online, so old registries need no migration). The flag is stored sparsely — going
back online deletes the entry rather than writing `false`.

`owners` maps a project to the username that owns it (an admin or superuser).
An **absent** owner means superuser-owned — which is how `global` and every project
created before auth existed read, so ownership needs no migration either. Only the
webui's authorization layer reads this map; pkgcache ignores it. See the auth section
below for what ownership gates.

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
caches/.dvc-shared/              # one DVC object store shared by ALL repos so an
                                 #   artifact several projects hold is stored once,
                                 #   not once per project — used on both the online
                                 #   checkpoint and the offline import (see below)
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
mounted live; removed names have their sub-app dropped and core closed; a changed
config (the offline flag) is live-swapped onto the running core without rebuilding
the sub-app, so in-flight downloads for that project survive the flip. No restart,
no rebind, no container recreate.

### Per-project offline (soft mode)

Each project — global included — can be taken offline on its own via the console's
top-bar toggle (or `POST /api/projects/<name>/mode` with `{"target": "offline"}`).
That writes the registry's `offline` flag; the supervisor applies it on its next
poll (≤5s), and that one project serves cache-only (misses fail, files-role PUTs
are rejected) while every other project keeps fetching. `/healthz` per (project,
role) reports the live state, which is what the console indicator and the lockwarm
guard read.

This soft flag is distinct from the instance-wide **hard** mode: `OFFLINE=1` on the
cache container (the air-gap deployment / the console's "Instance mode" action,
which recreates the container). The hard mode always wins — while it is on, every
project is offline regardless of soft flags, and the console locks the per-project
toggle. Switching the instance back online leaves soft flags intact: a project
flagged offline stays offline.

## Auth & ownership

Accounts and per-project ownership live in the webui only (pkgcache stays open on
its own ports — gating the data plane is out of scope; see below). Three roles:

- **superuser** — creates any account; promotes/demotes; reassigns who a user reports
  to and which admin a project belongs to; may operate any project. The break-glass
  superuser comes from `UI_ROOT_USER`/`UI_ROOT_PASSWORD` (verified from the env, never
  stored, can't be demoted or deleted).
- **admin** — creates `user` accounts that report to them; owns projects; operates the
  projects they own.
- **user** — reports to one admin; may view/consume that admin's projects and their
  own password; no account or project management.

Stored accounts live in `config/users.json` (scrypt-hashed, webui-managed); a session
is an opaque HttpOnly cookie the webui maps to a username in memory.

**Enforcement is opt-in.** It activates only once auth is configured — a root
superuser is set, or the store already holds accounts. Until then every route stays
open exactly as before (an un-migrated deployment keeps working). Setting
`UI_ROOT_USER`/`UI_ROOT_PASSWORD` is what turns it on.

Per route, with enforcement on:

| Action | Who |
|---|---|
| View / consume a project (dashboards, endpoints, packages, lockfile download, artifact upload) | owner, the owner's reports, or a superuser |
| Operate a project (checkpoint, rollback, export/import, per-project offline, rotate token, delete artifact, delete project) | owner or superuser |
| Create a project (becomes its owner) | admin or superuser |
| Reassign a project's owner (`POST /api/projects/<name>/owner`) | superuser |
| Instance-wide mode recreate (`action=mode` job) | superuser |
| Account management (`/api/users`) | superuser (any account); admin (own users only) |

An absent owner (global, pre-auth projects) reads as superuser-owned, so only a
superuser sees or touches them until one is explicitly assigned. `GET /api/projects`
is filtered to what the caller may view; `GET /api/me` reports `auth_enabled` so the
console knows whether to show a login screen.

## Implementation status

Backend implemented and unit/integration-tested
(`webui/tests/test_projects.py`, `webui/tests/test_multiproject.py`,
`pkgcache/tests/test_router.py`, `pkgcache/tests/test_unified.py`):

- **Registry** — `webui/app/services/projects.py`: name-only CRUD, name grammar + reserved names,
  the uniform `role_prefix(project, role)`, files write tokens; legacy port-carrying
  registries read without error.
- **Routing** — `pkgcache/unified.py`: the one HTTPS listener dispatching to the
  per-role `RoleServer`s (`pkgcache/router.py`) with the path / oci / apt selection
  strategies above; `external_base()` honors the stripped `root_path`;
  `handlers/oci.py` re-prefixes the `tags/list` name.
- **Serving** — `pkgcache/core/config.py` `load_roles()` returns one `Config` per
  `(role, project)`; `Config.project`; `/healthz` reports the project.
- **Live add/drop** — `pkgcache/__main__.py` supervisor polls the registry and
  reconciles each role's project sub-apps without a process restart; six long-lived
  role servers (five behind the unified port + apt) own their projects' core
  lifecycles.
- **Per-project VC + shuttle** — `webui/app/services/operations.py` (checkpoint/export/import/rollback)
  threaded by `project`; per-project export/import dirs; a name-only `project.json`
  travels in a named project's shuttle so import re-registers it;
  `gen_manifest.py` honors `PKGCACHE_MANIFEST_ROOT`; `scripts/pkgops.py --project`.
- **API + scoped reads** — `GET/POST /api/projects`, `DELETE /api/projects/<name>`;
  `?project=` on `/api/manifests`, `/api/history`, `/api/endpoints`, `/api/shuttle`,
  `/api/packages`; `config.endpoints/progress_sources/health_sources(project)` build
  the prefixed URLs.
- **Live progress aggregation** — `webui/app/services/livefeed.py` polls every project's
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
  shares the unified port, one cert (and one Docker `certs.d/<host>:8443` entry) covers
  all projects. The cert's SANs must still cover the host clients reach it by (ports
  and prefixes don't affect SANs).

## Caveats

- **First checkpoint of an empty project:** `dvc add` over empty cache dirs may
  warn/skip; checkpoint after at least one artifact has been cached.
- **Live project drop:** removing a project closes its cores immediately; an in-flight
  request to a just-deleted project may tear (deletion is a deliberate operator step).

## Cross-project dedup — the shared DVC object store

Every project's cache repo shares **one** DVC object store, `caches/.dvc-shared`, so
an artifact several projects hold is stored once, not once per project — one physical
copy per unique file however many projects reference it. It is used on **both** sides:

- **Online (`checkpoint`):** before `dvc add`, each repo is pointed at the shared
  store with `cache.type = reflink,copy`. `dvc add` stores the object once (a second
  project checkpointing the same file finds it already present and just materializes
  it) and reflinks it into the workspace. On a reflink (CoW) filesystem this is
  block-level dedup across all projects' live trees *and* their DVC caches.
- **Offline (`apply`):** the same store is configured before `dvc pull`, with
  `cache.type = reflink,hardlink,copy`. Importing project B skips every object project
  A already brought in, and B's workspace files link to the same objects.

Both write to `.dvc/config.local` (which DVC git-ignores) rather than the tracked
`.dvc/config`, so the setting never collides with the config that travels in the
shuttle bundle and the offline repo stays a pure fast-forward mirror. All in
`webui/app/services/operations.py` → `_use_shared_dvc_cache`.

- **`reflink,copy` online, never hardlink:** the live proxy rewrites `ledger.db` in
  place. reflink is copy-on-write (a proxy write forks new blocks, leaving the shared
  object intact) and copy is trivially private; a *hardlink* would corrupt the shared
  object, so it is excluded online. Offline allows hardlink because the proxy there is
  near-idle and `_unshare_ledgers` gives each `ledger.db` a private inode right after
  checkout as a safety net.
- **Same-filesystem requirement:** the store lives under `caches/` (the one mounted
  volume) so links to every repo resolve on one filesystem. Off a link-capable fs the
  `copy` fallback keeps both paths correct, just without dedup.
- **Git-ignored inside the global repo:** the store sits under `caches/`, which *is*
  the global repo, so `_ignore_shared_store` adds `/.dvc-shared/` to its `.gitignore`
  and the checkpoint's `git add -A` never stages the raw object bytes. (Named-project
  repos live under `caches/projects/<name>/`, so the store is outside them.)
- **Never run bare `dvc gc`** against a repo using the shared store — it would prune
  objects other projects reference. Don't gc (consistent with "deleting bytes is an
  explicit step"), or gc with `--projects <every repo>`.
- **Opt out** with `PKGCACHE_SHARED_DVC_CACHE=0` to fall back to per-repo behaviour on
  both sides.

**Scope of this dedup:** it covers *checkpointed* state — the durable bytes, which is
where the cost lives. The live window (between a fetch and the next checkpoint) and the
duplicate *download* are handled by the CAS below; the shuttle *media* is not (project
B's transfer still carries bytes the offline host may already hold — dedup happens on
arrival, which is fine when projects are shuttled one at a time).

## Live/download dedup — the sha256 content store (CAS)

The proxy keeps **one** sha256 content-addressed store for the whole instance,
`caches/.cas/sha256/<aa>/<hex>`, shared by every project *and* ecosystem. It closes
the window the checkpoint dedup leaves open: an artifact one project fetched is
neither re-downloaded nor re-stored for the next, live, without waiting for a
checkpoint. All in `pkgcache` (`core/storage.py` + `core/cache.py`); the hot path is
otherwise unchanged.

- **Populate on commit:** every committed download (`inflight.py`) and files-role
  upload (`handlers/files.py`) is hardlinked into the CAS by its sha256
  (`Storage.cas_link_from`). Best-effort — a CAS hiccup never fails the download.
- **Serve on miss (download avoidance):** when the sha256 is known *before* the
  download — pypi index hashes and OCI blob digests — `Cache.fetch` checks the CAS
  first (`cas_materialize`); a hit is hardlinked into this project's tree and served
  as a saved-bytes hit, with no upstream request. npm/apt don't advertise a sha256
  up front, so they still download on a first cross-project fetch, but their commits
  populate the CAS.
- **Hardlinks are safe here** because cached artifacts are immutable: nothing rewrites
  a committed file in place (an `?overwrite=1` files PUT renames a fresh inode over the
  path, leaving the shared inode untouched). So — unlike the DVC store's `ledger.db` —
  no unshare step is needed.
- **Same-filesystem requirement:** the CAS lives under `caches/`, so its hardlinks to
  every project tree resolve on one filesystem. If it somehow lands on a different
  device it is disabled at startup (a copy fallback would defeat the dedup).
- **Git-ignored inside the global repo** (`_ignore_path_in_repo` in `checkpoint`), same
  reasoning as the DVC store.
- **GC:** a CAS entry is unreferenced when its link count drops to 1 (no project tree
  points at it). Reclaiming those is a future explicit maintenance op, consistent with
  "deleting bytes is an operator step"; nothing prunes automatically today.
- **Opt out** with `PKGCACHE_CAS=0`.

**Known gap:** two projects fetching the *same brand-new* artifact at the *same time*
still download twice — each project's `Core` has its own single-flight registry, and
neither has populated the CAS yet. The window is small and the only cost is one
redundant download; a shared digest-keyed single-flight would close it later.

## Out of scope

- Hostname/vhost URL models (we chose path/name prefixes on shared ports).
- Concurrent first-fetch dedup across projects (a shared digest-keyed single-flight);
  see the CAS "known gap" above.
- Automatic CAS garbage collection (unreferenced entries are reclaimed by an explicit
  maintenance op, not on their own).
- **Data-plane auth.** Control-plane accounts + ownership (above) gate the *console
  API* only. The pkgcache pull ports stay open: anyone on the network can still
  `pip install` / `docker pull` / fetch files from any project's cache. Gating those
  would mean per-project pull credentials inside pkgcache (npm/pip basic-auth, OCI
  token auth, client config on the air-gapped side) — a separate, larger effort. The
  files-role **write** token is unchanged (a machine credential; rotating it is now an
  owner-only action).
