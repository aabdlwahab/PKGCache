---
target: go/internal/web/dist
total_score: 26
max_score: 40
na_heuristics: 
p0_count: 3
p1_count: 11
timestamp: 2026-07-31T08-52-26Z
slug: go-internal-web-dist
---
Method: dual-agent (A: design review, B: detector + code evidence) plus an independent technical audit pass.
Browser visualization unavailable: no browser automation or Playwright exposed. No overlays, no screenshots.
All contrast figures computed from token definitions (OKLCH -> sRGB -> WCAG relative luminance).

Surfaces: go/internal/web/dist/ -- landing (Persuade), tutorial (Read), console (Operate).

## Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 2 | No loading state in any of six console views; regions render their EMPTY state during flight ("Nothing cached yet.") |
| 2 | Match System / Real World | 4 | OUTCOME_LABEL (format.js:83-89); "the last bars are the eviction queue" |
| 3 | User Control and Freedom | 1 | No undo; rail has no Escape/focus management; project switch destroys in-progress forms |
| 4 | Consistency and Standards | 3 | Revoke/Remove are danger-styled; Evict, the most destructive button, is plain default |
| 5 | Error Prevention | 1 | Five irreversible actions, zero confirmations |
| 6 | Recognition Rather Than Recall | 3 | Connect generates live start command; Export digest is free text from a truncated table |
| 7 | Flexibility and Efficiency | 2 | Zero keyboard shortcuts, no pagination, no bulk actions |
| 8 | Aesthetic and Minimalist Design | 3 | Admin stacks six unrelated panels; six equal-weight KPIs, no primary |
| 9 | Error Recovery | 3 | Superb server-state copy; all runtime failures funnel to one overwritable global string |
| 10 | Help and Documentation | 4 | Full tutorial surface, documented Connect/tutorial split, real-failure-mode FAQ |
| **Total** | | **26/40** | **Acceptable (65%)** |

Heuristic 1 lowered from the reviewer's 3: views do not render blank while loading, they render a positive
claim that no data exists.

## Audit Health Score

| # | Dimension | Score | Key Finding |
|---|-----------|-------|-------------|
| 1 | Accessibility | 2 | Form controls/buttons have no perceivable boundary (1.09-1.36:1 vs 3:1 required) |
| 2 | Performance | 2 | Full region rebuild once per 64 KiB of every download |
| 3 | Theming | 3 | Strong dual-palette, no FOUC, theme-reactive charts; seven eco hues have no light variant |
| 4 | Responsive | 2 | Horizontal overflow at 320/375px; entire six-view SPA has ONE media query |
| 5 | Implementation Integrity | 3 | Every copy claim verified true against Go handlers; ~40 lines dead CSS |
| **Total** | | **12/20** | **Acceptable** |

## Design Specificity Verdict

Landing: genuinely specific. The hero is a literal simulation of the request pipeline -- cache wall serving
hits from its left face, misses opening a slit, single-flight as "two machines asking, one download",
misses dying at the seam in offline mode. Rails measured from real DOM geometry.

Console: generic composition wearing outstanding domain copy. Topbar + KPI row + auto-fit panel grid +
fixed drawer is the default admin shell since 2015. Specificity lives entirely in vocabulary and colour
semantics. Only Sources lets the domain drive composition.

Deterministic scan: exit 2, 10 findings -- 8 side-tab, 1 gradient-text (landing.css:937), 1 em-dash
density advisory. All low-severity stylistic. The detector missed both P0 performance defects; they live
in JS event plumbing it does not read.

## Priority Issues

[P0] Five irreversible operations behind a single unconfirmed click
  admin.js:150-161 (Evict, Collect), sources.js:70-73 (Go offline), connect.js:290 (Revoke),
  sources.js:196 (Remove upstream). Rollback/Delete account/Delete project DO confirm (admin.js:346),
  so the absence reads as a safety guarantee. Go offline fails every miss fleet-wide on one click.
  Fix: confirm() + danger styling, using the proven admin.js:346 pattern.

[P0] Full region rebuild once per 64 KiB of every download
  store.js:201-209; server inflight.go:68,491. copyBufSize = 64<<10, one fetch.progress per chunk,
  no throttle. Each frame wakes three subscribers doing full replaceChildren. 250MB layer = ~4000
  rebuild cycles. sse.go:41-45 answers a slow consumer by closing the stream, so the UI cost causes
  the data loss. Fix: coalesce progress frames into one rAF/250ms flush.

