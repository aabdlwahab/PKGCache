# Hero animation redesign — "the membrane"

**From:** design · **Date:** 2026-07-25 · **Replaces:** the hub-and-spoke `.scene`
in `webui/console/public/landing.{html,css,js}`
**Status:** ready to land. Drop-in, no build step, no new dependencies.

---

## 0. TL;DR for the engineer

Three files in this folder are the deliverable:

| File | What to do with it |
|---|---|
| `landing-hero.html` | replaces the hero `<section>` + `.scene` block in `landing.html` |
| `landing-hero.css` | replaces the hero/scene block in `landing.css` (token-based, nothing else changes) |
| `landing-hero.js` | replaces the scene engine + typed-command block in `landing.js` |
| `scene-standalone.html` | playground — loads the three real files, open it to iterate |

Everything else in the page (theme applier, reveal, footer, other sections)
is untouched. The typed command line is kept, with the same `#typed` hook.

Preview: `python3 -m http.server 8000` in this folder → `localhost:8000/scene-standalone.html`.
(Opening it via `file://` shows a hint instead — the loader `fetch`es the markup
fragment; in production the fragment is just part of `landing.html`, no fetch.)

---

## 1. What changed, and why

The old scene put the cache in the middle of a hub-and-spoke star. It read as
"traffic", and the two things that actually distinguish the product — *how little
crosses to upstream*, and *the air gap* — were not visible.

The redesign makes the cache a **wall across the page**:

```
 clients ──lane──▶ │ CAS │ ──▶ ┆ ──▶ upstreams
        ◀─ hit ─── │ wall│     ┆ air gap
                   two faces   only misses cross
```

1. **Every request rides its own lane.** One dashed rail per ecosystem, from the
   client chip to the wall and (fainter) on to its upstream. A request now has a
   path instead of drifting across open space.
2. **The wall has two different faces.** Green hairline on the near face (serves
   hits), blue on the far face (fetches misses). A hit flashes the near face,
   prints `0 ms`, and returns in **240 ms** — about a third of the 680 ms it took
   to arrive, so "instant" is a *felt* difference, not a label. A miss makes a
   **slit** open in the wall on that one lane, crosses, and comes back as a single
   fat **stream** dot that lights a CAS tile.
3. **The air gap is a seam**, not a decorative line: a vertical hairline with the
   label set along it, sitting between the wall and the upstreams. Everything that
   crosses it is countable at a glance — that *is* the pitch.
4. **Single-flight is shown.** Sometimes two clients ask for the same artifact:
   the follower parks at the wall and pulses, one upstream fetch happens, and
   **both** clients are served from it. Labelled `single-flight · 2 clients, 1 fetch`.
5. **The air-gap beat.** Every 24 s the scene goes `OFFLINE=1` for 9 s: upstream
   chips, upstream rails and the miss key dim, the seam turns solid red, misses
   die at the seam with a red return — and **hits keep flowing at full speed**.
6. **Calmer.** ~70 % hits, ≤ 4 requests in flight, 940–1800 ms spawn cadence.
   Scripted intro (hit → miss → single-flight) then ambient, so a first-time
   viewer sees the whole story in the first ~6 s.

Kept from before: IBM Plex Mono, 3 px radii, 1 px borders, uppercase mono labels,
the blue accent / green HIT OKLCH palette, the CAS grid, the checkpoint beat, the
readout, the typed command line.

---

## 2. Per-request lifecycle (what the JS does)

1. Dot leaves the client chip's **right edge** (never its label) → wall near face,
   680 ms.
2. **Hit (~70 %)** — near face flashes green, `0 ms` tag, a stored CAS tile pings,
   green dot returns in 240 ms, client chip goes green (`.served`).
3. **Miss (~30 %)** — slit opens on that lane, dot passes *behind* the wall
   (200 ms) → upstream (620 ms, chip `.busy`) → one fat stream dot back (900 ms)
   → a CAS tile lights blue (`.lit`) → client served (300 ms).
4. **Single-flight** — as (3), but two clients are served from the one stream.
5. **Checkpoint** — every 8 fresh tiles the wall pulses green (`.commit`), the
   version bumps, all `.lit` tiles become `.committed`; a full grid clears and
   refills (delta → checkpoint → new delta).
6. **Offline beat** — `.scene.offline` for 9 s every 24 s; misses fail at the seam
   (`.rd-fail`, chip `.failed`), hits are unaffected.

---

## 3. DOM / class contract

Structure (full markup in `landing-hero.html`):

