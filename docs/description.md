# Product description, in pastable pieces

Every block below is meant to be copied as-is into a particular field. They say the same
thing at four lengths, in the README's voice: what it does, for whom, and nothing it does
not do.

The repository's current GitHub description still describes the Python implementation that
this tree replaced — "a single host runs one Python service … versioned with git + DVC" —
and it is cut off mid-sentence at GitHub's 350-character limit. It is also what shows in
link previews and what search engines index, so it is the one worth replacing first.

## GitHub About field

Settings are not needed: the pencil icon beside **About** on the repository home page.
262 characters, inside the 350 limit.

```
One package cache in front of PyPI, npm, Docker registries, apt, apk and git. Fetch once, serve every later request from local disk, and carry the whole cache across an air gap as a verified pack. Two static Go binaries: pkgcache for a laptop, pkgreg for a team.
```

Shorter, if that reads long in the sidebar — 130 characters:

```
A pull-through cache for PyPI, npm, Docker, apt, apk and git. One static Go binary, on one machine or in front of your whole team.
```

## Homepage field

Beside the description, now that the site is live:

```
https://aabdlwahab.github.io/PKGCache/
```

## Topics

GitHub takes up to 20. These are the ones people actually search:

```
package-cache pull-through-cache artifact-repository pypi npm oci-registry docker-registry apt alpine-apk git-mirror air-gapped offline-first golang devtools ci-cd developer-tools self-hosted
```

## One line

For a talk slide, a footer, a chat topic:

```
A download crosses your network once.
```

## Two sentences

For a link preview, a newsletter blurb, a directory entry:

```
pkgcache keeps every package your machines have already downloaded — Python wheels, npm tarballs, container images, .debs, .apks, git objects — and serves them back from local disk. The same engine runs as a team cache in front of the registries, and the whole thing can be carried into a network with no internet as a single verified pack.
```

## Short paragraph

For a README lead, a package listing, the site's meta description — about 90 words:

```
Your builds download the same dependencies over and over. pkgcache fetches each one once
and serves every later request from local disk: pip and uv, npm, yarn and pnpm, Docker and
any OCI registry, apt and apk, git clones, and plain files — six ecosystems on one loopback
port, with no certificate, no account and nothing another machine can reach. Run the same
engine as pkgreg and it becomes the cache your whole team points at. Export what it holds
and a machine with no network keeps building.
```

## Longer

For a release announcement, a Show HN intro, an internal proposal — about 200 words:

```
PKGCache is a pull-through cache for the package managers a build actually uses: pip and
uv, npm, yarn and pnpm, any OCI registry, apt and apk, git, and plain files. It ships as
two programs from one codebase. pkgcache runs on a single machine — one loopback port, no
certificate, no account, no privileged setup, and it refuses to bind an address another
machine can reach. pkgreg is the same engine run as a host a team points at, with
projects, accounts, quotas, an audit log and a web console.

Point a machine at a team's pkgreg and you get three tiers: the local disk answers first,
the team cache answers what this machine has never seen, and only the team talks to the
internet. The team cache is verified by a CA fingerprint you are given out of band, so
nothing has to install a certificate authority.

Docker is handled rather than documented: pkgcache rewrites a build's base images, its
`# syntax=` frontend and its borrowed `COPY --from=` images to come through the cache,
without modifying the Dockerfile on disk.

Name what a project holds, export it as a verified pack, and import it on a machine that
has never had a network.
```

## What not to claim

Three that are easy to get wrong, and all three appear in the notes of every unsigned
build for a reason:

- **The installers are not code-signed yet.** There is no Apple Developer account and no
  Windows certificate, so the macOS `.pkg` and the Windows `setup.exe` are unsigned and
  say so. Every artefact does carry GitHub provenance attestation, which is verifiable
  with `gh attestation verify` and is not the same thing.
- **The desktop app is not a static binary.** pkgcache and pkgreg are CGO-free and have no
  runtime dependencies; the app links GTK and WebKit on Linux and AppKit on macOS.
- **apt and git are not chained through a team cache.** Chaining covers pypi, npm and OCI.
  apt and git derive their origin from the request itself, and `files` has no upstream at
  all, so those three are absent from a chain rather than half-supported.
