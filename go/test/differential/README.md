# Differential cutover gate

Run the retired Python stack and `pkgreg` against independent copies of the same
warm cache. Keep both instances hard-offline so a missing artifact fails instead of
silently reaching the internet.

The retired deployment has three public surfaces, while a single-port Go deployment
may use one address for all three:

- `-python` / `-go`: TLS package protocols and Git;
- `-python-proxy` / `-go-proxy`: plain HTTP apt/apk forward proxy;
- `-python-admin` / `-go-admin`: legacy `/api/*`;
- `-python-ca` / `-go-ca`: CA trusted for HTTPS and Git on that side.

Proxy and admin addresses default to the corresponding package origin, so the short
two-address form remains valid for synthetic or split-port deployments that do not
need overrides.

```sh
go test ./test/differential -run TestProductionCorpus -count=1 -v -args \
  -python https://127.0.0.1:18443 \
  -python-proxy http://127.0.0.1:13142 \
  -python-admin http://127.0.0.1:18088 \
  -python-ca /path/to/python-ca.crt \
  -go https://127.0.0.1:28443 \
  -go-proxy http://127.0.0.1:23142 \
  -go-admin https://127.0.0.1:28443 \
  -go-ca /path/to/go-ca.crt \
  -var PROJECT=gamma \
  -var PYPI_WHEEL_PATH=/global/pypi/root/pypi/+f/numpy/example.whl \
  -var PYPI_METADATA_PATH=/global/pypi/root/pypi/+f/numpy/example.whl.metadata \
  -var NPM_TARBALL_PATH=/global/npm/chalk/-/chalk-4.1.2.tgz \
  -var NPM_SCOPED_PATH=/global/npm/%40types%2Fnode \
  -var OCI_IMAGE=library/alpine \
  -var OCI_TAG=3.20 \
  -var OCI_CHILD_DIGEST=sha256:... \
  -var OCI_BLOB_DIGEST=sha256:... \
  -var APT_INRELEASE_URL=http://archive.ubuntu.com/ubuntu/dists/noble/InRelease \
  -var APT_INRELEASE_ETAG='"..."' \
  -var APT_DEB_URL=http://archive.ubuntu.com/ubuntu/pool/...deb \
  -var APK_URL=http://dl-cdn.alpinelinux.org/alpine/...apk \
  -var GIT_REPO=github.com/octocat/Hello-World
```

Use copy-on-write snapshots when available. Never mount one writable cache tree into
both implementations: metadata refreshes, rolling activity, files uploads, and
SQLite sidecars would make the comparison self-interfering. The checked-in Python
Compose override gives every writable path an isolated mount and forces `OFFLINE=1`.

## Comparison contract

The run stops at the first difference. Status, cached byte bodies, generated HTML,
semantic JSON, and protocol headers remain strict. The harness excludes only
implementation-independent transport/security/framing fields (`Date`, `Server`,
`Connection`, `Content-Length`, the outer security policy), opaque HTTP validators,
and additive cache hints. Range and conditional behavior are exercised explicitly.

Deployment-specific JSON keys can be excluded per case with
`ignore_json_keys`. The production corpus uses that only for values that cannot be
equal across two independent processes: listener ports/storage roots and rolling or
historical telemetry. Stable fields and endpoint notes remain compared.

The files mutation cases assert the hard-offline read-only contract. Full, shallow,
and `--filter=blob:none` clones must all succeed and produce identical checked-out
trees; `.git` implementation metadata such as remote URLs, reflogs, indexes, and pack
layout is intentionally not hashed. Git `info/refs` is compared separately.

The 2026-07-28 qualification used independent XFS reflink snapshots of the repository
cache, the retired TLS/proxy/admin topology, and a single-port Go deployment. It
passed all 46 cases.
