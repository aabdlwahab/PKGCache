# Six-repository Docker test

This example builds one reusable client image and tests every `pkgcache` role with
the real ecosystem clients. Assertions run at container **runtime**, so the same
image can be re-run without rebuilding, keeps the cache address / CA / write token
out of image layers, and can be pointed at either side of the air gap.

> **See also [`examples/Dockerfile`](../Dockerfile)** — the same six roles tested
> the other way round: there the *build* is the test (`docker build` fails on the
> first failed assertion), which additionally proves that a build can complete with
> nothing but the cache reachable. Use this one to re-test an existing deployment;
> use that one to validate that air-gapped CI can build at all.

| Role | What is exercised |
|---|---|
| OCI | A complete `skopeo copy` of an image, twice, including manifests and blobs |
| PyPI | Two clean-cache `pip download` runs through the PEP 503/691 index |
| npm | Two clean-cache `npm pack` runs through rewritten packuments and tarballs |
| apt + apk | Two isolated apt downloads and two Alpine `apk fetch` runs through the shared forward proxy |
| Git | Two smart-HTTP shallow clones, object checks, and an explicit push-refusal check |
| files | Token and checksum failures, upload, write-once protection, `HEAD`, byte ranges, and repeated downloads |

Every role also has its health response, immediate hit feed, and ledger inventory
checked. Any failed assertion exits the container non-zero; a successful run writes
`/results/summary.json`.

## Build

From the repository root:

```bash
docker build \
  -t pkgcache-six-repositories \
  ./examples/six-repositories
```

The digest-pinned Alpine, Node, and Python base images can themselves be pulled
through pkgcache, making the Dockerfile a practical OCI build example. The Docker
daemon or BuildKit builder must already trust `certs/ca.crt`:

```bash
docker build \
  --build-arg BASE_REGISTRY=HOST:8443/dockerhub \
  -t pkgcache-six-repositories \
  ./examples/six-repositories
```

For a named project, use
`BASE_REGISTRY=HOST:8443/PROJECT/dockerhub`.

## Run the online warm/hit test

The files role needs its project write token. Put only that token in a temporary
read-only file; it is mounted at runtime and is never copied into the image.
`CACHE_HOST` must be a hostname or IP present in the serving certificate.

On Linux, testing the local Compose stack via host networking is the shortest
route because the generated certificate includes `localhost`:

```bash
docker run --rm --network host \
  -e CACHE_HOST=localhost \
  -e PROJECT=global \
  -e TEST_PHASE=online \
  -v "$PWD/certs/ca.crt:/certs/ca.crt:ro" \
  -v "/path/to/files-token:/run/secrets/files_token:ro" \
  pkgcache-six-repositories
```

The defaults deliberately use small, pinned artifacts. Override any fixture with
environment variables such as `OCI_IMAGE`, `PYPI_PACKAGE`, `NPM_PACKAGE`,
`APT_PACKAGE`, `APK_PACKAGE`, or `GIT_REPOSITORY`.

When a custom artifact changes the package name, set its matching
`*_LEDGER_QUERY` variable too so the inventory assertion searches for that name.

## Prove offline replay

After the online run passes, switch the same project to offline mode in the
console, wait for its health response to report `"offline": true`, and rerun:

```bash
docker run --rm --network host \
  -e CACHE_HOST=localhost \
  -e PROJECT=global \
  -e TEST_PHASE=offline \
  -v "$PWD/certs/ca.crt:/certs/ca.crt:ro" \
  pkgcache-six-repositories
```

The offline phase creates fresh client caches again. Because pkgcache itself
reports offline mode, success demonstrates that the warmed OCI layers, package
artifacts, apt/apk indexes, Git mirror, and generic file are all replayable without
an upstream fallback.

## CI inputs

| Variable | Default | Purpose |
|---|---|---|
| `CACHE_HOST` | `localhost` | Cache hostname, matching the TLS certificate |
| `CACHE_HTTPS_PORT` | `8443` | Unified HTTPS port |
| `CACHE_APT_PORT` | `3142` | apt/apk forward-proxy port |
| `PROJECT` | `global` | Project whose six roles are tested |
| `TEST_PHASE` | `online` | `online` warms/repeats; `offline` replays |
| `CA_CERT` | `/certs/ca.crt` | Mounted public CA |
| `FILES_TOKEN_FILE` | `/run/secrets/files_token` | Runtime-only files write token |
| `RESULTS_DIR` | `/results` | Location for `summary.json` |

`FILES_TOKEN` is also accepted for CI systems that inject masked environment
variables, but the mounted file is preferable because it does not appear in
container inspection output.
