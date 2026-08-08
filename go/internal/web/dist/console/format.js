/* Value formatting. Every number a person reads passes through here, so units and
 * rounding stay consistent across six views. */

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

export const ECOSYSTEM_ORDER = ["pypi", "npm", "oci", "apt", "git", "files"];

/** Colour follows the entity, never its rank: a filter that removes npm must not
 *  repaint oci with npm's colour. An unknown ecosystem falls through to a neutral
 *  rather than being handed a slot that belongs to something else. */
export function ecoColor(eco) {
  const index = ECOSYSTEM_ORDER.indexOf(eco);
  return index === -1 ? "var(--muted)" : `var(--series-${index + 1})`;
}

export const OUTCOME_ORDER = ["hit", "dedup", "peer", "miss", "fail"];

export const OUTCOME_LABEL = {
  hit: "local hit",
  dedup: "deduplicated",
  peer: "from a peer",
  miss: "from upstream",
  fail: "failed",
};

export function outcomeColor(outcome) {
  return OUTCOME_ORDER.includes(outcome) ? `var(--outcome-${outcome})` : "var(--muted)";
}
