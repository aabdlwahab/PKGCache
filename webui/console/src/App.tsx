import { useEffect, useMemo, useState } from "react";
import { api } from "./lib/api";
import { ECOS, type Artifact, type Commit, type Eco } from "./lib/types";
import { fmtBytes } from "./lib/format";
import type { Mode, Theme } from "./lib/uiState";
import { useLocalStorage } from "./hooks/useLocalStorage";
import { useClock } from "./hooks/useClock";
import { usePolling } from "./hooks/usePolling";
import { useJob } from "./hooks/useJob";
import { useRoute } from "./hooks/useRoute";
import { TopBar, OfflineBanner, Footer } from "./components/Chrome";
import { HealthStrip, type Kpi } from "./components/HealthStrip";
import { PackagesPanel } from "./components/PackagesPanel";
import { StoragePanel } from "./components/StoragePanel";
import { DownloadsPanel } from "./components/DownloadsPanel";
import { RecentPanel } from "./components/RecentPanel";
import { ActionsPanel } from "./components/ActionsPanel";
import { HistoryPanel } from "./components/HistoryPanel";
import { EndpointsPanel } from "./components/EndpointsPanel";

export default function App() {
  const [theme, setTheme] = useLocalStorage<Theme>("pcc_theme", "dark");
  const [mode, setMode] = useLocalStorage<Mode>("pcc_mode", "online");
  const [view, setView] = useRoute();
  const now = useClock(1000);

  // Reflect theme on <html> so the token stylesheet applies app-wide.
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
  }, [theme]);

  // A bump to force the slower polls to refetch right after a job settles.
  const [refreshKey, setRefreshKey] = useState(0);
  const { job, busy, start, close } = useJob(() => setRefreshKey((k) => k + 1));

  const manifests = usePolling(api.manifests, 5000, [refreshKey]);
  const downloads = usePolling(api.downloads, 1500, []);
  const recent = usePolling(api.recent, 3000, []);
  const proxies = usePolling(api.proxies, 4000, [refreshKey]);
  const history = usePolling(api.history, 8000, [refreshKey]);
  const endpoints = usePolling(api.endpoints, 30000, []);

  const ecosystems: Partial<Record<Eco, Artifact[]>> = manifests.data?.ecosystems ?? {};
  const checkpointed: Partial<Record<Eco, number>> = manifests.data?.checkpointed ?? {};

  const usage = manifests.data?.usage;
  const { totalPkgs, totalSize, diskSize } = useMemo(() => {
    let pkgs = 0;
    let bytes = 0;
    for (const eco of ECOS) {
      const rows = ecosystems[eco] ?? [];
      pkgs += rows.length;
      // Docker layers are shared in the CAS; summing per-image sizes double-counts
      // them. Use the deduplicated CAS byte total instead (added back below).
      if (eco === "docker") continue;
      for (const it of rows) bytes += it.size ?? 0;
    }
    const dockerRows = ecosystems.docker ?? [];
    bytes +=
      usage?.docker_deduped ?? dockerRows.reduce((s, it) => s + (it.size ?? 0), 0);
    return {
      totalPkgs: pkgs,
      totalSize: fmtBytes(bytes),
      diskSize: usage ? fmtBytes(usage.disk_total) : null,
    };
  }, [ecosystems, usage]);

  // Real per-role health from /api/proxies. When at least one role is reachable we
  // trust the server's up-count and offline state; while the cache is restarting
  // (no roles up / unknown) we fall back to the optimistic localStorage mode.
  const roleUp = proxies.data?.up;
  const serverKnown = (roleUp ?? 0) > 0;
  const effectiveMode: Mode = serverKnown
    ? proxies.data?.offline
      ? "offline"
      : "online"
    : mode;
  const online = effectiveMode === "online";
  const proxiesUp =
    roleUp ?? Object.values(downloads.data?.sources ?? {}).filter((v) => v != null).length;
  const proxyColor = online ? "var(--ok)" : "var(--warn)";
  const proxyLabel = `${proxiesUp} ${proxiesUp === 1 ? "proxy" : "proxies"} up · ${effectiveMode}`;

  // Toggling mode recreates the pkgcache container under the other profile (a real
  // restart), streamed in the job console; the indicator confirms via health polls.
  const switchMode = (m: Mode) => {
    if (m === effectiveMode || busy) return;
    setMode(m); // optimistic until the health poll confirms
    start("mode", { target: m });
  };

  const commits: Commit[] = history.data?.commits ?? [];
  const headCommit = commits.find((c) => c.is_head) ?? null;
  const headShort = headCommit?.short ?? (history.data?.head ?? "").slice(0, 7);

  const rollback = (c: Commit) => {
    if (!busy) start("rollback", { commit: c.hash });
  };

  // ---- health-strip KPIs (the operator's instant glance read) ------------
  const kpis = useMemo<Kpi[]>(() => {
    // Hit rate over the recent-pulls window.
    const pulls = recent.data?.pulls ?? [];
    const hits = pulls.filter((p) => p.hit).length;
    const miss = pulls.filter((p) => !p.hit).length;
    const denom = hits + miss;
    const hitRate = denom ? Math.round((hits / denom) * 100) : 0;
    const hitColor =
      hitRate >= 70 ? "var(--ok)" : hitRate >= 40 ? "var(--warn)" : "var(--bad)";

    // Active downloads across all reachable proxies.
    const sources = downloads.data?.sources ?? {};
    let active = 0;
    for (const eco of ECOS) {
      for (const it of sources[eco] ?? []) if (it.status === "active") active++;
    }

    // Artifacts cached since the last checkpoint (uncommitted).
    let pendingNew = 0;
    for (const eco of ECOS) {
      pendingNew += Math.max(0, (ecosystems[eco]?.length ?? 0) - (checkpointed[eco] ?? 0));
    }

    const proxiesTotal = proxies.data?.roles?.length ?? 4;

    return [
      { label: "Packages", value: totalPkgs, sub: "5 ecosystems" },
      { label: "Cache size", value: diskSize ?? totalSize, sub: "on disk" },
      {
        label: "Hit rate",
        value: denom ? `${hitRate}%` : "—",
        sub: denom ? `last ${denom} pulls` : "no pulls yet",
        color: denom ? hitColor : "var(--fg)",
      },
      {
        label: "Downloads",
        value: active,
        sub: active ? "in progress" : "idle",
        color: active ? "var(--accent)" : "var(--fg)",
      },
      {
        label: "Uncommitted",
        value: pendingNew ? `+${pendingNew}` : "clean",
        sub: pendingNew ? "since checkpoint" : "all committed",
        color: pendingNew ? "var(--warn)" : "var(--ok)",
      },
      {
        label: "Proxies",
        value: `${proxiesUp}/${proxiesTotal}`,
        sub: online ? "up · online" : "up · offline",
        color: proxyColor,
      },
    ];
  }, [
    recent.data,
    downloads.data,
    ecosystems,
    checkpointed,
    proxies.data,
    totalPkgs,
    totalSize,
    diskSize,
    proxiesUp,
    online,
    proxyColor,
  ]);

  return (
    <div className="app">
      <TopBar
        theme={theme}
        onToggleTheme={() => setTheme(theme === "dark" ? "light" : "dark")}
        view={view}
        onView={setView}
        mode={effectiveMode}
        onMode={switchMode}
        proxyLabel={proxyLabel}
        proxyColor={proxyColor}
        headShort={headShort}
      />

      {view === "overview" ? (
        <>
          {!online && <OfflineBanner totalPkgs={totalPkgs} head={headCommit} />}

          <HealthStrip kpis={kpis} />

          {/* Activity row — Downloads + Recent. Same flex split as the bottom row
              (1.2 1 460px / 1 1 380px) so the vertical gutters line up exactly. */}
          <div className="region">
            <DownloadsPanel
              className="activity-main"
              sources={downloads.data?.sources ?? {}}
              theme={theme}
              online={online}
            />
            <RecentPanel
              className="activity-side"
              pulls={recent.data?.pulls ?? []}
              theme={theme}
              now={now}
            />
          </div>

          <div className="region bottom">
            <ActionsPanel busy={busy} job={job} commits={commits} onStart={start} onCloseJob={close} />
            <div className="col history">
              <HistoryPanel commits={commits} busy={busy} onRollback={rollback} />
              <StoragePanel fs={usage?.fs} cacheBytes={usage?.disk_total ?? 0} />
              <EndpointsPanel endpoints={endpoints.data ?? {}} theme={theme} />
            </div>
          </div>

          <Footer clock={new Date(now).toLocaleTimeString("en-GB")} />
        </>
      ) : (
        <div className="page-packages">
          <PackagesPanel
            fullHeight
            ecosystems={ecosystems}
            checkpointed={checkpointed}
            endpoints={endpoints.data ?? {}}
            theme={theme}
          />
        </div>
      )}
    </div>
  );
}
