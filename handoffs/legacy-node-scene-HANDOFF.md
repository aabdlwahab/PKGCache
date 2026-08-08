# Legacy handoff — superseded node-scene animation

**For:** a design agent iterating on the hero animation (and, optionally, the
wider landing page).
**Prepared:** 2026-07-25.
**Scope of this package:** the product context you need to stay on-brand, the
current animation's full concept + mechanics, the exact constraints it must keep
satisfying, and the runnable current source.

---

## 0. TL;DR for the design agent

- The page is the **marketing homepage** for **pkgcache** (a.k.a.
  *package-registry*). Its one job is to explain the product and send visitors to
  the operator **console** (the app), reachable at `/console`.
- The **hero animation** currently depicts the product's core idea — a
  **pull-through cache**: build clients pull packages from a central cache; most
  requests are instant **HITs**, a **MISS** goes upstream, streams back, and fills
  a content-addressed blob; periodically a **checkpoint** commits the blobs.
- Your brief: **make this animation better** — clearer, more elegant, more
  on-brand — while keeping it *truthful to what the product does* and keeping the
  hard constraints in §5. The typed command line under the headline is a separate
  element and should stay.
- **Start here:** open `current/scene-standalone.html` (double-click, no build) to
  watch and tweak the animation in isolation.

---

## 1. What the product is

**pkgcache / package-registry** is a *versioned, air-gap-portable package cache*.

One Python process pull-through-caches **six package ecosystems** for a whole
build/CI fleet, then lets you version the cache and carry it across an air gap:

| Ecosystem | What it caches | Replaces |
|---|---|---|
| **Docker / OCI** | container images (Docker Hub, GHCR, Quay) at `/v2` | zot |
| **PyPI (pip/uv)** | Python wheels, PEP 503/691 index | devpi |
| **npm** | Node packages + tarballs | Verdaccio |
| **apt / apk** | Debian/Alpine OS packages (HTTP forward proxy) | apt-cacher-ng |
| **git** | git repos, mirror-and-serve + Git LFS (read-only) | *(new)* |
| **files** | generic artifacts — wget to download, token PUT to upload | *(new)* |

**The three ideas the marketing page sells:**

1. **Fetch once, serve forever.** A pull-through cache: first request fills the
   cache from upstream; every later request is served from local disk. Streaming
   is *single-flight* — the first requester "leads" (tees the upstream to disk and
   to the client at once, and finishes even if that client disconnects); everyone
   else "follows" the same download. One upstream fetch per artifact, ever.
2. **One process, one port, no zoo.** Everything HTTPS on `:8443` (apt/apk keep a
   plain-HTTP proxy on `:3142`). One slim Python image instead of four polyglot
   daemons. Serves many isolated projects on the same ports via URL prefix.
3. **Versioned & air-gap portable.** The cache is stored content-addressed and
   versioned in its **own git + DVC repo**. You *checkpoint* live, *export* just
   the delta into a staging dir, carry it across the gap on removable media,
   *import* on the offline host, and serve with `OFFLINE=1` (a miss simply fails).

**Supporting concepts worth knowing (they appear on the page):**

- **Content-addressed store (CAS):** blobs live at `blobs/sha256/<hex>`, written
  atomically (temp → fsync → rename) so a checkpoint can hash the cache *live*
  with no downtime, and HTTP Range (206/resume) is free.
- **Per-ecosystem SQLite ledger:** records each artifact as it's cached, so you
  always know exactly what a checkpoint contains and the offline side can resolve
  tags/refs.
- **The operator console:** a dependency-free React SPA (Overview / Statistics /
  Packages) showing live downloads, a HIT/MISS/FAIL feed, usage stats with an
  estimated *"time saved"*, disk monitoring, a project switcher, and one-click
  checkpoint / export / import / rollback. **This is the "current homepage" the
  landing page links to.**

**Voice / positioning:** technical, confident, terminal-flavored, no fluff.
Reference lines from the page: *"Fetch it once. Serve it forever. Even across the
air gap."* / *"A hit serves from disk. A miss streams once — to everyone."*

