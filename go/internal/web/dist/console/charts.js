/* Inline-SVG chart primitives. No charting library, consistent with the rest of the
 * console needing nothing installed.
 *
 * Three rules are enforced here rather than left to each view:
 *
 *   1. Every chart ships a table view. On the light surface three of the six series
 *      colours measure below 3:1 against the panel, so colour alone cannot carry
 *      identity. Building the toggle into the component means no chart can ship
 *      without its relief.
 *
 *   2. Absent buckets are drawn as gaps, never as zero. A dropped flush window means
 *      "not recorded"; plotting zero there would assert that no traffic happened.
 *
 *   3. No text inside the plot. The SVG stretches to the panel width with
 *      preserveAspectRatio="none" so a chart is a fixed height at any width — which
 *      would smear glyphs horizontally. Axis labels are HTML beside the plot, so they
 *      stay crisp and inherit the page's font.
 */

import { el, svg, table } from "./dom.js";

// The plot's own coordinate space. Deliberately unitless: x runs 0–1000 and y runs
// 0–100 downward, and CSS decides what that becomes in pixels.
const W = 1000;
const H = 100;

/* Which representation each figure is showing, keyed by title and held outside the
 * figure itself.
 *
 * It used to be a closure local, which meant a view that refreshes — Overview refetches
 * every 60 seconds and rebuilds the whole figure — silently threw the reader back to the
 * chart. That is not a cosmetic reset: the table is the documented relief for the three
 * light-mode series colours that fall below 3:1, so an accessibility affordance was
 * being undone on a timer. Titles are stable and unique within a view, so they are the
 * natural key. */
const preferredView = new Map();

/** Wrap a plot in a figure with a caption, legend and a table alternative. */
export function figure(title, { note, legend, rows, columns } = {}, plot) {
  const hasTable = Boolean(columns && rows);
  let showing = hasTable ? preferredView.get(title) || "chart" : "chart";

  // role="img" without a name announces as an unnamed graphic. The caption already says
  // what this is, so it becomes the accessible name — and the reader is told a table
  // exists, since discovering the toggle otherwise depends on seeing it.
  if (plot instanceof SVGElement) {
    plot.setAttribute("aria-label", hasTable
      ? `${title} — chart. A table view of the same data is available.`
      : `${title} — chart.`);
  }

  const asTable = () => table(columns || [], rows || [], { empty: "No data." });
  const body = el("div", { class: "chart-body" }, showing === "table" ? asTable() : plot);

  const toggle = el("button", {
    class: "btn ghost small",
    text: showing === "table" ? "Chart" : "Table",
    "aria-pressed": String(showing === "table"),
    onclick: () => {
      showing = showing === "chart" ? "table" : "chart";
      preferredView.set(title, showing);
      toggle.textContent = showing === "chart" ? "Table" : "Chart";
      toggle.setAttribute("aria-pressed", String(showing === "table"));
      body.replaceChildren(showing === "chart" ? plot : asTable());
    },
  });

  return el(
    "figure",
    { class: "chart" },
    el(
      "figcaption",
      { class: "chart-head" },
      el("span", { class: "chart-title", text: title }),
      note ? el("span", { class: "note", text: note }) : null,
      hasTable ? toggle : null,
    ),
    legend ? renderLegend(legend) : null,
    body,
  );
}

/** A legend is present whenever there are two or more series — identity is never
 *  left to colour alone. A single-series chart needs none; the title names it. */
function renderLegend(entries) {
  if (entries.length < 2) return null;
  return el(
    "ul",
    { class: "legend" },
    entries.map((entry) =>
      el(
        "li",
        {},
        el("span", { class: "swatch", style: `background:${entry.color}` }),
        el("span", { text: entry.label }),
      ),
    ),
  );
}

export function emptyPlot(message = "No data in this window.") {
  return el("p", { class: "empty", text: message });
}

/** The frame: an HTML y-axis, the stretched plot, and an HTML x-axis under it. */
function frame(peak, format, marks, xLabels, height) {
  const ticks = [];
  for (let i = 4; i >= 0; i--) ticks.push(el("span", { text: format((peak * i) / 4) }));

  return el(
    "div",
    { class: "plot-frame", style: `--plot-h:${height}px` },
    el("div", { class: "y-axis" }, ticks),
    el(
      "div",
      { class: "plot-col" },
      el(
        "div",
        { class: "plot" },
        svg(
          "svg",
          {
            viewBox: `0 0 ${W} ${H}`,
            preserveAspectRatio: "none",
            role: "img",
            focusable: "false",
          },
          [0, 25, 50, 75, 100].map((y) =>
            svg("line", { class: y === 100 ? "axis" : "grid", x1: 0, x2: W, y1: y, y2: y }),
          ),
          marks,
        ),
      ),
      xLabels.length ? el("div", { class: "x-axis" }, xLabels.map((l) => el("span", { text: l }))) : null,
    ),
  );
}

