// Thin typed wrapper over the webui JSON API. All paths are relative (/api/…)
// so the same code works behind the Vite dev proxy and the prod nginx proxy.
import type {
  DownloadsResp,
  Endpoints,
  HistoryResp,
  JobAction,
  JobResp,
  ManifestsResp,
  ProxiesResp,
  RecentResp,
} from "./types";

async function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  const r = await fetch(path, { headers: { Accept: "application/json" }, signal });
  if (!r.ok) throw new Error(`${path} → ${r.status}`);
  return (await r.json()) as T;
}

export const api = {
  manifests: (s?: AbortSignal) => getJSON<ManifestsResp>("/api/manifests", s),
  downloads: (s?: AbortSignal) => getJSON<DownloadsResp>("/api/downloads", s),
  recent: (s?: AbortSignal) => getJSON<RecentResp>("/api/recent", s),
  endpoints: (s?: AbortSignal) => getJSON<Endpoints>("/api/endpoints", s),
  proxies: (s?: AbortSignal) => getJSON<ProxiesResp>("/api/proxies", s),
  history: (s?: AbortSignal) => getJSON<HistoryResp>("/api/history", s),
  job: (id: number, s?: AbortSignal) => getJSON<JobResp>(`/api/jobs/${id}`, s),

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
