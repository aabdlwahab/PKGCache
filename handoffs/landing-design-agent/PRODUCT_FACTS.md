# Product facts and claim guardrails

Use this file when writing marketing copy. It separates verified product behavior
from claims that need qualification.

## Verified product facts

- One Python service exposes six cache roles:
  - OCI/container images;
  - PyPI for pip and uv;
  - npm;
  - apt and apk through one forward-proxy role;
  - Git mirror-and-serve;
  - generic files.
- Five HTTPS roles share port `8443`.
- apt and apk use a plain-HTTP forward proxy on port `3142`.
- One process can serve a global cache and multiple isolated projects on the same
  ports.
- Cache writes are atomic and content-addressed.
- Concurrent requests for the same uncached artifact are single-flight: one
  upstream stream is shared while the artifact is written to cache.
- Cached byte artifacts support Range requests.
- Cache state is versioned in a separate git + DVC repository.
- Operators can checkpoint, export, import, roll back, and switch online/offline
  mode from the CLI or console.
- Checkpoints can be taken while the cache continues serving.
- Offline mode serves cached content only. A cache miss fails.
- The console exposes live downloads, recent activity, storage, package browsing,
  usage statistics, hit rate, and an estimated time-saved value.
- Generic files support downloads and token-gated uploads.
- Git is read-only mirror-and-serve. Cached repository data is available offline.

## Claims that need precise wording

### “One port”

Do not say every protocol uses one port. Correct wording:

> Five HTTPS roles share `:8443`; apt and apk use the `:3142` forward proxy.

“One service” or “one process” is safe.

### “Fetch once”

Safe as a positioning line for immutable package content and concurrent misses,
but avoid asserting that every ecosystem contacts upstream exactly once forever.
Git mirrors revalidate online, tags can change, caches can be rolled back, and
operators can remove content.

### “Instant”

Do not promise instant delivery or fixed latency. Say “served locally,” “from
your infrastructure,” or “without another upstream download.”

### Offline behavior

Never say “everything works offline.” Correct wording:

> Cached dependencies remain available offline; uncached requests fail clearly.

### Air-gap export

The export workflow can produce a full export or a base-to-target delta. The
product positioning emphasizes delta transfer, but avoid saying every export is
always a single file.

### Metrics

Do not use `93%`, `41.6 GB`, time-saved hours, or performance multipliers as
product-wide proof. Those are example-instance values unless backed by measured
customer or benchmark data.

### Git LFS

The implementation contains Git LFS cache routes, while some repository
documentation still describes LFS as future work. Do not make Git LFS a headline
claim until the documentation discrepancy is resolved and end-to-end behavior is
confirmed.

## Preferred marketing language

- shared package cache
- served locally
- fills on demand
- one Python service
- versioned cache state
- checkpoint without stopping builds
- export the delta
- disconnected environment
- cached packages remain available offline
- existing package clients and familiar commands

## Avoid

- instant
- everything works offline
- every tool on one address
- exactly one download forever
- zero waiting
- guaranteed time or bandwidth savings
- unqualified benchmark numbers

