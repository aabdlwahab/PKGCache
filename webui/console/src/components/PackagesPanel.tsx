import { useMemo, useState } from "react";
import { Panel, Segmented, LiveDot, EcoChip } from "./ui";
import { ecoColors, fmtBytes, shortDigest, versionDesc } from "../lib/format";
import { ECOS, type Artifact, type Eco, type Endpoints } from "../lib/types";
import type { SortKey, Theme } from "../lib/uiState";

const SORTS: { value: SortKey; label: string }[] = [
  { value: "name", label: "name" },
  { value: "size", label: "size" },
  { value: "date", label: "date" },
  { value: "ver", label: "ver" },
];

function compare(a: Artifact, b: Artifact, sort: SortKey): number {
  switch (sort) {
    case "size":
      return (b.size ?? 0) - (a.size ?? 0);
    case "date":
      return String(b.cached_at ?? "").localeCompare(String(a.cached_at ?? ""));
    case "ver":
      return versionDesc(a.version, b.version);
    default:
      return a.name.localeCompare(b.name);
  }
}

export function PackagesPanel({
  ecosystems,
  checkpointed,
  endpoints,
  theme,
  fullHeight = false,
}: {
  ecosystems: Partial<Record<Eco, Artifact[]>>;
  checkpointed: Partial<Record<Eco, number>>;
  endpoints: Endpoints;
  theme: Theme;
  // On its own page the panel scrolls as one tall body with sticky group headers
  // (`full`), instead of each ecosystem scrolling within its own 190px box.
  fullHeight?: boolean;
}) {
  const [query, setQuery] = useState("");
  const [sort, setSort] = useState<SortKey>("date");

  const groups = useMemo(() => {
    const q = query.trim().toLowerCase();
    return ECOS.map((eco) => {
      const all = ecosystems[eco] ?? [];
      const colors = ecoColors(eco, theme);
      const rows = all
        .filter((it) => !q || `${it.name} ${it.version}`.toLowerCase().includes(q))
        .slice()
        .sort((a, b) => compare(a, b, sort));
      const newCount = all.length - (checkpointed[eco] ?? 0);
      return {
        eco,
        colors,
        rows,
        total: all.length,
        countLabel: q
          ? `${rows.length} / ${all.length}`
          : `${all.length} ${all.length === 1 ? "pkg" : "pkgs"}`,
        hasNew: !q && newCount > 0,
        newCount,
        hint: endpoints[eco] ?? "",
        emptyMsg: q ? "no matches" : "nothing cached",
      };
    });
  }, [ecosystems, checkpointed, endpoints, theme, query, sort]);

  return (
    <Panel
      className={`packages${fullHeight ? " full" : ""}`}
      title="Packages in cache"
      headRight={
        <>
          <span className="live">
            <LiveDot /> live
          </span>
          <span className="spacer" />
          <div className="search">
            <span>⌕</span>
            <input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="filter name / version…"
            />
          </div>
          <Segmented<SortKey> options={SORTS} value={sort} onChange={setSort} />
        </>
      }
    >
      <div className="pkg-list">
        {groups.map((g) => (
          <div className="eco-group" key={g.eco}>
            <div className="eco-header">
              <EcoChip eco={g.eco} color={g.colors.color} tint={g.colors.tint} border={g.colors.border} />
              <span className="count">{g.countLabel}</span>
              {g.hasNew && (
                <span className="new-badge" title="cached but not yet checkpointed">
                  +{g.newCount} new
                </span>
              )}
              <span className="spacer" />
              <span className="hint" title={g.hint}>
                {g.hint}
              </span>
            </div>
            {/* Each ecosystem scrolls on its own so a large group can't push the
                others off-screen; the header above stays fixed for its group. */}
            <div className="eco-rows">
              {g.rows.length === 0 ? (
                <div className="empty">{g.emptyMsg}</div>
              ) : (
                <table className="pkg-table">
                  <tbody>
                    {g.rows.map((row, i) => (
                      <tr key={`${row.name}@${row.version}@${row.digest ?? i}`}>
                        <td className="c-name" title={row.name}>
                          {row.name}
                        </td>
                        <td className="c-ver" style={{ color: g.colors.color }}>
                          {row.version}
                        </td>
                        <td className="c-size">{fmtBytes(row.size)}</td>
                        <td className="c-dig">{shortDigest(row.digest)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          </div>
        ))}
      </div>
    </Panel>
  );
}
