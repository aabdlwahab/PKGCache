import type { Eco } from "./types";

// Ecosystem identity hues (OKLCH H) — equal lightness/chroma, hue-rotated.
export const HUE: Record<Eco, number> = {
  docker: 248,
  npm: 25,
  pip: 95,
  apt: 320,
  apk: 168,
  git: 210,
  files: 285,
};

export interface EcoColors {
  color: string;
  tint: string;
  border: string;
}

// Per-theme ecosystem color triplet (matches the handoff's ecoMeta()).
export function ecoColors(eco: Eco, theme: "dark" | "light"): EcoColors {
  const h = HUE[eco];
  if (theme === "light") {
    return {
      color: `oklch(0.50 0.17 ${h})`,
      tint: `oklch(0.62 0.15 ${h} / .13)`,
      border: `oklch(0.55 0.16 ${h} / .4)`,
    };
  }
  return {
    color: `oklch(0.82 0.13 ${h})`,
    tint: `oklch(0.82 0.13 ${h} / .15)`,
    border: `oklch(0.82 0.13 ${h} / .38)`,
  };
}

export function fmtBytes(n: number | null | undefined): string {
  if (n == null) return "—";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i ? 1 : 0)} ${u[i]}`;
}

// Relative time from an epoch-seconds value. `now` is passed so a 1s clock tick
// re-renders the feed without each row reading Date.now() independently.
export function relTime(s: number | null | undefined, now: number): string {
  if (s == null) return "";
  const d = Math.max(0, Math.round(now / 1000 - s));
  if (d < 60) return `${d}s`;
  if (d < 3600) return `${Math.round(d / 60)}m`;
  if (d < 86400) return `${Math.round(d / 3600)}h`;
  return `${Math.round(d / 86400)}d`;
}

// Short, truncated digest as shown in the packages table (sha256:… ~19 chars).
export function shortDigest(d: string | null | undefined): string {
  if (!d) return "";
  return d.length > 19 ? `${d.slice(0, 19)}…` : d;
}

// Version-aware descending compare (size/date are numeric; name is locale).
export function versionDesc(a: string, b: string): number {
  return String(b).localeCompare(String(a), undefined, { numeric: true });
}
