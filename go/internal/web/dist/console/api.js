/* The control-plane client. One place that knows about HTTP, so views never do. */

const V1 = "/api/v1";

export class APIError extends Error {
  /* `detail` is the whole parsed body. Some things a caller must know are only
     knowable from a refusal — the sign-in screen learns whether guest browsing is on
     offer from the 401 that told it to sign in, because every endpoint that could
     have answered sits behind that same check. */
  constructor(message, status, code, detail = {}) {
    super(message);
    this.status = status;
    this.code = code;
    this.detail = detail;
  }
}

async function request(path, init) {
  const response = await fetch(`${V1}${path}`, {
    credentials: "same-origin",
    ...init,
    headers: {
      ...(init?.body ? { "Content-Type": "application/json" } : {}),
      ...init?.headers,
    },
  });
  if (!response.ok) {
    let detail = {};
    try {
      detail = await response.json();
    } catch {
      // A reverse proxy in front can answer with HTML; the status is still useful.
    }
    throw new APIError(
      detail.message ?? detail.error ?? response.statusText,
      response.status,
      detail.code,
      detail,
    );
  }
  if (response.status === 204) return undefined;
  return response.json();
}

const body = (value) => JSON.stringify(value);
const at = (project) => `/projects/${encodeURIComponent(project)}`;
const params = (values) => {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(values)) {
    if (value !== undefined && value !== null && value !== "") search.set(key, String(value));
  }
  const text = search.toString();
  return text ? `?${text}` : "";
};

export const api = {
  // ---- identity
  me: () => request("/me"),
  login: (username, password) =>
    request("/login", { method: "POST", body: body({ username, password }) }),
  loginGuest: () => request("/login/guest", { method: "POST" }),
  logout: () => request("/logout", { method: "POST" }),

  // ---- shape of the instance
  ecosystems: () => request("/ecosystems"),
  projects: () => request("/projects"),
  createProject: (name) => request("/projects", { method: "POST", body: body({ name }) }),
  patchProject: (project, patch) =>
    request(at(project), { method: "PATCH", body: body(patch) }),
  deleteProject: (project) => request(at(project), { method: "DELETE" }),

  // ---- who else may reach a project
  grants: (project) => request(`${at(project)}/grants`),
  // PUT rather than POST: granting an account that already has a grant is the same
  // request as granting it the first time, so the caller never has to know which.
  setGrant: (project, name, level) =>
    request(`${at(project)}/grants/${encodeURIComponent(name)}`, {
      method: "PUT",
      body: body({ level }),
    }),
  deleteGrant: (project, name) =>
    request(`${at(project)}/grants/${encodeURIComponent(name)}`, { method: "DELETE" }),

  // ---- content
  artifacts: (project, { eco = "", q = "", sort = "date", page = 1 } = {}) =>
    request(`${at(project)}/artifacts${params({ eco, q, sort, page })}`),
  stats: (project) => request(`/stats${params({ project })}`),

  // ---- time series
  //
  // `span` is advisory. The server answers at the finest resolution that still
  // exists for the window and reports which one it used, so a month-long request
  // for five-minute buckets comes back hourly rather than empty.
  series: ({ project, eco, span = "auto", from, to, by = "outcome" } = {}) =>
    request(`/stats/series${params({ project, eco, span, from, to, by })}`),
  storage: ({ from, to } = {}) => request(`/stats/storage${params({ from, to })}`),
  upstreamHealth: ({ project, from, to } = {}) =>
    request(`/stats/upstreams${params({ project, from, to })}`),
  ages: (project) => request(`/stats/ages${params({ project })}`),

  // ---- reaching the cache
  coordinates: () => request("/coordinates"),
  downloads: () => request("/downloads"),
  endpoints: (project) => request(`${at(project)}/endpoints`),
  tokens: (project) => request(`/tokens${params({ project })}`),
  createToken: (value) => request("/tokens", { method: "POST", body: body(value) }),
  deleteToken: (id) => request(`/tokens/${encodeURIComponent(id)}`, { method: "DELETE" }),

  // ---- where misses go
  upstreams: (project) => request(`${at(project)}/upstreams`),
  addUpstream: (project, value) =>
    request(`${at(project)}/upstreams`, { method: "POST", body: body(value) }),
  patchUpstream: (project, id, patch) =>
    request(`${at(project)}/upstreams/${id}`, { method: "PATCH", body: body(patch) }),
  deleteUpstream: (project, id) =>
    request(`${at(project)}/upstreams/${id}`, { method: "DELETE" }),

  // ---- moving a cache
  snapshots: (project) => request(`${at(project)}/snapshots`),
  checkpoint: (project, message) =>
    request(`${at(project)}/snapshots`, { method: "POST", body: body({ message }) }),
  rollback: (project, snapshot) =>
    request(`${at(project)}/snapshots/${encodeURIComponent(snapshot)}/rollback`, {
      method: "POST",
    }),
  exportPack: (project, value) =>
    request(`${at(project)}/export`, { method: "POST", body: body(value) }),
  importPack: (project, value) =>
    request(`${at(project)}/import`, { method: "POST", body: body(value) }),
  lockwarm: (project, value) =>
    request(`${at(project)}/lockwarm`, { method: "POST", body: body(value) }),

  // ---- governance
  jobs: () => request("/jobs"),
  // One job with its full persisted log. The list endpoint omits the log, and the log
  // is where a dry run reports what it would have removed.
  job: (id) => request(`/jobs/${id}`),
  cancelJob: (id) => request(`/jobs/${id}`, { method: "DELETE" }),
  audit: () => request("/audit"),
  users: () => request("/users"),
  createUser: (value) => request("/users", { method: "POST", body: body(value) }),
  patchUser: (name, patch) =>
    request(`/users/${encodeURIComponent(name)}`, { method: "PATCH", body: body(patch) }),
  deleteUser: (name) => request(`/users/${encodeURIComponent(name)}`, { method: "DELETE" }),
  gc: (dryRun) => request("/maintenance/gc", { method: "POST", body: body({ dry_run: dryRun }) }),
  evict: (project, dryRun) =>
    request(`${at(project)}/maintenance/evict`, { method: "POST", body: body({ dry_run: dryRun }) }),

  /** Subscribe to the event bus. Returns an unsubscribe function. */
  events(receive, onState) {
    const source = new EventSource(`${V1}/events`, { withCredentials: true });
    const handle = (raw) => {
      try {
        receive(JSON.parse(raw.data));
      } catch {
        // One malformed frame does not justify tearing down the stream.
      }
    };
    for (const name of ["progress", "job", "health", "audit"]) {
      source.addEventListener(name, handle);
    }
    source.onopen = () => onState(true);
    source.onerror = () => onState(false);
    return () => source.close();
  },
};
