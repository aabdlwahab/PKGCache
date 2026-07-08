# Component Handoff — Checkpoint & transfer widget

A self-contained operator widget for the **pkgcache** air-gapped package cache. It does two jobs:
**(1) Checkpoint** — version/commit the current cache tree; **(2) Shuttle transfer** — export a
delta to, or import from, a removable "shuttle" drive that physically crosses the air gap. Both run
as one-at-a-time jobs whose output streams into an inline terminal console.

## Files
- `Checkpoint Transfer Widget.dc.html` — **runnable, isolated reference** of just this widget, with
  its own theme tokens and a thin demo harness. Open it in a browser to see/feel every state.
  *(It's a "Design Component" prototype built on a `support.js` runtime — read it for the exact
  markup, inline styles, and logic, but re-express it in your stack; don't port the runtime.)*
- This `README.md`.

> The harness row at the bottom (`HARNESS` — theme / skin / +1 uncommitted) is **not part of the
> widget** — it only exists to exercise states in isolation. Drop it on integration.

## Fidelity
High-fidelity. Colors (OKLCH), type (IBM Plex), spacing, the air-gap diagram, the job console, and
all state transitions are final. Recreate exactly. The widget inherits theme/skin from its host (see
Tokens); it does not own a theme.

---

## Layout
A panel (`--panel` bg, 1px `--line` border, `--radius`, `--shadow`) with a header
(**"Checkpoint & transfer"** title + a right-aligned job hint) and a body of **two inset cards**
followed by the **job console** (conditional).

### Header
- Title: `--title` font, 12px/600, `letter-spacing: var(--tspace)`, `text-transform: var(--titlecase)`,
  color `--fg2`.
- Right hint (`--mono`, 11px, `--muted`): `one job at a time` → `job running…` while a job runs.

### Card 1 — Checkpoint
Inset card (`--inset` bg, 1px `--line`, `--radius-sm`, padding 13×14).
- **Header row:** `⎘ Checkpoint` (mono, 11.5px/600, `--accent`) · caption "quiesce · hash · commit
  the cache tree" (`--muted`) · spacer · **state pill** (right-aligned, mono 11px/600, `white-space:nowrap`,
  pill radius 999px):
  - uncommitted artifacts present → `+N uncommitted`, color `--warn` on `--warn-bg`
  - none → `all committed`, color `--ok` on `--ok-bg`
- **Input + button row** (`display:flex; gap:8px`): message text input (`--panel` bg, mono 12.5px) +
  primary button `⎘ Checkpoint` (accent bg, `--accent-ink` text).
- **Footer line** (mono 10.5px, `--muted`): a small `--ok` dot + `last: HEAD <sha> · <time> ago`.

### Card 2 — Shuttle transfer
Inset card, same chrome.
- **Header row:** `⇄ Shuttle transfer` (mono 11.5px/600, `--fg2`) + caption "move artifacts across
  the air gap".
- **Air-gap diagram** (`display:flex; align-items:center; gap:8px`):
  - Left node — `--panel` tile (`--radius-sm`), centered: `▣ pkgcache` (mono 12px, `--fg`) over
    "this cache" (10px, `--muted`). `flex:1`.
  - Center connector — `flex:none; width:84px`, column: uppercase "AIR GAP" label (mono 9.5px,
    `--muted`, `letter-spacing:.08em`, `nowrap`); a full-width `2px dashed var(--line2)` rule; then
    the **direction arrow** (15px) that reflects job state:
    - idle → `⇄`, color `--muted`
    - export running → `↦`, color `--accent`
    - import running → `↤`, color `--accent`
  - Right node — `▤ shuttle` over "removable drive". `flex:1`.
- **Drive input** — single shared path field (label `drive` + input, `--panel` bg, height 34px),
  default `/media/shuttle`. **Both Export and Import operate on this one path.**