---

## 2. Brand & design system (stay inside this)

The landing page deliberately shares the **console's** visual language so the two
feel like one product. Do not introduce a new palette or typeface.

- **Typeface:** **IBM Plex Mono** everywhere (loaded via `local()` with a
  `/fonts/IBMPlexMono.woff2` fallback; degrades to the system mono stack). Yes —
  headings are mono too. It reads as "terminal / tooling."
- **Shape language:** sharp corners (`--radius: 3px`), thin 1px borders, uppercase
  mono labels with letter-spacing (`--tspace: 0.12em`), HUD/terminal feel.
- **Color (OKLCH tokens; theme-aware via `[data-theme]` on `<html>`):**
  - Surfaces: near-black **blue**-black backgrounds; panels one step lighter.
  - **Accent = blue** `oklch(0.66 0.16 248)` → primary actions, MISS/stream, "lit"
    CAS tiles. rgb glow token `--glow: 74 128 240`.
  - **HIT/success = green** `oklch(0.78 0.15 152)` → HITs, committed CAS tiles,
    "online". rgb glow token `--glow-ok: 69 217 138`.
  - Per-ecosystem hues used for dots/accents: docker = blue `248`, pip = amber
    `92`, npm = red `12`, git = violet `300`, apt = orange `42`, files = green
    `152`.
  - Also: `--bad` red `28`, `--warn` amber `82`, muted greys for secondary text.
- **The "night" band:** the hero and footer force a deep near-black-blue palette
  (in both light and dark themes) with a faint masked HUD grid and two radial
  glows. The animation lives inside this band.
- **Motion vocabulary:** subtle, purposeful, HUD-like — boot-in fades, a blinking
  cursor block, gentle float, pulse rings, a scan/sweep. Nothing bouncy or
  playful.
- **Logo:** a `[ ]` bracket pair with a cursor block (the console favicon). Green
  brackets on a dark tile.

Full token values are at the top of `current/landing.css` (`:root`,
`[data-theme="dark"|"light"]`, `.night`) and inlined in
`current/scene-standalone.html`.

---

## 3. The hero animation — current concept & mechanics

### 3.1 What it depicts (the metaphor)

A live **pull-through cache**, hub-and-spoke:

```
  clients (left)            CACHE (center)            upstreams (right)
  ┌──────────┐                                        ┌────────────────────┐
  │ docker   │──req──▶  ┌───────────────┐   ──miss──▶ │ registry-1.docker.io│
  │ pip / uv │──req──▶  │  pkgcache·CAS │             │ pypi.org            │
  │ npm      │──req──▶  │  ▦▦▦▦▦▦ (blob │  ◀─stream──  │ registry.npmjs.org  │
  │ git      │──req──▶  │  tile grid)   │             │ github.com          │
  └──────────┘          └───────────────┘             └────────────────────┘
        ▲   HIT (green) returns instantly from the cache ─┘
   thin dashed "rails" connect every client to the cache, and the cache to every upstream
```

### 3.2 The per-request lifecycle (what actually animates)

A loop spawns a request roughly every 0.6–1.3 s (capped at 7 in flight):

