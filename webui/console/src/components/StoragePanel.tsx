import { Panel } from "./ui";
import { fmtBytes } from "../lib/format";
import type { FsStats } from "../lib/types";

// Free-space thresholds → color. Below 10% is critical, below 20% is a warning.
function freeColor(ratio: number): string {
  if (ratio < 0.1) return "var(--bad)";
  if (ratio < 0.2) return "var(--warn)";
  return "var(--ok)";
}

export function StoragePanel({ fs, cacheBytes }: { fs?: FsStats | null; cacheBytes: number }) {
  if (!fs || !fs.total) {
    return (
      <Panel title="Storage">
        <div className="panel-body">
          <div className="idle">Storage usage unavailable.</div>
        </div>
      </Panel>
    );
  }

  const { total, used, free } = fs;
  const cache = Math.min(cacheBytes, used); // cache is part of used
  const other = Math.max(0, used - cache);
  const pct = (n: number) => `${(100 * n) / total}%`;
  const freeRatio = free / total;
  const fc = freeColor(freeRatio);
  const freePctLabel = `${Math.round(freeRatio * 100)}% free`;
  const cacheOfTotal = total ? Math.round((1000 * cache) / total) / 10 : 0;

  return (
    <Panel
      title="Storage"
      headRight={
        <>
          <span className="spacer" />
          <span className="note" style={{ color: fc }}>
            {freePctLabel}
          </span>
        </>
      }
    >
      <div className="panel-body">
        <div className="store-bar" title={`cache ${fmtBytes(cache)} · other ${fmtBytes(other)} · free ${fmtBytes(free)}`}>
          <div className="store-seg" style={{ width: pct(cache), background: "var(--accent)" }} />
          <div className="store-seg" style={{ width: pct(other), background: "var(--muted)" }} />
        </div>
        <div className="store-stats">
          <span className="store-stat">
            <span className="eco-square" style={{ background: "var(--accent)" }} />
            <b>{fmtBytes(cache)}</b> cache ({cacheOfTotal}%)
          </span>
          <span className="store-stat">
            <span className="eco-square" style={{ background: "var(--muted)" }} />
            <b>{fmtBytes(other)}</b> other
          </span>
          <span className="store-stat">
            <span className="eco-square" style={{ background: "var(--inset)", border: "1px solid var(--line2)" }} />
            <b style={{ color: fc }}>{fmtBytes(free)}</b> free
          </span>
          <span className="spacer" />
          <span className="store-stat muted-stat">{fmtBytes(total)} total</span>
        </div>
      </div>
    </Panel>
  );
}
