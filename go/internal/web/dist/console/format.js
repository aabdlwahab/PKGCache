/* Value formatting. Every number a person reads passes through here, so units and
 * rounding stay consistent across every view. */

import * as store from "./store.js";

const UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

export function bytes(value) {
  const n = Number(value);
  if (!Number.isFinite(n) || n <= 0) return "0 B";
  const unit = Math.min(Math.floor(Math.log(n) / Math.log(1024)), UNITS.length - 1);
  const scaled = n / 1024 ** unit;
  // One decimal above KiB, none for raw bytes: "1.4 GiB" is useful, "1462.3 B" is not.
  return `${scaled.toFixed(unit === 0 ? 0 : scaled >= 100 ? 0 : 1)} ${UNITS[unit]}`;
}

export function count(value) {
  const n = Number(value) || 0;
  return n.toLocaleString();
}

export function percent(part, whole) {
  if (!whole) return "—";
  return `${Math.round((part / whole) * 100)}%`;
}

export function ago(value) {
  if (!value) return "—";
  const then = Date.parse(value);
  if (Number.isNaN(then)) return "—";
  return `${spell(Math.max(0, Math.round((Date.now() - then) / 1000)))} ago`;
}

/** Relative time in either direction. An expiry a week out has to read "in 7d", not
 *  "0s ago" — which is what a past-only formatter produces for every future date. */
export function when(value) {
  if (!value) return "—";
  const at = Date.parse(value);
  if (Number.isNaN(at)) return "—";
  const seconds = Math.round((at - Date.now()) / 1000);
  return seconds >= 0 ? `in ${spell(seconds)}` : `${spell(-seconds)} ago`;
}

function spell(seconds) {
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.floor(seconds / 86400)}d`;
}

export function duration(ms) {
  const n = Number(ms) || 0;
  if (n < 1000) return `${Math.round(n)}ms`;
  if (n < 60000) return `${(n / 1000).toFixed(1)}s`;
  return `${Math.floor(n / 60000)}m${Math.round((n % 60000) / 1000)}s`;
}

/** Clock time for an axis tick. Deliberately not a date: every chart in this console
 *  spans hours or days, and the range is stated in the panel note. */
export function clock(value, span) {
  const at = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(at.getTime())) return "";
  if (span >= 86400) return at.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  return at.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

/** Digests are 64 hex characters and never need to be read in full to be compared. */
export function digest(value) {
  const s = String(value || "");
  return s.length > 16 ? `${s.slice(0, 12)}…` : s || "—";
}

/** How many --series-N slots tokens.css defines. Past the last one an ecosystem is drawn
 *  in the neutral rather than handed a slot that is already somebody else's. */
const SERIES_SLOTS = 8;

/** Colour follows the entity, never its rank: a filter that removes npm must not
 *  repaint oci with npm's colour.
 *
 *  The slot is the ecosystem's place among the registered ones, which the server lists
 *  in ID order. It used to be a hand-kept list here, which is exactly the per-ecosystem
 *  table AGENTS.md calls a defect: gomod was registered and never added, so it drew in
 *  the "unknown" grey on every chart. Reading the descriptors means an ecosystem added
 *  on the server has a colour with no change here — and because the index is taken
 *  over every registered ecosystem rather than over the rows a chart happens to draw,
 *  filtering one out still cannot move another. */
export function ecoColor(eco) {
  const index = store.state.ecosystems.findIndex((descriptor) => descriptor.id === eco);
  return index === -1 || index >= SERIES_SLOTS ? "var(--muted)" : `var(--series-${index + 1})`;
}

/* Pipeline order: closest to the caller first, then what left the cache, then failure.
 * "uncached" sits beside miss because it is one — fetched from upstream — except that
 * pkgcache, over its limit, served it without keeping it (engine/passthrough.go). */
export const OUTCOME_ORDER = ["hit", "dedup", "peer", "miss", "uncached", "fail"];

export const OUTCOME_LABEL = {
  hit: "local hit",
  dedup: "deduplicated",
  peer: "from a peer",
  miss: "from upstream",
  uncached: "served, not kept",
  fail: "failed",
};

/** The reader's word for an outcome. One the console does not know yet is shown as the
 *  server spelled it, rather than dropped from the chart it is counted in. */
export function outcomeLabel(outcome) {
  return OUTCOME_LABEL[outcome] ?? outcome;
}

/** Where an outcome stacks: known ones in pipeline order, anything newer after them. */
export function outcomeRank(outcome) {
  const index = OUTCOME_ORDER.indexOf(outcome);
  return index === -1 ? OUTCOME_ORDER.length : index;
}

export function outcomeColor(outcome) {
  return OUTCOME_ORDER.includes(outcome) ? `var(--outcome-${outcome})` : "var(--muted)";
}

/** Whether a finished-fetch event describes a download pkgcache served and did not keep.
 *  It arrives as an ordinary fetch.done, told apart only by its detail — see
 *  servePassThrough, which is the one place that writes "not cached: …". */
export function passedThrough(event) {
  return event?.kind === "fetch.done" && String(event.detail ?? "").startsWith("not cached");
}