/* ---- stacked column chart ------------------------------------------------ */

/**
 * Volume and composition on one axis: the column height is the total, the segments
 * are the split. Two measures without a second y-scale, which is the mistake this
 * shape exists to avoid.
 *
 * @param buckets  [{ at: Date, parts: [{ key, value, color, label }] }] — pre-sorted
 * @param step     bucket width in seconds, so gaps can be detected
 */
export function stackedColumns(buckets, { step, height = 200, format = String, tick } = {}) {
  if (!buckets.length) return emptyPlot();

  const peak = Math.max(...buckets.map((b) => b.parts.reduce((sum, p) => sum + p.value, 0)), 1);

  // Slots are laid out on the time axis, not on the array index, so a missing bucket
  // leaves a real hole rather than closing up and misdating everything after it.
  const first = buckets[0].at.getTime();
  const last = buckets[buckets.length - 1].at.getTime();
  const slots = Math.max(1, Math.round((last - first) / (step * 1000)) + 1);
  const slotW = W / slots;
  const barW = Math.max(2, Math.min(slotW * 0.72, 40));

  const marks = buckets.map((bucket) => {
    const index = Math.round((bucket.at.getTime() - first) / (step * 1000));
    const x = index * slotW + (slotW - barW) / 2;
    let y = H;
    const total = bucket.parts.reduce((sum, part) => sum + part.value, 0);

    const segments = bucket.parts
      .filter((part) => part.value > 0)
      .map((part) => {
        const h = (part.value / peak) * H;
        y -= h;
        // A hairline surface gap between stacked segments keeps adjacent fills
        // legible without a border, which would double every edge in the chart.
        const gap = h > 1.5 ? 0.5 : 0;
        return svg(
          "rect",
          { x, y: y + gap, width: barW, height: Math.max(0.4, h - gap * 2), fill: part.color },
          svg("title", {}, `${part.label}: ${format(part.value)}`),
        );
      });

    return svg("g", {}, segments, svg("title", {}, `${format(total)} total`));
  });

  return frame(peak, format, marks, timeLabels(buckets.map((b) => b.at), tick), height);
}

/* ---- line ---------------------------------------------------------------- */

/**
 * A single measure over time. Series with a gap wider than one step are broken into
 * separate paths rather than joined by a line that would invent the missing readings.
 */
export function lineChart(series, { step, height = 200, format = String, reference, tick } = {}) {
  const points = series.filter((point) => Number.isFinite(point.value));
  if (points.length < 2) return emptyPlot("Not enough history yet.");

  const first = points[0].at.getTime();
  const last = points[points.length - 1].at.getTime();
  const span = Math.max(1, last - first);
  const peak = Math.max(...points.map((p) => p.value), reference?.value || 0, 1);

  const x = (at) => ((at.getTime() - first) / span) * W;
  const y = (value) => H - (value / peak) * H;

  const segments = [];
  let run = [];
  for (let i = 0; i < points.length; i++) {
    // 1.5 steps of tolerance: sampling jitter is not a gap, a skipped sample is.
    if (i > 0 && points[i].at - points[i - 1].at > step * 1000 * 1.5) {
      segments.push(run);
      run = [];
    }
    run.push(points[i]);
  }
  segments.push(run);

  const marks = segments
    .filter((run) => run.length > 1)
    .map((run) =>
      svg("path", {
        class: "line",
        // Without this the non-uniform scale would make the stroke thick horizontally
        // and thin vertically.
        "vector-effect": "non-scaling-stroke",
        d: run.map((p, i) => `${i ? "L" : "M"}${x(p.at).toFixed(2)},${y(p.value).toFixed(2)}`).join(" "),
      }),
    );

  if (reference) {
    marks.unshift(
      svg("line", {
        class: "reference",
        "vector-effect": "non-scaling-stroke",
        x1: 0, x2: W, y1: y(reference.value), y2: y(reference.value),
      }),
    );
  }

  return frame(peak, format, marks, timeLabels(points.map((p) => p.at), tick), height);
}

