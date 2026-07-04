# Control-UI API reference

The webui backend exposes a small JSON HTTP API that the React console (and
`scripts/pkgops.py`, in-process) drive. It is **standard-library only** — there is
no framework and no generated OpenAPI schema, so this document is the contract.
Keep it in sync with [webui/app/api/routes.py](../webui/app/api/routes.py) (the
declarative route table) and [webui/console/src/lib/types.ts](../webui/console/src/lib/types.ts)
(the client-side shapes).

The console reaches the API through the `console` nginx container, which
reverse-proxies `/api` and `/healthz` to `webui:8088`. All paths below are relative.

## Conventions

**Project scoping.** Cache views are per-project. Scope a request with the
`?project=<name>` query param (POST bodies use a `project` field). Omitting it — or
passing `global` — targets the implicit global project, whose URLs are unprefixed.
A named project must already exist or the request is a 400.

**Responses** are JSON unless noted (`/api/lockfile` streams a file). Success is
200 except where stated (`201` on project create).

**Errors** are uniform: `{"error": "<message>"}` with a status that reflects the
cause — the backend raises a typed `ApiError` and the dispatcher renders it:

| Status | Meaning | Example |
|---|---|---|
| 400 | bad request — invalid input, unknown project, malformed JSON body | `{"error":"no such project: ghost"}` |
| 404 | no such job / no lockfile produced yet | `{"error":"no such job"}` |
| 409 | files write token not set | `{"error":"no write token set — generate one first"}` |
| 502 | the files role on `pkgcache` is unreachable | `{"error":"files role unreachable: …"}` |
| 500 | an unexpected server bug (never a validation failure) | — |

A path/method with no matching route is a 404 (no body). Anything that is *not* an
`ApiError` propagates to a 500, so a genuine bug is never disguised as a client 400.

## Read endpoints (GET)

| Path | Query | Response |
|---|---|---|
| `/healthz` | — | `{"status":"ok"}` |
| `/` | — | `{"service","console"}` — a pointer to the console (no HTML served) |
| `/api/projects` | — | `{"projects":[{name, ports, repo, default}]}` (global first) |
| `/api/proxies` | `project` | container status + live per-role health: `{available, profile, services:[{name,state,status}], project, roles:[{role,up,offline}], up, offline}` |
| `/api/downloads` | `project` | `{project, sources:{eco:[download]}, age}` — in-flight downloads |
| `/api/recent` | `project` | `{project, pulls:[{eco,name,id,size,hit,failed,time}]}` |
| `/api/manifests` | `project` | `{project, ecosystems:{eco:[artifact]}, checkpointed:{eco:int}, usage, age}` |
| `/api/stats` | `project` | aggregate stats — `{project, totals, hit_rate, bytes_saved, time_saved_seconds, by_eco, by_arch, leaderboard, top_largest, recent_added, bandwidth, usage}` |
| `/api/history` | `project` | `{head, commits:[{hash,short,date,subject,is_checkpoint,is_head}]}` |
| `/api/endpoints` | `project` | `{eco: "<client pull hint>"}` per ecosystem |
| `/api/shuttle` | `project` | `{project, export_dir, import_dir, import_ready, import_checkpoints:[…]}` |
| `/api/packages` | `project, eco, q, sort, page` | `{project, ecosystems:{eco:[artifact]}, page, sort}` — server-side filter/sort/paginate |
| `/api/token` | `project` | `{"set": bool}` — whether a files write token exists (never the token) |
| `/api/jobs` | — | `{busy, jobs:[{id,action,status}]}` |
| `/api/jobs/{id}` | `offset` | `{id, action, status, log, offset}`; `log` is the slice from `offset`, `offset` the new total. 404 if unknown. |
| `/api/lockfile` | `project` | the rewritten `uv.lock` as a file download; 404 if no lockwarm has produced one |

`artifact` = `{name, version, digest, size, cached_at}` (+ `origin, arch` in the
`full` packages view). `usage` = `{disk:{subdir:bytes}, disk_total, docker_deduped,
fs:{total,used,free}}`.

## Mutations (POST / DELETE)

| Method · Path | Query / Body | Response |
|---|---|---|
| `POST /api/projects` | body `{name}` | `201 {name, ports, repo}`; 400 on bad/duplicate/reserved name |
| `DELETE /api/projects/{name}` | — | `{name, repo}` — drops the registry entry; cached bytes stay on disk |
| `POST /api/token` | body `{project?}` | `{token}` — generated/rotated, **returned once** |
| `POST /api/jobs` | body `{action, project?, …}` | `{id}` — starts a background job (see actions below); 400 on bad input or if one is already running |
| `POST /api/artifacts` | `?project&path&overwrite`, raw body = file bytes | streamed to the files role with the write token injected; relays its `{path,size,sha256,url}`. 409 if no token, 502 if unreachable |
| `DELETE /api/artifacts` | `?project&path` | relays the files role's delete (204 on success) |

### Job actions (`POST /api/jobs`)

One job runs at a time. The job's streamed log is polled via `GET /api/jobs/{id}`.
`project` is optional (default global) except where noted.

| `action` | Body fields | Notes |
|---|---|---|
| `checkpoint` | `message` (required) | hash → commit the cache, live (no downtime) |
| `export` | `base?`, `target?` | both omitted → full export; both set (valid checkpoint hashes) → delta |
| `import` | — | applies the shuttle in `shuttle/in`; may register a brand-new project |
| `rollback` | `commit` (hash) | restore the cache to a checkpoint |
| `lockwarm` | `lock` (uv.lock text), `host` (bare hostname/IP) | warm the cache from a uv.lock, then produce a rewritten lock (download via `/api/lockfile`) |
| `mode` | `target` ∈ `online`\|`offline` | instance-wide (no project) — recreates the pkgcache container under that profile |

## Notes for maintainers

- The route table, controllers, and dispatch live in
  [webui/app/api/routes.py](../webui/app/api/routes.py); the server plumbing (send
  helpers, service wiring) in
  [webui/app/api/handler.py](../webui/app/api/handler.py).
- Ledger-backed reads (`/api/manifests`, `/api/packages`, `/api/stats`) fetch
  pkgcache's `GET /+ledger/artifacts` and `/+ledger/stats` per (project, role)
  through the `pkgcache` gateway and combine them in the reads service — the webui
  no longer opens `ledger.db` directly. If pkgcache is unreachable, a short last-good
  cache serves the previous values (then empty), so a role blip doesn't blank a panel.
