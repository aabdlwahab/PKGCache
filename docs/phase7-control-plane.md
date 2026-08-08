# Phase 7 — control plane

Status: **complete** (2026-07-27).

Phase 7 moves tenant, identity and operator state into the Go process. `control.db`
is the durable source of truth; successful mutations publish one immutable config
snapshot, so routing, offline mode, upstreams, quotas and data-plane auth change on
the next request.

## Delivered

| Task | Evidence |
|---|---|
| P7-01 | Forward-only, idempotent migrations for projects, flags, users, tokens, upstreams, sealed credentials, jobs/logs and audit |
| P7-02 | Exact Python-compatible scrypt parameters; unchanged `users.json` hashes import and authenticate; user/admin/superuser reporting and ownership policy |
| P7-03 | Process-local opaque sessions with monotonic expiry and per-IP lockout on the fifth failure; restart revokes sessions |
| P7-04 | Hash-only bearer secrets, expiry/revoke/scopes/`last_used`; files writes are constrained to one project/ecosystem/scope |
| P7-05 | Project CRUD, owner, offline, quota and data-plane-auth settings with atomic live publication |
| P7-06 | Live per-project upstream add/partial-edit/remove; private origins receive credentials decrypted only in memory |
| P7-07 | View/operate/create/superuser guards, anonymous safe-read mode, trusted-proxy IP handling and same-origin mutation checks |
| P7-08 | Persisted per-project queues, bounded cross-project pool, durable logs and queued/running cancellation |
| P7-09 | The complete `/api/v1` route table with uniform `{error,code}` failures |
| P7-10 | Legacy `/api/*` project, cache-view, endpoint, job, token and auth shapes used by the existing React console |
| P7-11 | Non-blocking SSE for progress/job/health/audit; fetch events carry project and chunk progress |
| P7-12 | Immutable audit rows for every API mutation and `pkgreg audit` text/JSON output |

Snapshot, rollback, export and import routes enqueue durable jobs now. Their operation
runners intentionally arrive in Phase 8; an unsupported action fails visibly and
durably rather than pretending it ran.

## Credential sealing

The architecture draft named NaCl secretbox. This implementation uses AES-256-GCM
from Go's standard library, with a random nonce per record and a host-local 0600
32-byte key. It provides authenticated encryption without adding a module that an
air-gapped build would need to fetch. Plaintext is present only in the immutable
in-memory control projection and outbound request object, and is never serialized by
the API or logger.

## Acceptance coverage

- migration rerun and restart persistence;
- a fixed Python `hashlib.scrypt` vector and unchanged legacy hash import;
- the account/report/owner policy and five-failure throttle;
- token project/ecosystem/scope isolation, revoke and `last_used`;
- project routing on the immediately following request;
- a real authenticated request to a newly-added private PyPI origin without restart;
- encrypted credential bytes and host-key permissions;
- project-parallel/project-serial job execution, persisted logs and cancel;
- legacy console response shapes, uniform errors and cross-origin rejection;
- SSE progress delivery in under 100 ms;
- audit persistence for mutations.

Verification:

```text
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./...
go mod verify
```