- **Buttons row** (`gap:8px`): two equal-width (`flex:1`) ghost buttons — `↥ Export delta` and
  `↧ Import`.

### Job console (conditional — visible only while/after a job)
Inset terminal card.
- Header: status dot (color by status; pulses while `running`) + `$ pkgcache <action>` (mono 12px)
  + **status pill** (`running` → `--warn`/`--warn-bg`; `done` → `--ok`/`--ok-bg`; `failed` →
  `--bad`/`--bad-bg`) + spacer + `close` button.
- Body: a `<pre>` (mono 11.5px, `line-height:1.55`, `white-space:pre-wrap`, max-height 200px, scroll)
  streaming the job's log lines, with a blinking `▋` accent cursor appended while `running`.

---

## Tokens (inherited from host — do not redefine per-widget in production)
The reference file embeds these so it runs standalone; in the app they come from the host theme.

**Theme (`[data-theme]`):** `--bg --panel --panel2 --inset --line --line2 --fg --fg2 --muted
--ok --bad --warn --ok-bg --bad-bg --warn-bg --shadow`. Dark + light values are in the reference
file's `<style>`; `--ok/--bad/--warn` carry the semantic states.

**Skin (`[data-skin]`):** `--ui --title --mono` (IBM Plex Mono/Sans), `--radius --radius-sm`,
`--tspace --titlecase`, and `--accent` / `--accent-ink`. Two skins: `console` (mono, sharp 3px radii,
uppercase titles, green accent) and `dashboard` (sans, soft 10px radii, blue accent). The widget
reads these — it must look correct in all four theme×skin combos.

**Button recipes** (from the reference's `renderVals`):
- primary: `background:var(--accent); color:var(--accent-ink); border:0; radius:var(--radius-sm);`
  600 weight, 7×14 padding.
- ghost: `background:transparent; color:var(--fg2); border:1px solid var(--line2);` 500 weight.
- transfer buttons = ghost + `flex:1` + centered icon/label.
- All buttons: `disabled` while a job runs → `opacity:.5; cursor:not-allowed`.

---

## Behavior & State
- **Single job at a time.** Any action is a no-op while `job.status === 'running'`; all action
  buttons render disabled.
- **Checkpoint:** empty/whitespace message → do **not** submit; instead show an immediate `failed`
  job in the console (`✗ a checkpoint message is required`). On success the job streams, then HEAD
  advances (new short sha, date `now`) and the **uncommitted count resets to 0** (state pill →
  `all committed`). Clear the input on submit.
- **Export / Import:** require a non-empty drive path; stream the job; the air-gap arrow shows
  direction while running. Import increases the cached/uncommitted count (new artifacts arrived);
  export does not change it.
- **Console close** clears the job (`job → null`), hiding the console.

### Local state
`pendingNew` (int — uncommitted artifact count, drives the pill), `head` (short sha) + `headDate`,
`ckmsg`, `exdrive` (drive path), and `job` (`{action,title,status:'running'|'done'|'failed',log,params}`
or `null`). `busy = job?.status === 'running'`.

## API wiring (replace the simulated job stream)
The reference fakes the streamed log in `jobScript(action, params)`. In production:
- **Submit:** `POST /api/jobs` with `{ action, ...params }` where action ∈
  `checkpoint{message}` · `export{drive}` · `import{drive}`.
- **Stream:** poll `GET /api/jobs/<id>` (or subscribe) while `running`; append returned log lines to
  the console; on terminal status set `done`/`failed`.
- **Derived state after success:** checkpoint → refresh HEAD + recompute uncommitted (manifest count
  minus last-checkpoint count); import → refresh manifests/uncommitted.
- The `last: HEAD <sha>` line comes from the latest commit in `GET /api/history`.

## Glyphs (Unicode — no icon font)
`⎘` checkpoint · `⇄ ↦ ↤` shuttle direction · `▣ ▤` nodes · `↥ ↧` export/import · `▋` console cursor.