/** First, middle and last stamps. Three labels read cleanly at any panel width; a
 *  label per bucket would collide the moment the window got long. */
function timeLabels(times, tick) {
  if (!tick || times.length < 2) return [];
  const last = times[times.length - 1];
  // With only two stamps the "middle" is the last one, and repeating it reads as a
  // chart that stopped moving.
  if (times.length < 3) return [tick(times[0]), tick(last)];
  return [tick(times[0]), tick(times[Math.floor(times.length / 2)]), tick(last)];
}

/* ---- horizontal bars ----------------------------------------------------- */

/** Magnitude across a handful of named things. Sorted descending, direct-labelled —
 *  which is also what satisfies the light-mode contrast relief. */
export function barRows(rows, { format = String } = {}) {
  if (!rows.length) return emptyPlot("Nothing cached yet.");
  const peak = Math.max(...rows.map((row) => row.value), 1);
  return el(
    "div",
    { class: "bars" },
    rows.map((row) =>
      el(
        "div",
        { class: "bar-row" },
        el("span", { class: "bar-label", text: row.label, title: row.label }),
        el(
          "span",
          { class: "bar-track" },
          // A zero draws nothing. A one-pixel sliver would read as "a little", and the
          // difference between none and a little is the whole point of these bars.
          row.value > 0
            ? el("span", {
                class: "bar-fill",
                style: `width:${Math.max(0.5, (row.value / peak) * 100)}%;background:${row.color}`,
              })
            : null,
        ),
        el("span", { class: "bar-value", text: format(row.value) }),
      ),
    ),
  );
}

/** A segmented bar for a composition within one row (hit vs miss, say). */
export function segmentedBar(parts, { format = String } = {}) {
  const total = parts.reduce((sum, part) => sum + part.value, 0);
  if (!total) return el("span", { class: "note", text: "—" });
  return el(
    "span",
    { class: "segbar", title: parts.map((p) => `${p.label}: ${format(p.value)}`).join("  ") },
    parts
      .filter((part) => part.value > 0)
      .map((part) =>
        el("span", {
          class: "segbar-part",
          style: `width:${(part.value / total) * 100}%;background:${part.color}`,
        }),
      ),
  );
}

/* ---- meter --------------------------------------------------------------- */

/**
 * A single headline with a bar. Not a chart — one number does not need axes.
 * `threshold` marks the level the system will actually act on, so the gauge is read
 * against the eviction floor rather than an arbitrary "looks low".
 */
export function meter(used, total, { threshold, format = String } = {}) {
  const share = total > 0 ? Math.min(1, used / total) : 0;
  const free = total - used;
  let status = "good";
  if (threshold && free < threshold) status = "critical";
  else if (threshold && free < threshold * 2) status = "warning";
  else if (share > 0.9) status = "warning";

  return el(
    "div",
    { class: "meter-block" },
    el(
      "div",
      { class: `meter status-${status}` },
      el("div", { class: "meter-fill", style: `width:${share * 100}%` }),
      threshold && total
        ? el("div", {
            class: "meter-threshold",
            style: `left:${Math.min(100, ((total - threshold) / total) * 100)}%`,
            title: `eviction floor: ${format(threshold)} free`,
          })
        : null,
    ),
    el(
      "div",
      { class: "meter-legend" },
      // Status never rests on colour: the words are the encoding, the colour agrees.
      el("span", { class: `pill ${status}`, text: statusWord(status) }),
      el("span", { class: "note", text: `${format(used)} used of ${format(total)}` }),
      el("span", { class: "note", text: `${format(free)} free` }),
    ),
  );
}

function statusWord(status) {
  return { good: "healthy", warning: "filling", critical: "low space" }[status] || status;
}

/** Bucket a raw /stats/series payload into the shape stackedColumns wants. */
export function toBuckets(points, keyOf, colorOf, labelOf) {
  const byTime = new Map();
  for (const point of points) {
    const at = new Date(point.bucket);
    const stamp = at.getTime();
    if (!byTime.has(stamp)) byTime.set(stamp, { at, parts: [] });
    byTime.get(stamp).parts.push({
      key: keyOf(point),
      value: point.count,
      color: colorOf(point),
      label: labelOf(point),
    });
  }
  return [...byTime.values()].sort((a, b) => a.at - b.at);
}