[P0] Every completed fetch triggers five control-plane requests
  store.js:218 -> store.js:146-152. Promise.all of stats/endpoints/tokens/upstreams/snapshots.
  A uv sync over 300 wheels = ~1500 extra requests. Four of five cannot change from a cache fill.
  Fix: debounce, and refresh only stats on fetch.done.

[P1] Form controls and buttons have no perceivable boundary
  console.css:102-110, 84-99. Input border 1.35:1 light / 1.36:1 dark; fill 1.09/1.12:1;
  button border 1.27:1. WCAG 1.4.11 requires 3:1.

[P1] Console scrolls sideways on every phone
  console.css:143, 352. minmax(360px,1fr) with no min() guard; 339px content box at 375px.

[P1] The safe path returns no information
  admin.js:158. Dry run reports only "started". The result EXISTS -- service.go:308 logs
  scanned/evicted_entries/reclaimed_bytes to a durable log returned by GET /api/v1/jobs/{id}.
  The console never calls it.

[P1] Cache inventory has no pagination; api.js:66 already accepts page
  cache.js:210. "48,312 matching" then page one forever.

[P1] .night leaves four tokens unoverridden
  landing.css:6-13 overrides surfaces and text but not --ok/--bad/--warn/--accent-ink, so light theme
  puts light-theme status colours and white ink on a pinned near-black band. White on accent = 2.65:1.

[P1] Seven landing ecosystem hues have no light variant
  landing.css:794-799, tutorial.css:340. Measured on light panel: 2.30-3.98:1. The chart palette ships
  per-theme values; the marketing cards never got the same pass.

[P1] Activity rail: no Escape, no focus management
  chrome.js:46-70. Fixed drawer at min(380px,92vw); focus stays on the toggle; rail is the last child
  of #root so its Close button is behind the whole view. aria-expanded/aria-controls ARE wired.

[P1] Chart SVGs are role="img" with no accessible name
  charts.js:98-105. Five charts. Table toggle is the mitigation but reverts every 60s (overview.js:68).

[P1] Three Cache filter controls have no accessible name
  cache.js:22-40 bypasses field(), the wrapper every other console form uses.

[P1] Invisible landing links stay in the tab order
  landing.css:394-406. Non-current acts hidden with opacity:0 + pointer-events:none only.

[P1] Pinned landing stage clips its own content at 200% text zoom
  landing.css:385-402. Absolutely positioned, viewport-height, overflow:hidden, no scroll escape.

[P1] .pill.warning fails text contrast in light mode
  console.css:271, 4.34:1 at 11px. This is the offline-state indicator.

## Persona Red Flags

Sam (screen reader + keyboard): no skip link on any surface; rail opens without moving focus and no
keydown handler exists anywhere in console/; three unlabeled filter controls; five unnamed charts;
all four landing beats read in sequence as contradictory articles with two invisible focusable links.

Riley (stress tester): 10,000 artifacts = page one forever; URL and Error columns truncate without
title; sticky th inside a container that never scrolls vertically; project switch silently destroys
a pasted lockfile via remount().

Casey (mobile): sideways scroll on load; rail covers 92% with no backdrop/Escape/swipe; landing.css:695
display:none's three paragraphs of product explanation below 720px while keeping the 460vh scroll.

## Minor Observations

- The specified font is not shipped: tokens.css:15 requests /fonts/IBMPlexMono.woff2; dist/fonts/ holds
  only a README that still references the deleted Vite console's build path.
- --series-6 is the one series token dark mode forgot to override (identical at tokens.css:81 and :111).
- .grid defined twice in one file: layout (console.css:141) and SVG gridlines (console.css:325).
- charts.barRows silently ignores the height option admin.js:189 passes it.
- Two storage-key conventions: pkgreg-project and pcc_theme (leftover from the retired React console).
- Four different CTA labels all point at /tutorial.

## Questions to Consider

1. The landing page renders this system's pipeline as a live readable diagram. Why does the console,
   which has the real data as live SSE frames, render it as six unrelated panels of bars?
2. What is the console's single primary action, and can you point to it? If there isn't one, why does
   it open on Overview rather than on what the instance's state most needs?
3. Why does a destructive operation need a modal, when this system's core primitive is a checkpoint?
   Evict-then-undo-for-30s would use the product's own differentiator as its safety model.
