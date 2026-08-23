# Git through the cache

The cache keeps a mirror of the repositories you clone, so a second clone — on this
machine, in a container, or by a colleague pointed at the same server — is served from
disk. It speaks Git's smart HTTP protocol, so `git` needs no plugin and no special
client.

It is **read-only**. Push is refused, explicitly, with a message saying so rather than a
transport error to guess at.

## The URL shape

A repository is addressed by its real upstream host, kept in the path:

```
http://127.0.0.1:41780/global/git/github.com/some-org/some-repo
```

The host stays visible on purpose. A mirror that renames what it mirrors makes it
impossible to tell, from a URL in a lockfile or a submodule, what is actually being
fetched. The first path segment must be a DNS host, and every segment is checked before
it reaches the filesystem.

## Using it

The blunt way is to clone the cache URL directly. The useful way is to let git rewrite
the URLs you already have:

```sh
pkgcache run -- git clone https://github.com/some-org/some-repo
pkgcache shell     # every git command in this shell, for as long as it lasts
```

Both work by setting `GIT_CONFIG_COUNT` and an `insteadOf` rule, which is git's own
mechanism:

```
GIT_CONFIG_COUNT=1
GIT_CONFIG_KEY_0=url.http://127.0.0.1:41780/global/git/github.com/.insteadOf
GIT_CONFIG_VALUE_0=https://github.com/
```

That covers what you type, and also what you do not: submodules, `pip install git+https`,
Go module fetches, CMake `FetchContent`, and anything else that hardcodes a GitHub URL
several layers down.

`github.com` is redirected by default. Add others with `-git-host`:

```sh
pkgcache run -git-host github.com,gitlab.com -- git clone https://gitlab.com/g/p
```

In a build, `pkgcache build` declares the same variables as `ARG`s — see
[docker-builds.md](docker-builds.md).

## Making it permanent

```sh
pkgcache persist          # writes the rules into your git config
```

Without it, the rules live only in the session `run` and `shell` create, and vanish when
the shell exits. That is the default because a global git rewrite is a real change to a
machine, and it should be something you asked for.

## Large files

Git LFS works. The cache serves the batch API and the objects themselves, so
`git lfs pull` resolves through it like anything else. Uploads are refused, for the same
reason pushes are.

## What it does not do

**It does not chain to a team cache.** Unlike PyPI, npm and OCI, git derives its origin
from the request itself rather than from a configured upstream, so there is no ordered
chain to walk and no fall-through. A machine pointed at a team `pkgreg` still fetches
git from the real host. This is a gap, not a decision that git is different in kind —
see [pkgcache.md](pkgcache.md#three-tiers).

**It does not accept pushes.** It is a mirror of upstream, not a place to publish.

## Freshness

Refs are re-checked against upstream on a short TTL, so a clone or fetch sees new commits
without a manual step, while a burst of fetches in a build does not become a burst of
upstream requests. Objects, once fetched, are immutable and never re-fetched.

Concurrent `git-upload-pack` operations are bounded, because pack generation is the
expensive part of serving git and an unbounded number of them is how a cache becomes
slower than the network it was meant to save.

## Offline

With the cache offline (`pkgcache setup -offline`, or the widget's toggle), clones and
fetches are served entirely from the mirror. What was never fetched fails; what was is
available with no network at all — which is the point of carrying a cache across an air
gap.
