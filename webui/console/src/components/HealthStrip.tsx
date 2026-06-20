/** The at-a-glance health row (six KPI cells) directly under the top bar.
   Each cell answers a glance question: is the cache healthy / do I need to
   checkpoint? Values + threshold colors are derived in App and passed in. */
export interface Kpi {
  label: string;
  value: string | number;
  sub: string;
  color?: string; // value color; defaults to --fg
}

export function HealthStrip({ kpis }: { kpis: Kpi[] }) {
  return (
    <div className="health-wrap">
      <div className="health-grid">
        {kpis.map((k) => (
          <div className="health-cell" key={k.label}>
            <div className="health-label">{k.label}</div>
            <div className="health-value-row">
              <span className="health-value" style={{ color: k.color ?? "var(--fg)" }}>
                {k.value}
              </span>
              <span className="health-sub">{k.sub}</span>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
