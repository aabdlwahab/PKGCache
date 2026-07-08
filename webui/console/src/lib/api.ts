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
  type MeResp,
  type ProjectInfo,
  type ProjectsResp,
  type ProxiesResp,
  type RecentResp,
  type Role,
  type ShuttleResp,
  type StatsResp,
  type User,
  type UsersResp,
} from "./types";

// A 401 from any polling request means the session expired (or was never there);
// useAuth registers a handler here to drop the app back to the login screen. Kept
// module-level so the thin fetch helpers can signal it without threading a callback
// through every call site.
let _onUnauthorized: (() => void) | null = null;
export function setUnauthorizedHandler(fn: (() => void) | null): void {
  _onUnauthorized = fn;
}

async function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  const r = await fetch(path, { headers: { Accept: "application/json" }, signal });
  if (!r.ok) {
    if (r.status === 401) _onUnauthorized?.();
    throw new Error(`${path} → ${r.status}`);
  }
  return (await r.json()) as T;
}

// A JSON mutation (POST/PATCH/DELETE) that surfaces the server's {error} message and
// routes a 401 through the session-expiry handler.
async function mutate<T>(method: string, path: string, body?: unknown): Promise<T> {
  const r = await fetch(path, {
    method,
    headers: { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (r.status === 401) _onUnauthorized?.();
  const data = (await r.json().catch(() => ({}))) as T & { error?: string };
  if (!r.ok || data.error) throw new Error(data.error || `${path} → ${r.status}`);
  return data;
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
  stats: (project?: string, s?: AbortSignal) =>
    getJSON<StatsResp>(withProject("/api/stats", project), s),
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
  // Poll a job, fetching only the log written since `offset` (the response's `log`
  // is that slice; its `offset` is the new total). Kills the O(n²) re-fetch of the
  // whole log every tick on a long checkpoint.
  job: (id: number, offset = 0, s?: AbortSignal) =>
    getJSON<JobResp>(`/api/jobs/${id}?offset=${offset}`, s),

  // The rewritten uv.lock a lockwarm job produced for a project (a download URL,
  // not JSON — the browser fetches it directly).
  lockfileUrl: (project?: string) => withProject("/api/lockfile", project),

  // ---- files ecosystem: write token + artifact upload/delete --------------
  // Whether a write token is set for the project (never returns the token itself).
  tokenStatus: (project?: string, s?: AbortSignal) =>
    getJSON<{ set: boolean }>(withProject("/api/token", project), s),

  // Generate/rotate the project's write token; returned ONCE for the UI to display.
  async rotateToken(project?: string): Promise<string> {
    const r = await fetch("/api/token", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ project: project ?? GLOBAL_PROJECT }),
    });
    const data = (await r.json().catch(() => ({}))) as { token?: string; error?: string };
    if (!r.ok || data.error || !data.token)
      throw new Error(data.error || `token generation failed (${r.status})`);
    return data.token;
  },

  // Upload one file via the webui proxy (which injects the write token). Uses
  // XMLHttpRequest because fetch has no upload-progress events. onProgress gets a
  // 0–1 fraction. The raw File is the body; server streams it to the files role.
  uploadArtifact(
    project: string | undefined,
    path: string,
    file: File | Blob,
    overwrite: boolean,
    onProgress?: (frac: number) => void,
  ): Promise<{ path: string; size: number; sha256: string; url: string }> {
    const qs = new URLSearchParams({ path });
    if (project && project !== GLOBAL_PROJECT) qs.set("project", project);
    if (overwrite) qs.set("overwrite", "1");
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open("POST", `/api/artifacts?${qs.toString()}`);
      xhr.setRequestHeader("Content-Type", "application/octet-stream");
      if (onProgress && xhr.upload)
        xhr.upload.onprogress = (e) => e.lengthComputable && onProgress(e.loaded / e.total);
      xhr.onload = () => {
        let data: Record<string, unknown> = {};
        try {
          data = JSON.parse(xhr.responseText);
        } catch {
          /* non-JSON error body (e.g. plain text from the role) */
        }
        if (xhr.status >= 200 && xhr.status < 300)
          resolve(data as { path: string; size: number; sha256: string; url: string });
        else reject(new Error((data.error as string) || xhr.responseText || `upload failed (${xhr.status})`));
      };
      xhr.onerror = () => reject(new Error("upload failed (network error)"));
      xhr.send(file);
    });
  },

  async deleteArtifact(project: string | undefined, path: string): Promise<void> {
    const qs = new URLSearchParams({ path });
    if (project && project !== GLOBAL_PROJECT) qs.set("project", project);
    const r = await fetch(`/api/artifacts?${qs.toString()}`, { method: "DELETE" });
    if (r.status === 204) return;
    const data = (await r.json().catch(() => ({}))) as { error?: string };
    throw new Error(data.error || `delete failed (${r.status})`);
  },

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

  // Flip ONE project's soft offline flag (global included). A registry write the
  // cache process applies on its next poll (~5s) — scoped to this project, no
  // restart. The instance-wide hard switch is the separate `mode` job.
  async setProjectMode(
    name: string,
    target: "online" | "offline",
  ): Promise<{ name: string; offline: boolean }> {
    const r = await fetch(`/api/projects/${encodeURIComponent(name)}/mode`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ target }),
    });
    const data = (await r.json().catch(() => ({}))) as {
      name?: string;
      offline?: boolean;
      error?: string;
    };
    if (!r.ok || data.error) throw new Error(data.error || `mode change failed (${r.status})`);
    return data as { name: string; offline: boolean };
  },

  async startJob(action: JobAction, params: Record<string, string>): Promise<number> {
    const r = await fetch("/api/jobs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action, ...params }),
    });
    // A proxy error (e.g. 413 on a large uv.lock upload) returns an HTML page, not
    // JSON — surface it as a clear status message instead of a JSON.parse crash.
    const data = (await r.json().catch(() => ({}))) as { id?: number; error?: string };
    if (!r.ok || data.error) throw new Error(data.error || `job failed (${r.status})`);
    return data.id as number;
  },

  // Reassign a project's owner (superuser only). The new owner must be an existing
  // admin or superuser.
  setProjectOwner: (name: string, owner: string) =>
    mutate<{ name: string; owner: string }>(
      "POST",
      `/api/projects/${encodeURIComponent(name)}/owner`,
      { owner },
    ),

  // ---- auth: session + accounts -------------------------------------------
  // Bootstrap the auth state. 401 (enabled but no session) is normalized to a
  // "must log in" answer rather than an error, so the caller has one shape to read.
  async me(): Promise<MeResp> {
    const r = await fetch("/api/me", { headers: { Accept: "application/json" } });
    if (r.status === 401) return { auth_enabled: true, authenticated: false };
    if (!r.ok) throw new Error(`/api/me → ${r.status}`);
    return (await r.json()) as MeResp;
  },

  login: (username: string, password: string) =>
    mutate<{ username: string; role: Role }>("POST", "/api/login", { username, password }),

  logout: () => mutate<{ ok: boolean }>("POST", "/api/logout"),

  users: (s?: AbortSignal) => getJSON<UsersResp>("/api/users", s),

  createUser: (body: {
    username: string;
    password: string;
    role: Role;
    reports_to?: string | null;
  }) => mutate<User>("POST", "/api/users", body),

  updateUser: (
    name: string,
    changes: { role?: Role; reports_to?: string | null; password?: string },
  ) => mutate<User>("PATCH", `/api/users/${encodeURIComponent(name)}`, changes),

  deleteUser: (name: string) =>
    mutate<{ deleted: string }>("DELETE", `/api/users/${encodeURIComponent(name)}`),
};
