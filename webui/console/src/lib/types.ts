// API response shapes — mirror webui's JSON endpoints (see webui/server.py).

export type Eco = "docker" | "npm" | "pip" | "apt" | "apk";
export const ECOS: Eco[] = ["docker", "npm", "pip", "apt", "apk"];

export interface Artifact {
  name: string;
  version: string;
  digest: string | null;
  size: number | null;
  origin?: string | null;
  arch?: string | null;
  cached_at?: string | null;
}

export interface FsStats {
  total: number;
  used: number;
  free: number;
}

export interface Usage {
  disk: Record<string, number>; // bytes per cache subdir (docker/npm/pip/apt)
  disk_total: number; // total on-disk footprint
  docker_deduped: number; // CAS blob bytes — docker counted once (shared layers)
  fs?: FsStats | null; // filesystem capacity of the volume holding the cache
}

export interface ManifestsResp {
  ecosystems: Record<Eco, Artifact[]>;
  checkpointed: Record<Eco, number>;
  usage?: Usage;
  age: number;
}

export interface PackagesResp {
  ecosystems: Partial<Record<Eco, Artifact[]>>;
  page: number;
  sort: string;
}

export interface DownloadItem {
  id: string;
  name: string;
  downloaded: number;
  total: number | null;
  pct: number | null;
  status: "active" | "complete" | "error";
  updated: number;
}

// sources[eco] === null means that proxy/role was unreachable.
export interface DownloadsResp {
  sources: Partial<Record<Eco, DownloadItem[] | null>>;
  age: number | null;
}

export interface RecentPull {
  eco: Eco;
  name: string | null;
  id?: string;
  size: number | null;
  hit: boolean;
  // Backend does not emit this yet; reserved for the offline-miss FAIL state.
  failed?: boolean;
  time: number | null;
}

export interface RecentResp {
  pulls: RecentPull[];
}

export type Endpoints = Partial<Record<Eco, string>>;

export interface Commit {
  hash: string;
  short: string;
  date: string;
  subject: string;
  is_checkpoint: boolean;
  is_head: boolean;
}

export interface HistoryResp {
  head: string;
  commits: Commit[];
}

export interface ProxyService {
  name: string;
  state: string;
  status: string;
}

export interface RoleHealth {
  role: string;
  up: boolean;
  offline: boolean | null;
}

export interface ProxiesResp {
  available: boolean;
  profile?: "online" | "offline" | null;
  services: ProxyService[];
  // Live per-role health (added server-side): the real up-count and offline state.
  roles?: RoleHealth[];
  up?: number;
  offline?: boolean;
}

export type JobStatus = "running" | "done" | "failed";

export interface JobResp {
  id: number;
  action: string;
  status: JobStatus;
  log: string;
}

export interface JobsResp {
  busy: boolean;
  jobs: { id: number; action: string; status: JobStatus }[];
}

export type JobAction = "checkpoint" | "export" | "import" | "rollback" | "mode";
