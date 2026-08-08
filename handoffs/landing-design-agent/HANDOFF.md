# pkgcache landing-page animation handoff

Prepared: 2026-07-25

This is the canonical handoff for a design agent working on the pkgcache
marketing homepage. It contains the product context, current implementation,
current copy, factual guardrails, user feedback, and runnable source.

## Start here

1. Read this file.
2. Read `PRODUCT_FACTS.md` before writing claims.
3. Read `COPY_DECK.md` for the approved messaging hierarchy and current copy.
4. Preview `current/landing.html` through a local HTTP server.
5. Present 2–3 low-fidelity animation directions before implementing another
   full concept.

Preview:

```bash
cd handoffs/landing-design-agent/current
python3 -m http.server 4173
# open http://127.0.0.1:4173/landing.html
```

The absolute `/landing.css`, `/landing.js`, and `/theme.js` paths require the
`current` directory to be the server root.

## Product in one sentence

pkgcache is one versioned package-cache service for container images, Python,
Node, system packages, Git repositories, and generic artifacts; it serves repeat
requests locally and lets operators move checkpointed cache state into
disconnected environments.

## The job of the homepage

The homepage should make a technical buyer understand three things quickly:

1. Repeat package downloads are wasteful.
2. pkgcache puts one shared cache in front of the build fleet.
3. The same versioned cache can be carried into an air-gapped environment.

The primary conversion is opening `/console`. The visitor is likely a platform
engineer, DevOps lead, build/release engineer, or operator responsible for
connected CI and disconnected deployment environments.

## User feedback history

The user has rejected several animation directions:

- The original node-and-particle network idea was acceptable in concept but its
  execution felt “cheap.”
- A diagnostic “live request trace” looked modern but did not sell the product.
- A multi-stage infrastructure journey sold more value but felt too complicated.
- A simplified three-message result panel was still not liked.

The user wants an animation that is:

- catchy;
- clearly marketing-led;
- simple to understand;
- visually premium;
- not a diagnostic dashboard;
- not a dense system diagram.

An earlier explicit requirement was to preserve commands being typed. The
current scroll-driven implementation no longer contains that typed-command
element. Treat this as an unresolved requirement: either restore it in the next
direction or call out why a proposed alternative communicates the same value
more effectively.

## Current animation

The current source is a pinned, scroll-scrubbed four-beat story:

1. **Problem:** six package clients repeatedly reach public upstreams.
2. **One cache:** a central cache “wall” appears; cached requests return locally
   and concurrent misses share an upstream transfer.
3. **Checkpoint:** cached tiles become committed and a small version graph
   advances.
4. **Offline:** an exported delta crosses the air-gap seam, fills an offline
   cache, and cached requests continue while uncached requests fail.

Implementation details:

- `#story` is the stage.
- `.stage-pin` stays pinned while `.stage-track` supplies `460vh` of scroll.
- Four `.act` articles provide the copy.
- JavaScript maps scroll position to `data-act`, `--p`, `--t`, `--peel`, and
  `--peel-inv`.
- Web Animations API particles illustrate requests and responses.
- The story reverses when the reader scrolls upward.
- Desktop uses a copy/visual split. Tablet and mobile stack the visual above the
  copy and remove secondary detail.
- Reduced-motion mode keeps the scroll states but removes autonomous motion and
  sliding transitions.
- With JavaScript disabled, the pin releases and the copy stacks in document
  flow.

This implementation is technically capable, but it is still information-dense.
The next design should first prove that its core idea can be explained in a
single sentence and a three-frame storyboard.

## Recommended concept space

These are directions to explore, not approved solutions:

### A. First pull / next pull

Run the same command twice. The first is labeled `UPSTREAM`; the second is
`LOCAL CACHE`. End on one line: “Fetch once. Keep the next build local.”

Why it may work: it demonstrates the benefit with one comparison and preserves
the command-writing idea.

### B. Command becomes the payoff

Type a real package command, press Enter, then let the command resolve into:
“Cached. Versioned. Ready offline.”

Why it may work: very little UI, strong product linkage, and a premium motion
opportunity.

### C. One artifact, one gap

Show one artifact enter pkgcache, compress into a delta, cross a seam, and appear
as `OFFLINE READY`.

Why it may work: highlights the most differentiated feature. Keep it to one
artifact and one path—no fleet map.

### D. No explanatory diagram

Keep the typed command and use only a concise response:
`HIT · served locally` or `CHECKPOINT · delta ready`.

Why it may work: the headline and copy do the selling; motion provides polish
without competing for attention.

## Design constraints

- Stay within the existing terminal-inspired design system:
  - IBM Plex Mono;
  - near-black blue surfaces;
  - blue accent;
  - green success state;
  - restrained amber/red for warnings;
  - sharp 3px corners;
  - fine borders and subtle grid texture.
- Avoid a separate illustration style, stock imagery, mascots, or glossy 3D.
- Do not invent benchmark numbers, latency values, hit rates, bandwidth savings,
  or customer logos.
- If sample metrics are shown, label them clearly as illustrative.
- Never imply that an uncached package works offline. Offline misses fail.
- Keep the content understandable at 390px without relying on hover.
- Provide a meaningful reduced-motion state.
- Preserve progressive enhancement: page copy must remain readable with
  JavaScript disabled.
- Keep the strict CSP:
  - scripts and styles are self-hosted;
  - no inline event handlers;
  - no external runtime dependencies.

## Definition of a successful next direction

A first-time visitor should be able to answer these within five seconds:

- What is it? A shared package cache.
- Why do I care? Repeat builds stop re-downloading dependencies.
- What is different? The cache is versioned and portable into disconnected
  environments.

The animation should add one memorable proof point—not try to explain the whole
architecture.

## Package contents

- `HANDOFF.md` — this brief.
- `PRODUCT_FACTS.md` — verified claims and claim boundaries.
- `COPY_DECK.md` — messaging hierarchy and current approved copy.
- `current/landing.html` — current page markup.
- `current/landing.css` — current page styles and responsive behavior.
- `current/landing.js` — current scroll and animation engine.
- `current/theme.js` — theme bootstrap.
- `current/README.md` — preview and integration instructions.
- `previews/desktop.png` — current desktop capture.
- `previews/mobile.png` — current mobile capture.

