# Generic artifacts (the `files` ecosystem)

The `files` role (port `3144`, HTTPS) is a **generic artifact store** — a home for
things with no package protocol: build outputs, datasets, installers, tarballs.
It's the system's only **write** path.

- **Download**: plain `wget`/`curl`, anonymous, Range/resume supported.
- **Upload**: `PUT` with a per-project **write token**, or drag-drop in the console.
- **Write-once** by default; **online-only** writes (the air-gapped side is read-only).

## URL scheme

Path-addressed under the role, one repository per project:

```
https://<host>:3144/<path>                         # global project
https://<host>:3144/<project>/files/<path>          # a named project (prefix on the shared port)
```

`<path>` is any nested path, e.g. `builds/v1.2/app.tar.gz`.

## Download (anonymous)

```bash
wget --ca-certificate=certs/ca.crt https://HOST:3144/builds/v1.2/app.tar.gz
curl --cacert certs/ca.crt -O https://HOST:3144/builds/v1.2/app.tar.gz
wget -c  --ca-certificate=certs/ca.crt https://HOST:3144/big.iso          # resume (HTTP 206)
wget -r -np --ca-certificate=certs/ca.crt https://HOST:3144/builds/       # mirror a folder
```

A directory URL returns an HTML index (browsable; `wget -r` walks it). If
`certs/ca.crt` is installed in the system trust store, drop `--ca-certificate`.

## Upload (write token required)

Generate a token once in the **console** (Packages page → *Artifacts*), or via the
API. Then `PUT` the raw bytes:

```bash
export TOKEN=…                 # from the console; shown once
curl --cacert certs/ca.crt -T app.tar.gz \
     -H "Authorization: Bearer $TOKEN" \
     https://HOST:3144/builds/v1.2/app.tar.gz
```

- **Write-once**: re-`PUT`ting an existing path → `409`. Add `?overwrite=1` to replace.
- **Checksum (optional)**: send `-H "X-Checksum-Sha256: <hex>"`; the server verifies
  it before committing (`400` on mismatch). The response always returns the computed
  `sha256`.
- **Size cap (optional)**: set `files.max_upload_mb` in `pkgcache/pkgcache.yaml`
  (`0` = unlimited) → oversize uploads get `413`.
- Response: `201` (new) / `200` (overwrite) with
  `{"path", "size", "sha256", "url"}`.

```bash
# delete (token required):
curl --cacert certs/ca.crt -X DELETE -H "Authorization: Bearer $TOKEN" \
     https://HOST:3144/builds/v1.2/app.tar.gz        # → 204
```

`X-Auth-Token: <token>` is accepted as an alternative to the `Authorization: Bearer`
header.

## Write tokens

- **Per project**, generated/rotated from the console or `POST /api/token`
  `{"project": "<name>"}` — returned **once** (there is no retrieve-later; rotate to
  get a new one, which immediately invalidates the old).
- Stored in `config/projects.json` under `"tokens"` (host-local, gitignored). The
  `files` role verifies against that file directly (mtime-cached), so a rotation
  applies within seconds — no restart.
- No token set → all writes are refused (`403`). Reads never need a token.

## Air-gap flow (online-only writes)

Writes are refused when `OFFLINE=1` (`403` "read-only — air-gapped side"). The
intended flow mirrors every other ecosystem:

1. **Online**: upload artifacts (`PUT` / console).
2. `pkgops.py checkpoint` — versions `caches/files/` into the project's git+DVC repo
   (auto-included; the ledger records each artifact with its sha256 + size).
3. `pkgops.py export` → copy `shuttle/out/` across → `pkgops.py import` on the far side.
4. **Offline**: `wget` serves the artifacts from cache; a miss simply 404s.

This one-directionality is deliberate: an upload on the offline side would be wiped
by the next import's `dvc checkout`, or block its bundle fast-forward.

## Notes

- **Reserved paths** are refused for write/delete: the role's `ledger.db*`, any
  `*.part` temp, and paths whose first segment starts with `+` (the endpoint
  namespace, e.g. `/+progress`). Path traversal (`..`) is rejected.
- **Integrity**: sha256 is computed inline during upload and stored in the ledger;
  the atomic temp→rename means a checkpoint never captures a half-written file.
- **Stats**: downloads count toward the Statistics tab (leaderboard, hit rate);
  since there's no upstream, bandwidth / "time saved" don't accrue for this eco.
- **No quotas / GC** (consistent with the rest of the system) — the console's
  storage monitor is the guard.
- **Token file permissions**: `config/projects.json` holds the tokens in cleartext;
  it's host-local and gitignored. Acceptable for this trust model (no-auth console,
  trusted network); restrict the file's mode if your host is shared.
