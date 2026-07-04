import { Panel } from "./ui";
import { ecoColors, fmtBytes, relTime } from "../lib/format";
import { ECOS, type Eco, type StatsResp } from "../lib/types";
import type { Theme } from "../lib/uiState";

function fmtDuration(s: number): string {
  if (!s || s < 1) return "0s";
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = Math.round(s % 60);
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${sec}s`;
  return `${sec}s`;
}

function fmtRate(bps: number): string {
  return bps > 0 ? `${fmtBytes(bps)}/s` : "—";
}

function EcoTag({ eco, theme }: { eco: Eco; theme: Theme }) {
  const c = ecoColors(eco, theme);
  return (
    <span className="eco-tag" style={{ color: c.color, background: c.tint }}>
      {eco}
    </span>
  );
}

function Bar({ value, max, color }: { value: number; max: number; color: string }) {
  const w = max > 0 && value > 0 ? Math.max(3, Math.round((value / max) * 100)) : 0;
  return (
    <div className="bar-track">
      <div className="bar-fill" style={{ width: `${w}%`, background: color }} />
    </div>
  );
}

function Sparkline({ values, color }: { values: number[]; color: string }) {
  if (values.length < 2) return <div className="idle">not enough samples yet</div>;
  const W = 600;
  const H = 60;
  const max = Math.max(...values);
  const pts = values
    .map((v, i) => {
      const x = (i / (values.length - 1)) * W;
      const y = H - (max > 0 ? (v / max) * (H - 4) : 0) - 2;
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
  return (
    <svg className="sparkline" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none">
      <polyline points={pts} fill="none" stroke={color} strokeWidth="2" />
    </svg>
  );
}

export function StatisticsPanel({
  data,
  theme,
  now,
}: {
  data: StatsResp | undefined;
  theme: Theme;
  now: number;
}) {
  if (!data) {
    return (
      <Panel title="Statistics">
        <div className="idle">loading…</div>
      </Panel>
    );
  }

  const t = data.totals;
  const diskTotal = data.usage?.disk_total ?? t.size;

  const cards = [
    { label: "Time saved", value: fmtDuration(data.time_saved_seconds), sub: "estimated vs. upstream", accent: true },
    { label: "Served from cache", value: fmtBytes(data.bytes_saved), sub: "bytes not re-fetched" },
    { label: "Hit rate", value: data.hit_rate != null ? `${data.hit_rate}%` : "—", sub: `${t.hits} hit · ${t.misses} miss` },
    { label: "Requests", value: t.requests.toLocaleString(), sub: "packages served" },
    { label: "Packages", value: t.packages.toLocaleString(), sub: "7 ecosystems" },
    { label: "Cache size", value: fmtBytes(diskTotal), sub: "on disk" },
  ];

  const maxEcoReq = Math.max(1, ...data.by_eco.map((e) => e.requests));
  const maxLarge = Math.max(1, ...data.top_largest.map((x) => x.size || 0));
  const maxArch = Math.max(1, ...data.by_arch.map((a) => a.count));
  const bwValues = data.bandwidth.samples.map((s) => s.bps);

  return (
    <div className="stats-stack">
      {/* headline KPI cards */}
      <div className="stat-cards">
        {cards.map((c) => (
          <div className={`stat-card${c.accent ? " accent" : ""}`} key={c.label}>
            <span className="sc-value">{c.value}</span>
            <span className="sc-label">{c.label}</span>
            <span className="sc-sub">{c.sub}</span>
          </div>
        ))}
      </div>

      {/* per-ecosystem breakdown */}
      <Panel title="By ecosystem">
        <table className="stat-table">
          <thead>
            <tr>
              <th>eco</th><th className="num">packages</th><th className="num">size</th>
              <th className="num">requests</th><th className="num">hits</th>
              <th className="num">misses</th><th>request share</th>
            </tr>
          </thead>
          <tbody>
            {data.by_eco.map((e) => {
              const c = ecoColors(e.eco, theme);
              return (
                <tr key={e.eco}>
                  <td><EcoTag eco={e.eco} theme={theme} /></td>
                  <td className="num">{e.count.toLocaleString()}</td>
                  <td className="num">{fmtBytes(e.size)}</td>
                  <td className="num">{e.requests.toLocaleString()}</td>
                  <td className="num ok">{e.hit_count.toLocaleString()}</td>
                  <td className="num bad">{e.miss_count.toLocaleString()}</td>
                  <td><Bar value={e.requests} max={maxEcoReq} color={c.color} /></td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </Panel>

      {/* leaderboards — most-requested per ecosystem */}
      <Panel title="Most requested" headRight={<span className="note">top packages per ecosystem</span>}>
        <div className="lb-grid">
          {ECOS.map((eco) => {
            const rows = data.leaderboard[eco] ?? [];
            const c = ecoColors(eco, theme);
            const max = Math.max(1, ...rows.map((r) => r.count));
            return (
              <div className="lb-col" key={eco}>
                <div className="lb-head"><EcoTag eco={eco} theme={theme} /></div>
                {rows.length === 0 ? (
                  <div className="idle sm">no requests yet</div>
                ) : (
                  rows.map((r) => (
                    <div className="lb-row" key={r.name} title={`${r.count} requests`}>
                      <span className="lb-rank">{r.count}</span>
                      <span className="lb-name">{r.name}</span>
                      <Bar value={r.count} max={max} color={c.color} />
                    </div>
                  ))
                )}
              </div>
            );
          })}
        </div>
      </Panel>

      <div className="stat-cols">
        {/* largest cached artifacts */}
        <Panel className="stat-col" title="Largest cached">
          <div className="list-pad">
            {data.top_largest.length === 0 ? (
              <div className="idle">empty</div>
            ) : (
              data.top_largest.map((x) => {
                const c = ecoColors(x.eco, theme);
                return (
                  <div className="big-row" key={`${x.eco}:${x.name}:${x.version}`}>
                    <EcoTag eco={x.eco} theme={theme} />
                    <span className="big-name">{x.name}</span>
                    <span className="big-ver">{x.version}</span>
                    <span className="spacer" />
                    <span className="big-size">{fmtBytes(x.size)}</span>
                    <Bar value={x.size} max={maxLarge} color={c.color} />
                  </div>
                );
              })
            )}
          </div>
        </Panel>

        {/* recently added */}
        <Panel className="stat-col" title="Recently cached">
          <div className="list-pad">
            {data.recent_added.length === 0 ? (
              <div className="idle">empty</div>
            ) : (
              data.recent_added.map((x) => (
                <div className="rec-row" key={`${x.eco}:${x.name}:${x.version}`}>
                  <EcoTag eco={x.eco} theme={theme} />
                  <span className="rec-name">{x.name}</span>
                  <span className="rec-ver">{x.version}</span>
                  <span className="spacer" />
                  <span className="rec-size">{x.size != null ? fmtBytes(x.size) : ""}</span>
                </div>
              ))
            )}
          </div>
        </Panel>
      </div>

      <div className="stat-cols">
        {/* platform / arch breakdown */}
        <Panel className="stat-col" title="By platform / arch">
          <div className="list-pad">
            {data.by_arch.length === 0 ? (
              <div className="idle">no arch data</div>
            ) : (
              data.by_arch.map((a) => (
                <div className="arch-row" key={a.arch}>
                  <span className="arch-name">{a.arch}</span>
                  <span className="arch-count">{a.count.toLocaleString()}</span>
                  <Bar value={a.count} max={maxArch} color="var(--accent)" />
                  <span className="arch-size">{fmtBytes(a.size)}</span>
                </div>
              ))
            )}
          </div>
        </Panel>

        {/* upstream bandwidth → time-saved basis */}
        <Panel
          className="stat-col"
          title="Upstream speed"
          headRight={<span className="note">{fmtRate(data.bandwidth.current_bps)} now</span>}
        >
          <div className="bw-body">
            <Sparkline values={bwValues} color="var(--accent)" />
            <p className="note bw-note">
              Measured from real cache-miss downloads (and scheduled probes). The
              “time saved” estimate divides bytes-served-from-cache by this rate, so
              it’s an approximation — small-file latency savings aren’t counted, and
              it reflects the online side only.
            </p>
          </div>
        </Panel>
      </div>

      <div className="stats-foot note">
        last access uses each package’s most recent request
        {data.leaderboard.pip && data.leaderboard.pip[0]?.last_access
          ? ` · top pip pkg last requested ${relTime(data.leaderboard.pip[0].last_access, now)} ago`
          : ""}
      </div>
    </div>
  );
}
