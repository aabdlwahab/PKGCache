# Landing-page messaging and copy deck

The source files contain the full current copy. This document captures the
approved hierarchy and the reasoning behind it.

## Positioning

**Category:** versioned, air-gap-portable package cache.

**Primary promise:** stop repeat package downloads by serving cached dependencies
from your own infrastructure.

**Differentiator:** checkpoint the cache and transfer it into disconnected
environments.

**Operational proof:** six ecosystems in one Python service, with an operator
console.

## Current title and description

**Title**

> pkgcache — one versioned package cache for every build environment

**Description**

> pkgcache stores packages on first request, serves repeat requests from your
> infrastructure, and carries a versioned cache into disconnected environments.

## Four story beats

### 1. Problem

**Eyebrow:** the problem

**Headline:** Your builds keep downloading the same dependencies.

**Support:** Across CI runners and developer machines, repeat downloads slow
builds, consume bandwidth, and become a hard stop when the network is
disconnected.

### 2. Shared cache

**Eyebrow:** one cache, every ecosystem

**Headline:** Fetch once. Keep repeat package requests local.

**Support:** Point your existing package clients at pkgcache. It fills on demand,
serves cached content from your infrastructure, and combines concurrent misses
into one upstream transfer.

### 3. Versioning

**Eyebrow:** versioned cache state

**Headline:** Checkpoint the cache without stopping builds.

**Support:** Capture the cached content and its ledger while the service keeps
running. Every checkpoint becomes an auditable restore point and a source for
air-gap transfer.

### 4. Offline

**Eyebrow:** built for disconnected networks

**Headline:** Bring the cache with you. Keep cached builds running.

**Support:** Export a checkpoint delta, move it through your approved transfer
process, and import it on the far side. Package clients keep using the cache with
upstream access disabled.

## Ecosystem section

**Eyebrow:** Six ecosystems · one service

**Headline:** One cache service for the tools your builds already use.

**Support:** Five HTTPS roles share `:8443`; apt and apk use the `:3142` forward
proxy. Existing commands stay familiar, and the cache fills on demand as builds
run.

## Console section

**Eyebrow:** operate it from one place

**Headline:** See what is cached. Measure the value. Control every transfer.

**Support:** Monitor live downloads, cache health and storage; review estimated
time saved; and run checkpoint, export, import or rollback workflows from the
console.

## Closing CTA

**Headline:** Make the next build independent of the internet.

**Support:** Run pkgcache on your infrastructure, point existing package clients
at it, and checkpoint the cache when it is ready to move.

**Primary CTA:** Open the console

## Tone

- Technical but readable.
- Confident without benchmark theater.
- Short, concrete sentences.
- Explain outcomes before implementation details.
- Use the operator’s vocabulary: cache, checkpoint, delta, offline.
- Avoid generic SaaS language such as “revolutionize,” “seamless,” or
  “supercharge.”

