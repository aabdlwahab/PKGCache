// API response shapes — mirror webui's JSON endpoints (see webui/app/api/routes.py).

export type Eco = "docker" | "npm" | "pip" | "apt" | "apk" | "git" | "files";
export const ECOS: Eco[] = ["docker", "npm", "pip", "apt", "apk", "git", "files"];

// The implicit default project: today's default URLs, the caches/ repo.
export const GLOBAL_PROJECT = "global";

// One project served by the central instance. `default: true` marks the global
// project (default ports); named projects carry their allocated ports.
export interface ProjectInfo {
  name: string;
  ports: Record<string, number>; // role (oci/npm/pypi/apt) → port
  repo: string;
  default: boolean;
}

export interface ProjectsResp {
  projects: ProjectInfo[];
}

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

// ---- /api/stats ----------------------------------------------------------
export interface StatsEco {
  eco: Eco;
  count: number;
  size: number;
  requests: number;
  hit_count: number;
  hit_bytes: number;
  miss_count: number;
  miss_bytes: number;
}
export interface LeaderEntry {
  name: string;
  count: number;
  last_access: number | null; // epoch seconds
}
export interface ArchEntry {
  arch: string;
  count: number;
  size: number;
}
export interface LargeEntry {
  eco: Eco;
  name: string;
  version: string;
  size: number;
}
export interface RecentAdded {
  eco: Eco;
  name: string;
  version: string;
  size: number | null;
  cached_at: string | null;
}
export interface BwSample {
  ts: number;
  bps: number;
  source: string;
}
export interface StatsResp {
  project: string;
  totals: { packages: number; size: number; requests: number; hits: number; misses: number };
  hit_rate: number | null;
  bytes_saved: number;
  time_saved_seconds: number;
  by_eco: StatsEco[];
  by_arch: ArchEntry[];
  leaderboard: Partial<Record<Eco, LeaderEntry[]>>;
  top_largest: LargeEntry[];
  recent_added: RecentAdded[];
  bandwidth: { current_bps: number; samples: BwSample[] };
  usage?: Usage;
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

// A client pull endpoint as data (the backend no longer preformats a display string):
// `url` is the copy-able target (carries the "<host>" placeholder), `note` a usage hint.
export interface Endpoint {
  url: string;
  note: string;
}
export type Endpoints = Partial<Record<Eco, Endpoint>>;

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
  // `log` is the slice FROM the requested offset; `offset` is the new total length,
  // so the poller can request only what it hasn't seen yet (see useJob).
  log: string;
  offset: number;
}

export interface JobsResp {
  busy: boolean;
  jobs: { id: number; action: string; status: JobStatus }[];
}

export type JobAction = "checkpoint" | "export" | "import" | "rollback" | "mode" | "lockwarm";

export interface ShuttleCheckpoint {
  hash: string;
  short: string;
  date: string;
  subject: string;
}

// /api/shuttle — the fixed staging dirs + what's currently staged for import.
export interface ShuttleResp {
  export_dir: string;
  import_dir: string;
  import_ready: boolean;
  import_checkpoints: ShuttleCheckpoint[];
}
