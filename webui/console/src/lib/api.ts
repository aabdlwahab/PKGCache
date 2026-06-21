// Thin typed wrapper over the webui JSON API. All paths are relative (/api/…)
// so the same code works behind the Vite dev proxy and the prod nginx proxy.
import {
  GLOBAL_PROJECT,
  type DownloadsResp,
  type Endpoints,
  type HistoryResp,
  type JobAction,
  type JobResp,
  type ManifestsResp,
  type ProjectInfo,
  type ProjectsResp,
  type ProxiesResp,
  type RecentResp,
  type ShuttleResp,
} from "./types";

async function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  const r = await fetch(path, { headers: { Accept: "application/json" }, signal });
  if (!r.ok) throw new Error(`${path} → ${r.status}`);
  return (await r.json()) as T;
}

// All the cache views are per-project; the global project takes no query param so
// its requests stay byte-for-byte what they were before projects existed.
function withProject(path: string, project?: string): string {
  return project && project !== GLOBAL_PROJECT
    ? `${path}?project=${encodeURIComponent(project)}`
    : path;
}

export const api = {
  manifests: (project?: string, s?: AbortSignal) =>
    getJSON<ManifestsResp>(withProject("/api/manifests", project), s),
  downloads: (project?: string, s?: AbortSignal) =>
    getJSON<DownloadsResp>(withProject("/api/downloads", project), s),
  recent: (project?: string, s?: AbortSignal) =>
    getJSON<RecentResp>(withProject("/api/recent", project), s),
  endpoints: (project?: string, s?: AbortSignal) =>
    getJSON<Endpoints>(withProject("/api/endpoints", project), s),
  proxies: (project?: string, s?: AbortSignal) =>
    getJSON<ProxiesResp>(withProject("/api/proxies", project), s),
  history: (project?: string, s?: AbortSignal) =>
    getJSON<HistoryResp>(withProject("/api/history", project), s),
  shuttle: (project?: string, s?: AbortSignal) =>
    getJSON<ShuttleResp>(withProject("/api/shuttle", project), s),
  job: (id: number, s?: AbortSignal) => getJSON<JobResp>(`/api/jobs/${id}`, s),

  // ---- projects -----------------------------------------------------------
  projects: (s?: AbortSignal) => getJSON<ProjectsResp>("/api/projects", s),

  async createProject(name: string): Promise<ProjectInfo> {
    const r = await fetch("/api/projects", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
    const data = (await r.json()) as ProjectInfo & { error?: string };
    if (!r.ok || data.error) throw new Error(data.error || `create failed (${r.status})`);
    return data;
  },

  async deleteProject(name: string): Promise<void> {
    const r = await fetch(`/api/projects/${encodeURIComponent(name)}`, { method: "DELETE" });
    const data = (await r.json().catch(() => ({}))) as { error?: string };
    if (!r.ok || data.error) throw new Error(data.error || `delete failed (${r.status})`);
  },

  async startJob(action: JobAction, params: Record<string, string>): Promise<number> {
    const r = await fetch("/api/jobs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action, ...params }),
    });
    const data = (await r.json()) as { id?: number; error?: string };
    if (!r.ok || data.error) throw new Error(data.error || `job failed (${r.status})`);
    return data.id as number;
  },
};
