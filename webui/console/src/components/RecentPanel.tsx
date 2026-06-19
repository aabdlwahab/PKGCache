import { Panel } from "./ui";
import { ecoColors, fmtBytes, relTime } from "../lib/format";
import type { RecentPull } from "../lib/types";
import type { Theme } from "../lib/uiState";

function badgeOf(p: RecentPull): { text: string; color: string; bg: string } {
  if (p.failed) return { text: "FAIL", color: "var(--bad)", bg: "var(--bad-bg)" };
  if (!p.hit) return { text: "MISS", color: "var(--bad)", bg: "var(--bad-bg)" };
  return { text: "HIT", color: "var(--ok)", bg: "var(--ok-bg)" };
}

export function RecentPanel({
  pulls,
  theme,
  now,
}: {
  pulls: RecentPull[];
  theme: Theme;
  now: number;
}) {
  const hits = pulls.filter((p) => p.hit).length;
  const miss = pulls.filter((p) => !p.hit).length;

  return (
    <Panel
      title="Recent pulls"
      headRight={
        <>
          <span className="spacer" />
          <span className="note" style={{ color: "var(--ok)" }}>
            {hits} hit
          </span>
          <span className="note" style={{ color: "var(--bad)" }}>
            {miss} miss
          </span>
        </>
      }
    >
      <div className="recent-scroll">
        {pulls.map((p, i) => {
          const c = ecoColors(p.eco, theme);
          const b = badgeOf(p);
          return (
            <div className="recent-row" key={`${p.id ?? i}:${p.time}`}>
              <span className="badge" style={{ color: b.color, background: b.bg }}>
                {b.text}
              </span>
              <span className="recent-eco" style={{ color: c.color }}>
                {p.eco}
              </span>
              <span className="recent-name">{p.name}</span>
              <span className="recent-size">{p.size ? fmtBytes(p.size) : ""}</span>
              <span className="recent-time">{p.time ? `${relTime(p.time, now)} ago` : ""}</span>
            </div>
          );
        })}
      </div>
    </Panel>
  );
}
