# Caching git repositories

The `git` role is a **mirror-and-serve** smart-HTTP git server (port `3143`, HTTPS).
Unlike the other ecosystems it can't byte-cache responses — a git fetch is a
negotiation — so it keeps a real bare mirror on disk (`git clone --mirror`),
revalidates it online, and serves `git upload-pack` from it. Offline it serves the
mirror as-is.

It caches whatever pulls code from git: `pip install git+https://…`, CMake
CPM/FetchContent, vcpkg ports, submodules, ansible roles.

## URL scheme

Put the **real upstream host** in the path:

```
https://<cache-host>:3143/<upstream-host>/<owner>/<repo>.git
```

```bash
git clone https://cache.local:3143/github.com/pallets/click.git
git clone https://cache.local:3143/gitlab.com/group/project.git
```

## Transparent adoption (recommended)

Rewrite upstream URLs to the cache once per machine/CI image, and everything —
including submodules, `pip`'s `git+https` deps, and CPM — routes through the cache
with no per-project changes:

```bash
git config --global url."https://cache.local:3143/github.com/".insteadOf "https://github.com/"
git config --global url."https://cache.local:3143/gitlab.com/".insteadOf  "https://gitlab.com/"
```

## Trusting the cache CA

The cache serves HTTPS with the private CA from `scripts/gen-certs.sh`. Point git at it:

```bash
# per user (covers all repos):
git config --global http."https://cache.local:3143/".sslCAInfo /path/to/certs/ca.crt
# or install certs/ca.crt into the system trust store (update-ca-certificates /
# update-ca-trust) and no git config is needed.
```

CI one-liner: `GIT_SSL_CAINFO=certs/ca.crt git clone https://cache.local:3143/github.com/…`.

## Behavior

- **Read-only.** `git push` is refused (`git-receive-pack` → 403, "read-only mirror").
- **Heads + tags only.** `refs/pull/*` and other server-side refs are not mirrored,
  so a commit reachable *only* from a PR ref isn't cached. Clone/fetch of branches,
  tags, and any commit reachable from them (including `--depth`, `--filter=blob:none`,
  and SHA-pinned `fetch <sha>`) works.
- **Freshness.** A mirror is re-fetched from upstream at most once per `refs_ttl`
  (default 60 s, see `pkgcache/pkgcache.yaml`). Within that window clones are served
  from the mirror with no upstream contact (a cache hit).
- **Offline.** With `OFFLINE=1`, cloning a mirrored repo works with no upstream; an
  un-mirrored repo returns 404 (the expected air-gap miss).
- **First clone of a large repo** makes the first requester wait for the server-side
  `clone --mirror` (single-flight — concurrent requesters share it); progress shows
  in the console's Downloads panel.

## Pre-seeding for the air gap

Add a `git:` list to your seed file (see `pkgcache/seed.example.yaml`) and run
`scripts/prefetch.py` on the online side; each entry triggers a server-side mirror
clone. Then `pkgops.py checkpoint` versions the mirrors into DVC.

## Air-gap / DVC notes

- Mirrors are DVC-tracked with the rest of the cache. They run with `gc.auto=0`, so
  git never rewrites packfiles on its own — the **only** rewrite is a geometric
  `git repack` run once per checkpoint. New commits arrive as new packfiles
  (append-only), so a checkpoint's shuttle delta is proportional to what actually
  changed, not the whole mirror.
- **Rollback** (`git checkout <commit> && dvc checkout` of the cache repo) rewinds
  the mirrors; a git server serving older refs is a valid state.
- **Local filesystem only.** Serving `upload-pack` while a fetch/repack runs relies
  on POSIX unlink-while-open semantics; the cache tree must not be on NFS.

## Not yet cached

- **Git LFS** objects (phase 2) — repos that store large binaries in LFS will fetch
  those objects direct from upstream until the LFS cache lands (fails offline).
- Very large mirrors (e.g. `torvalds/linux` ≈ 3–4 GB) are cached whole; there is no
  eviction (by design — the stats tab's per-repo request counts inform a future
  policy).
