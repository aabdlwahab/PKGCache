import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "./lib/api";
import {
  ECOS,
  GLOBAL_PROJECT,
  type Artifact,
  type Commit,
  type Eco,
  type JobAction,
} from "./lib/types";
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
import { ArtifactsPanel } from "./components/ArtifactsPanel";
import { StatisticsPanel } from "./components/StatisticsPanel";
import { StoragePanel } from "./components/StoragePanel";
import { DownloadsPanel } from "./components/DownloadsPanel";
import { RecentPanel } from "./components/RecentPanel";
import { ActionsPanel } from "./components/ActionsPanel";
import { LockwarmPanel } from "./components/LockwarmPanel";
import { HistoryPanel } from "./components/HistoryPanel";
import { EndpointsPanel } from "./components/EndpointsPanel";

export default function App() {
  const [theme, setTheme] = useLocalStorage<Theme>("pcc_theme", "dark");
  const [mode, setMode] = useLocalStorage<Mode>("pcc_mode", "online");
  const [project, setProject] = useLocalStorage<string>("pcc_project", GLOBAL_PROJECT);
  const [view, setView] = useRoute();
  const now = useClock(1000);

  // Reflect theme on <html> so the token stylesheet applies app-wide.
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
  }, [theme]);

  // A bump to force the slower polls to refetch right after a job settles.
  const [refreshKey, setRefreshKey] = useState(0);
  // Inline surface for project create/delete errors (replaces blocking window.alert).
  const [projectError, setProjectError] = useState<string | null>(null);
  const { job, busy, start: rawStart, close } = useJob(() => setRefreshKey((k) => k + 1));

  // Every cache op runs against the selected project (mode is instance-wide; the
  // server ignores `project` there). Scoping it here means panels need no changes.
  const start = useCallback(
    (action: JobAction, params: Record<string, string> = {}) =>
      rawStart(action, { project, ...params }),
    [rawStart, project],
  );

  // The project list drives the switcher; refetch after a create/delete (refreshKey).
  const projects = usePolling(api.projects, 10000, [refreshKey]);
  const projectList = projects.data?.projects ?? [];

  // All cache views are per-project; re-poll whenever the selection changes.
  const manifests = usePolling((s) => api.manifests(project, s), 5000, [refreshKey, project]);
  const downloads = usePolling((s) => api.downloads(project, s), 1500, [project]);
  const recent = usePolling((s) => api.recent(project, s), 3000, [project]);
  const proxies = usePolling((s) => api.proxies(project, s), 4000, [refreshKey, project]);
  const history = usePolling((s) => api.history(project, s), 8000, [refreshKey, project]);
  const endpoints = usePolling((s) => api.endpoints(project, s), 30000, [project]);
  const shuttle = usePolling((s) => api.shuttle(project, s), 6000, [refreshKey, project]);
  // Stats are heavier to aggregate (opens every ledger), so poll fast only while
  // the tab is open; otherwise idle it to ~hourly.
  const stats = usePolling(
    (s) => api.stats(project, s),
    view === "statistics" ? 5000 : 3_600_000,
    [refreshKey, project, view],
  );

  // Create/select/delete a project from the switcher. Create selects the new one;
  // deleting the current one falls back to global. The central process binds/drops
  // the project's ports on its next poll — no container restart.
  const selectProject = useCallback((p: string) => setProject(p), [setProject]);
  const createProject = useCallback(
    async (name: string) => {
      try {
        await api.createProject(name);
        setProjectError(null);
        setProject(name);
        setRefreshKey((k) => k + 1);
      } catch (e) {
        setProjectError((e as Error).message);
      }
    },
    [setProject],
  );
  const deleteProject = useCallback(
    async (name: string) => {
      try {
        await api.deleteProject(name);
        setProjectError(null);
        if (project === name) setProject(GLOBAL_PROJECT);
        setRefreshKey((k) => k + 1);
      } catch (e) {
        setProjectError((e as Error).message);
      }
    },
    [project, setProject],
  );

  const ecosystems: Partial<Record<Eco, Artifact[]>> = manifests.data?.ecosystems ?? {};
  const checkpointed: Partial<Record<Eco, number>> = manifests.data?.checkpointed ?? {};

  // Artifacts cached since the last checkpoint (uncommitted) — drives the
  // Checkpoint card's state pill and the "Uncommitted" health KPI.
  const pendingNew = useMemo(() => {
    let n = 0;
    for (const eco of ECOS) {
      n += Math.max(0, (ecosystems[eco]?.length ?? 0) - (checkpointed[eco] ?? 0));
    }
    return n;
  }, [ecosystems, checkpointed]);

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

    const proxiesTotal = proxies.data?.roles?.length ?? 4;

    return [
      { label: "Packages", value: totalPkgs, sub: `${ECOS.length} ecosystems` },
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
    pendingNew,
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
        projects={projectList}
        project={project}
        onSelectProject={selectProject}
        onCreateProject={createProject}
        onDeleteProject={deleteProject}
      />

      {projectError && (
        <div
          role="alert"
          style={{
            display: "flex",
            alignItems: "center",
            gap: "0.75rem",
            margin: "0.5rem 1rem 0",
            padding: "0.5rem 0.75rem",
            borderRadius: "6px",
            color: "var(--bad)",
            background: "var(--bad-bg)",
            fontSize: "0.85rem",
          }}
        >
          <span style={{ flex: 1 }}>{projectError}</span>
          <button
            className="copy-btn"
            style={{ color: "var(--bad)" }}
            onClick={() => setProjectError(null)}
          >
            dismiss
          </button>
        </div>
      )}

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
            <ActionsPanel
              busy={busy}
              job={job}
              commits={commits}
              shuttle={shuttle.data}
              pendingNew={pendingNew}
              headShort={headShort}
              headDate={headCommit?.date ?? ""}
              onStart={start}
              onCloseJob={close}
            />
            <div className="col history">
              <LockwarmPanel
                busy={busy}
                job={job}
                project={project}
                online={online}
                onStart={start}
                onCloseJob={close}
              />
              <HistoryPanel commits={commits} busy={busy} onRollback={rollback} />
              <StoragePanel fs={usage?.fs} cacheBytes={usage?.disk_total ?? 0} />
              <EndpointsPanel endpoints={endpoints.data ?? {}} theme={theme} />
            </div>
          </div>

          <Footer clock={new Date(now).toLocaleTimeString("en-GB")} />
        </>
      ) : view === "statistics" ? (
        <div className="page-stats">
          <StatisticsPanel data={stats.data ?? undefined} theme={theme} now={now} />
        </div>
      ) : (
        <div className="page-packages">
          <ArtifactsPanel
            project={project}
            online={online}
            filesEndpoint={endpoints.data?.files?.url ?? ""}
            onChanged={() => setRefreshKey((k) => k + 1)}
          />
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