1. A colored **request dot** (colored by the client's ecosystem) travels along the
   rail from a random client to the **left face** of the cache.
2. The cache decides **HIT (~58%)** or **MISS (~42%)**:
   - **HIT** — a **green** dot returns from the cache to the client; an existing
     CAS tile *pings* (an already-cached blob was served); the client node flashes
     green. No upstream contact.
   - **MISS** — the dot continues from the cache's **right face** to the matching
     **upstream** (which lights up "busy"); a fatter **stream** dot flows back from
     upstream into the cache; a **new CAS tile lights up** (blue = freshly cached);
     then a dot returns to the client (served).
3. Every ~15 freshly-lit tiles trigger a **checkpoint**: the cache node flashes and
   all lit (blue) tiles convert to **committed** (green). When the 24-tile grid is
   fully committed it clears and refills — the checkpoint → commit → new-delta
   lifecycle.
4. A **readout** (bottom-left) ticks the running **hit rate**, **blobs cached**,
   and an **online** status dot.

### 3.3 DOM / class contract (so redesigns stay wired to the JS)

- `.scene` — the band (relative, clipped).
- `svg.rails > line.rail(.rail-up)` — the connecting rails, drawn in a
  `viewBox="0 0 100 100"` with `preserveAspectRatio="none"` so their **percentage**
  endpoints line up with the nodes at any size.
- `.flow` — overlay that holds the nodes and the moving dots.
- Nodes, each with a `data-node` id the JS looks up:
  - clients `.fnode.fclient.n-{docker,pip,npm,git}` (`data-node="c-*"`),
  - upstreams `.fnode.fup.u-{docker,pip,npm,git}` (`data-node="u-*"`),
  - cache `.fnode.fcache` (`data-node="cache"`) containing `.fcache-h` + `.cas`.
- `#cas` — the tile grid; JS injects 24 `.cas-tile` (states: default / `.lit` /
  `.committed` / `.ping`).
- `.req-dot.rd-{eco|hit|stream}` — the moving dots (created/removed by JS).
- State classes the JS toggles: `.fclient.hit`, `.fup.busy`, `.fcache.flash`.
- Readout ids: `#hitrate`, `#cached`.

### 3.4 Timing & tunables (in `landing.js`, top of the scene block)

- `TILES = 24`, `LIT_MAX = 15` (tiles before a checkpoint).
- HIT probability: `Math.random() >= 0.42` → ~58% HIT.
- Leg durations (ms): client→cache 620, cache→upstream 560 (+200 traverse),
  upstream→cache stream 820, cache→client 520.
- Spawn cadence: `620 + random*640` ms; in-flight cap 7.
- Positions: clients at `left: 11%`, upstreams at `left: 89%`, cache centered;
  node vertical %s must match the rail `y` coordinates.

### 3.5 How the motion is implemented (and why)

- Dots move via the **Web Animations API** (`element.animate(...)`, transform
  translate in px). Node/edge pixel coordinates are **measured** from
  `getBoundingClientRect` and recomputed on resize (`ResizeObserver`). This keeps
  dots aligned to nodes at any viewport size and — importantly — avoids inline
  styles (see §5, CSP).
- Tile/flash/ping states are plain CSS class toggles + keyframes.

---

## 4. History / what this replaced (context, not a request)

The **first** version of this hero was a generic "moving tracks" animation —
horizontal lanes with package chips streaming left→right past a vertical spine.
It was replaced precisely because it was decorative but didn't say *what the
product is*. The pull-through version was chosen for being **literally the product
model**. Keep that bar: **prefer meaning over spectacle.** If you propose something
new, it should still read, at a glance, as "a cache that serves hits locally and
only misses go upstream."

---

## 5. Hard constraints (must keep satisfying)

These are non-negotiable because of where and how the page ships.

1. **Strict CSP in production.** The page is served by nginx with
   `style-src 'self'; script-src 'self'` — **no inline `<style>`, no inline
   `<script>`, no `style="..."` attributes, no inline event handlers.** In the
   real page, CSS and JS are separate same-origin files (`landing.css`,
   `landing.js`). Setting styles *from JavaScript* (`el.style.transform = …`,
   `element.animate`) is allowed and is how the dots move. *(The
   `scene-standalone.html` playground inlines things only for your convenience —
   any change there must be portable back into the separate files.)*
2. **Fully self-contained / air-gap-friendly.** No CDN, no external fonts, no
   remote requests of any kind. Everything must work offline. Fonts are local;
   images are inline SVG or `data:` URIs.
3. **Theme-aware.** Must look right in both dark and light (`[data-theme]` on
   `<html>`; also respect `prefers-color-scheme`). The hero sits in the forced
   "night" band, but tokens still resolve per theme elsewhere.
4. **Reduced motion.** With `prefers-reduced-motion: reduce`, the animation must
   **not** run; instead show a meaningful static end-state (currently: a
   partly-committed CAS grid, "93% hit rate", "1,284 blobs cached").
5. **Responsive.** No horizontal page scroll. The scene must degrade gracefully on
   narrow screens (currently: upstream nodes + their rails hide < 720px).
6. **Decorative / accessible.** The scene is `aria-hidden="true"` — it must carry
   no information that isn't also in the text. Fine to keep it decorative.
7. **Truthful.** Labels and behavior should reflect the real system (real upstream
   hostnames, real ecosystems, HIT/MISS/checkpoint semantics). Don't invent
   features.
8. **Keep the typed command line.** The `$ …` line that types out real pull /
   checkpoint commands under the headline is a separate element (`#typed` in
   `landing.js`) and should remain.

---

## 6. What's in this package

```
handoffs/
├── HANDOFF.md                     ← this file
└── current/
    ├── scene-standalone.html      ← ISOLATED animation playground (open directly, no build)
    ├── landing.html               ← full production homepage markup
    ├── landing.css                ← full production styles (tokens + all sections + scene)
    ├── landing.js                 ← full production JS (theme, typed cmd, reveal, scene engine)
    └── theme.js                   ← tiny pre-paint theme applier (shared with the console)
```

### How to preview

- **Just the animation:** open `current/scene-standalone.html` in any browser
  (double-click). Resize to test responsiveness; toggle your OS "reduce motion"
  setting to see the static fallback.
- **The whole page:** from `current/`, run `python3 -m http.server 8000` and open
  `http://localhost:8000/landing.html`. (Opening `landing.html` directly also
  works, though `/console` and `/theme.js` absolute links won't resolve off a
  server.)

### Where it lives in the repo (for the engineer who lands your redesign)

The production files are `webui/console/public/landing.{html,css,js}`; Vite copies
`public/*` verbatim into the build. nginx serves the landing page at `/` and the
console SPA at `/console` (`webui/console/nginx.conf`). Keep the CSS/JS split and
the DOM/class contract in §3.3, and the redesign drops straight in.

---

## 7. Design brief — where to take it

Open-ended; pick what makes it stronger. Some directions:

- **Readability of the core idea.** Can a first-time viewer tell within ~3 seconds
  that hits stay local and only misses go upstream? Consider clearer labeling of
  the two return paths, a subtle "0 ms" vs "streaming…" cue, or motion that makes
  the HIT feel *instant* vs the MISS feel like a *round-trip*.
- **The CAS grid.** It's the most novel element — the cache visibly filling and
  getting checkpointed. Is the lit→committed→cleared story legible? Could the
  checkpoint moment be more of a "beat"?
- **Single-flight.** Not currently shown. An optional idea: occasionally two
  clients request the same artifact and *share* one upstream stream (the product's
  signature behavior). Only add if it stays legible.
- **Composition & rhythm.** Node placement, rail styling, dot pacing, density,
  the readout. Make it feel calm and deliberate, not busy.
- **Air-gap hint.** Optional: a rare "offline" beat where upstreams dim and the
  cache keeps serving hits (misses fail) — the product's other headline. Only if
  it doesn't muddy the main read.
- **Light theme.** Verify it's as good as dark; the accent/HIT contrast and glows
  need checking on light surfaces.

### Acceptance criteria for any redesign

- Reads as a pull-through cache (hit local / miss upstream) at a glance.
- Stays on-brand: IBM Plex Mono, sharp corners, the blue-accent + green-HIT OKLCH
  palette, HUD/terminal restraint.
- Satisfies every constraint in §5 (CSP-portable, self-contained, theme-aware,
  reduced-motion fallback, responsive, decorative/aria-hidden, truthful).
- Ships as separate `landing.css` / `landing.js` edits (no inline styles/scripts),
  keeping the DOM/class contract or updating the JS to match a new one.
- The typed command line survives.

Questions worth confirming with the product owner before big swings: is the
hub-and-spoke framing the keeper, or is a different cache metaphor on the table?
How much motion is "too much" for this audience? Should single-flight and/or the
air-gap beat be shown explicitly?