```
.hero.night
├── .hero-copy      eyebrow · h1 · .hero-sub · .hero-cmd (#typed) · .hero-cta · .hero-kpis (#kpi-hitrate)
└── .scene[aria-hidden]
    ├── .upzone                     tint beyond the air gap
    ├── svg.rails > line.rail ×6, g.rails-up > line.rail ×6
    ├── .gapline · .gaplabel        the air-gap seam
    ├── .wall                       the cache  (.wall-h, .cas#cas, .wall-ckpt)
    ├── .flow                       JS appends dots/markers here (z below .wall)
    ├── .clients > .fnode.fclient.e-<eco>[data-node="c-<eco>"] ×6
    ├── .ups     > .fnode.fup.e-<eco>[data-node="u-<eco>"] ×6
    └── .scene-key                  legend + .scene-status .label
```

- **Lane order is the DOM order of `.clients` children.** The JS derives the
  ecosystem list from it, and writes rail endpoints from measurement — add or
  remove an ecosystem by editing markup (chip + upstream + one `line.rail` in each
  group). No CSS or JS edit needed.
- **Eco colour is one custom property.** `.e-npm { --h: … }`; chips, dots and
  streams all read `var(--h)`. New ecosystem = one line in the `.e-*` block.
- **State classes the JS toggles** (all styled in CSS, marked `[JS]` there):
  `.cas-tile.lit / .committed / .ping`, `.wall.commit`, `.fnode.served / .busy /
  .failed`, `.scene.offline`, and the transient `.face`, `.slit`, `.sealed`,
  `.tag.tag-ms`, `.tag.tag-sf` markers.
- **JS writes only `transform` (and left/top on transient markers).** Everything
  else is a class. No inline `<style>`, no inline `<script>`, no handlers.

### Tunables — `CFG` at the top of `landing-hero.js`

`tiles 22 · litPerCheckpoint 8 · hitRate 0.70 · sfChance 0.14 · maxLive 4 ·
spawn 940 + 0–860 ms · legIn 680 · legHit 240 · legThrough 200 · legOut 620 ·
legStream 900 · legServe 300 · offlineEvery 24000 · offlineFor 9000`

---

## 4. One bug worth knowing about (it bit the old engine too)

`getBoundingClientRect()` returns **screen** pixels. If any ancestor is scaled —
browser zoom, a `transform: scale()` wrapper, an embed — those numbers no longer
match the CSS pixels a `transform` animates in, and every dot rides a *shrunken
copy* of the layout: the drift grows the further down the scene you look.

`measure()` now divides all measured offsets by
`scene.getBoundingClientRect().width / scene.offsetWidth`, so geometry is
zoom-independent. Keep that if you refactor.

---

## 5. Constraint compliance (HANDOFF §5)

| # | Constraint | Status |
|---|---|---|
| 1 | Strict CSP (`style-src 'self'; script-src 'self'`) | ✅ separate files; no inline style/script/handlers; JS sets `transform` only |
| 2 | Self-contained / air-gap friendly | ✅ no CDN, no remote fonts, no fetches in production; font `local()` + `/fonts/IBMPlexMono.woff2` |
| 3 | Theme-aware | ✅ 100 % token-based; the scene sits in `.night`, so it resolves in both themes with no extra rules |
| 4 | Reduced motion | ✅ no loops start; static end-state: 14 committed + 3 lit tiles, `ckpt v13`, `93 %`, and the command line printed once |
| 5 | Responsive, no h-scroll | ✅ ≤ 980 px hero stacks (scene 420 px); ≤ 720 px hides upstreams + upstream rails + seam, wall moves right, scene 340 px |
| 6 | Decorative / accessible | ✅ `.scene[aria-hidden="true"]`; every claim in it is also in the copy |
| 7 | Truthful | ✅ real upstream hostnames, real six ecosystems, real HIT/MISS/single-flight/checkpoint/`OFFLINE=1` semantics |
| 8 | Typed command line survives | ✅ same `#typed` hook, same four real commands |

---

## 6. Landing it

1. Paste `landing-hero.html` over the current hero `<section>` in
   `webui/console/public/landing.html`.
2. In `landing.css`, delete the old `.scene …` block (from `.scene {` through the
   scene's media queries) and paste `landing-hero.css` in its place. Tokens at the
   top of the file are unchanged — the redesign adds no new colours except
   `--bad` / `--warn`, which already exist.
3. In `landing.js`, delete the old scene IIFE **and** the typed-command block, and
   paste `landing-hero.js` (it contains both).
4. Check: dark + light, 1440 / 1024 / 768 / 390 widths, `prefers-reduced-motion`,
   and browser zoom at 80 % / 150 % (see §4).

## 7. Open questions for the product owner

- The offline beat currently fires every 24 s. Should it be rarer (say 45 s) so the
  online story dominates, or is the air gap worth the airtime?
- Should the wall show the artifact name on the lane during a miss (e.g.
  `library/nginx:1.27 · 42 MB`), or does that make it too busy?
- Light theme: the scene keeps the forced night palette. Confirm that's still the
  intent for the hero band.
