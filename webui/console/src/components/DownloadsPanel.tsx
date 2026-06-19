import { useMemo } from "react";
import { Panel, LiveDot } from "./ui";
import { ecoColors, fmtBytes } from "../lib/format";
import { ECOS, type DownloadItem, type DownloadsResp, type Eco } from "../lib/types";
import type { Theme } from "../lib/uiState";

interface Row {
  key: string;
  eco: Eco;
  item: DownloadItem;
}

export function DownloadsPanel({
  sources,
  theme,
  online,
}: {
  sources: DownloadsResp["sources"];
  theme: Theme;
  online: boolean;
}) {
  const rows = useMemo<Row[]>(() => {
    const out: Row[] = [];
    for (const eco of ECOS) {
      const items = sources[eco];
      if (!items) continue; // null = unreachable proxy
      for (const item of items) {
        if (item.status !== "active") continue; // panel shows in-flight only
        out.push({ key: `${eco}:${item.id}`, eco, item });
      }
    }
    return out;
  }, [sources]);

  return (
    <Panel
      title="Downloads in progress"
      headRight={
        <>
          <span className="spacer" />
          <span className="live">
            <LiveDot color="var(--accent)" fast />
            {rows.length ? `${rows.length} active` : "idle"}
          </span>
        </>
      }
    >
      <div className="dl-body">
        {rows.length === 0 ? (
          <div className="idle">
            {online ? "No transfers in flight — cache is warm." : "Offline — no upstream transfers."}
          </div>
        ) : (
          rows.map(({ key, eco, item }) => {
            const c = ecoColors(eco, theme);
            const indet = item.total == null;
            const pct = item.pct ?? (item.total ? Math.round((item.downloaded / item.total) * 100) : 0);
            return (
              <div className="dl-item" key={key}>
                <div className="dl-head">
                  <span className="dl-tag" style={{ color: c.color, background: c.tint }}>
                    {eco}
                  </span>
                  <span className="dl-name">{item.name}</span>
                  <span className="spacer" />
                  <span className="dl-meta">
                    {indet
                      ? `${fmtBytes(item.downloaded)} · resolving`
                      : `${fmtBytes(item.downloaded)} / ${fmtBytes(item.total)} · ${pct}%`}
                  </span>
                </div>
                <div className="dl-bar">
                  {indet ? (
                    <div className="dl-indet" style={{ background: c.color }} />
                  ) : (
                    <div className="dl-fill" style={{ width: `${pct}%`, background: c.color }} />
                  )}
                </div>
              </div>
            );
          })
        )}
      </div>
    </Panel>
  );
}
