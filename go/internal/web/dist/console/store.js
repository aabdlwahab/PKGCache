/* One state object, explicit subscriptions, and an SSE reducer.
 *
 * Views subscribe to the keys they draw. A view that renders jobs is not woken by a
 * download-progress frame — which matters because the bus is chatty during a `uv
 * sync` and re-rendering everything on every frame would make the page unusable at
 * exactly the moment someone is watching it.
 */

import { api } from "./api.js";

const LIVE_LIMIT = 40;
const RECENT_LIMIT = 30;

export const state = {
  me: null,
  project: localStorage.getItem("pkgreg-project") || "global",
  theme: localStorage.getItem("pcc_theme") || "dark",

  ecosystems: [],
  projects: [],
  coordinates: null,
  downloads: [],

  stats: null,
  artifacts: { rows: [], total: 0 },
  ages: [],
  series: null,
  storage: null,
  upstreamHealth: [],

  endpoints: {},
  onboarding: null,
  tokens: [],
  upstreams: [],
  snapshots: [],
  users: [],
  audit: [],
  jobs: [],
  /* Who else may reach a project, carried with the project it was read for so a slow
     answer cannot paint over the project the reader has since switched to. */
  grants: null,

  live: new Map(),
  recent: [],
  connected: false,
  health: "starting",
  error: "",
  notice: "",
};

const listeners = new Map();

/** Subscribe to one or more state keys. Returns an unsubscribe function. */
export function on(keys, fn) {
  const names = Array.isArray(keys) ? keys : [keys];
  for (const name of names) {
    if (!listeners.has(name)) listeners.set(name, new Set());
    listeners.get(name).add(fn);
  }
  return () => names.forEach((name) => listeners.get(name)?.delete(fn));
}

/* Keys the views iterate over. The server now guarantees arrays, but a nil slice in
 * Go marshals to null, so this is one regression away from returning — and the failure
 * mode is a blank page with `Cannot read properties of null (reading 'filter')` in a
 * console nobody has open. The store refuses to hold the wrong type instead. */
const ARRAY_KEYS = new Set([
  "ecosystems", "projects", "downloads", "tokens", "upstreams", "snapshots",
  "users", "audit", "jobs", "recent", "ages",
]);

/** Apply a patch and wake only the subscribers of the keys that actually changed.
 *
 * The equality check is load-bearing, not an optimisation. Every refresh re-sets the
 * current project name, and waking that key remounts the whole view — so without this,
 * an ordinary reload tore down and rebuilt the page for a value that had not moved.
 * Nothing in the store is mutated in place, so identity is the right test. */
export function set(patch) {
  const woken = new Set();
  for (const [key, value] of Object.entries(patch)) {
    const next = ARRAY_KEYS.has(key) && !Array.isArray(value) ? [] : value;
    if (Object.is(state[key], next)) continue;
    state[key] = next;
    for (const fn of listeners.get(key) || []) woken.add(fn);
  }
  for (const fn of woken) fn(state);
}

export function fail(error) {
  set({ error: error?.message || String(error), notice: "" });
}

export function notify(notice) {
  set({ notice, error: "" });
}

export function setProject(name) {
  localStorage.setItem("pkgreg-project", name);
  // The project-scoped slices now describe the wrong project, so they stop counting as
  // loaded until loadProject() refills them. Otherwise the new project's views would
  // render the previous one's emptiness as though it were their own.
  for (const key of ["stats", "storage", "endpoints", "tokens", "upstreams", "snapshots"]) {
    loaded.delete(key);
  }
  set({ project: name });
}

export function setTheme(theme) {
  document.documentElement.dataset.theme = theme;
  localStorage.setItem("pcc_theme", theme);
  set({ theme });
}

/* Which slices have actually come back from the server.
 *
 * The initial values above cannot answer this: `jobs: []` and `upstreams: []` look
 * exactly like a real empty result, so every view rendered its empty state during the
 * first fetch and told the reader "Nothing cached yet." about a cache it had not looked
 * at. An operator who reads that on a 40,000-artifact instance has been given a false
 * statement, which is worse than a blank panel. */
const loaded = new Set();

function markLoaded(...keys) {
  for (const key of keys) loaded.add(key);
}

/** True only once every named slice has been fetched at least once for this project. */
export function hasLoaded(...keys) {
  return keys.every((key) => loaded.has(key));
}

/** A guest session: signed in, read-only, global project only. */
export function isGuest() {
  return state.me?.guest === true;
}

/** Whether the signed-in actor may change things in the current project.
 *
 * This mirrors the server's rule exactly — superuser, the project's owner, or an
 * explicit operate grant. It used to answer true for any admin, which was wrong in the
 * one direction that matters: an admin was shown every control on a project they did
 * not own, and found out only from the 403 the button produced. */
export function canOperate() {
  const me = state.me;
  /* Checked before the open-mode shortcut below, not after. A guest exists only on an
     instance that enforces authentication, but reading the guard in the wrong order is
     how a read-only session acquires buttons that 403 when pressed. */
  if (isGuest()) return false;
  if (!me || !me.auth_enabled) return true;
  if (!me.authenticated) return false;
  if (me.role === "superuser") return true;
  const project = state.projects.find((p) => p.name === state.project);
  if (project && project.owner === me.username) return true;
  return me.grants?.[state.project] === "operate";
}

/** Whether the actor may change who else can reach the current project. Deliberately
 *  narrower than canOperate: a grantee works on the project, but only the owner (or a
 *  superuser) decides who joins them. */
export function canGrant() {
  const me = state.me;
  if (isGuest()) return false;
  if (!me || !me.auth_enabled) return false;
  if (me.role === "superuser") return true;
  const project = state.projects.find((p) => p.name === state.project);
  return Boolean(me.authenticated && project && project.owner === me.username);
}

export function isSuperuser() {
  const me = state.me;
  if (isGuest()) return false;
  return !me?.auth_enabled || me?.role === "superuser";
}

/** Load what every view depends on.
 *
 * A guest is served a narrower set, matching the route allowlist the server enforces.
 * Asking for the rest and swallowing the failures would work, but it would put a row
 * of 403s in everyone's network tab and make a real permission bug indistinguishable
 * from normal operation. */
export async function loadInstance() {
  const guest = isGuest();
  const [ecosystems, projects, jobs, coordinates, downloads] = await Promise.all([
    api.ecosystems(),
    api.projects(),
    guest ? Promise.resolve({ jobs: [] }) : api.jobs(),
    api.coordinates(),
    api.downloads(),
  ]);
  // Read through a default here as well as in set(): this one runs before the store
  // sees the value, so a null would throw on .map rather than being coerced.
  const rows = projects.projects ?? [];
  const names = rows.map((p) => p.name);
  const project = names.includes(state.project) ? state.project : (names[0] ?? "global");
  markLoaded("ecosystems", "projects", "jobs", "coordinates", "downloads");
  set({
    ecosystems: ecosystems.ecosystems,
    projects: rows,
    jobs: jobs.jobs,
    coordinates,
    downloads: downloads.downloads,
    project,
  });
}

/** Load the current project's slice. Views call this after any mutation. */
export async function loadProject() {
  const project = state.project;
  const guest = isGuest();
  const empty = (key) => Promise.resolve({ [key]: [] });
  const [stats, endpoints, tokens, upstreams, snapshots] = await Promise.all([
    api.stats(project),
    api.endpoints(project),
    // Tokens, upstreams and snapshots are outside a guest's allowlist: they carry
    // operational secrets, internal mirror hostnames and air-gap cadence.
    guest ? empty("tokens") : api.tokens(project),
    guest ? empty("upstreams") : api.upstreams(project),
    guest ? empty("snapshots") : api.snapshots(project),
  ]);
  // A project switch mid-flight must not overwrite the new project's data with the
  // old one's; the switch is the newer intent.
  if (state.project !== project) return;
  markLoaded("stats", "storage", "endpoints", "tokens", "upstreams", "snapshots");
  set({
    stats,
    storage: stats.storage,
    endpoints: endpoints.endpoints,
    onboarding: endpoints.onboarding ?? null,
    tokens: tokens.tokens,
    upstreams: upstreams.upstreams,
    snapshots: snapshots.snapshots,
  });
}

export async function refreshJobs() {
  if (isGuest()) return;
  set({ jobs: (await api.jobs()).jobs });
}

/* ---- event coalescing -------------------------------------------------------
 *
 * The server publishes one fetch.progress frame per 64 KiB copy-buffer chunk, so a
 * single container layer is thousands of frames. Applying each one directly meant a
 * full replaceChildren of the rail, the KPI grid and the activity list per chunk —
 * the page janked hardest at exactly the moment someone opened it to watch a
 * transfer. Worse, a browser that cannot keep up makes the server declare it a slow
 * consumer and close the stream, so the render cost was also causing data loss.
 *
 * Progress frames are therefore accumulated and applied once per animation frame:
 * the newest value for each transfer wins, and one flush wakes the subscribers once
 * no matter how many chunks arrived. Terminal frames (done/error/hit) still apply
 * immediately — those are the ones an operator is waiting to see. */
const pendingLive = new Map();
let liveFrame = 0;

function flushLive() {
  if (liveFrame) return;
  liveFrame = requestAnimationFrame(() => {
    liveFrame = 0;
    if (!pendingLive.size) return;
    const live = new Map(state.live);
    for (const [id, event] of pendingLive) live.set(id, event);
    pendingLive.clear();
    // A burst can open more transfers than anyone can read. Keep the newest;
    // the counter in the rail still reports the true total.
    while (live.size > LIVE_LIMIT) live.delete(live.keys().next().value);
    set({ live });
  });
}

/* A completed fetch changes the stats tile and nothing else: tokens, upstreams,
 * endpoints and snapshots cannot change because a cache filled. Reloading the whole
 * project slice per completion turned one `uv sync` into four figures' worth of
 * control-plane requests against SQLite while a tab was merely open. */
let statsTimer = 0;

function refreshStatsSoon() {
  if (statsTimer) return;
  statsTimer = setTimeout(async () => {
    statsTimer = 0;
    const project = state.project;
    try {
      const stats = await api.stats(project);
      if (state.project !== project) return;
      set({ stats, storage: stats.storage });
    } catch {
      // A dropped stats refresh is not worth an alert; the next completion retries,
      // and every mutation still reloads the full slice.
    }
  }, 2000);
}

/* ---- traffic level ----------------------------------------------------------
 *
 * One 0..1 reading of how busy the current project is, used by the topbar shader to
 * decide how many wave crests to draw.
 *
 * Deliberately not a state key. It moves on every frame of a `uv sync`, and pushing
 * it through set() would wake subscribers thousands of times for a value no view
 * renders — the exact cost the coalescing above exists to avoid. The shader already
 * runs on requestAnimationFrame, so it pulls this per frame instead.
 *
 * Two signals, because either one alone lies about a real workload:
 *   - request events (fetch.start / done / error / cache.hit). A warm instance serves
 *     mostly cache.hit, which never opens a live row, so reading only in-flight
 *     transfers would draw a mirror doing thousands of hits/second as idle.
 *   - concurrent in-flight transfers. One container layer is a single request event
 *     followed by minutes of copying, so reading only events would draw a saturated
 *     link as idle.
 * The level is whichever of the two reads busier.
 *
 * The event signal is a decaying score, not a windowed rate: each event adds 1 and
 * the score halves every HALF_LIFE. No timer, no ring buffer, and it decays on read.
 * A sustained r events/second settles at r * HALF_LIFE / ln2, so EVENT_SATURATION of
 * 24 puts "as busy as the bar can show" at roughly 4 requests/second. */
const HALF_LIFE = 4000;
const EVENT_SATURATION = 24;
const LIVE_SATURATION = 8;

let trafficScore = 0;
let trafficMark = 0;

function decayTraffic(now) {
  if (trafficMark) trafficScore *= Math.pow(0.5, (now - trafficMark) / HALF_LIFE);
  trafficMark = now;
}

function noteTraffic() {
  decayTraffic(performance.now());
  trafficScore += 1;
}

/** How busy the current project looks right now: 0 quiet, 1 saturated. */
export function trafficLevel() {
  decayTraffic(performance.now());
  return Math.min(1, Math.max(trafficScore / EVENT_SATURATION, state.live.size / LIVE_SATURATION));
}

/** Run a mutation with uniform error, notice and refresh handling. */
export async function mutate(operation, success) {
  try {
    const result = await operation();
    notify(success);
    await loadProject();
    await refreshJobs();
    return result;
  } catch (cause) {
    fail(cause);
    return undefined;
  }
}

/** Wire the event bus into the store. */
export function connectEvents() {
  return api.events(
    (event) => {
      if (event.kind === "health") {
        set({ health: event.status ?? event.detail ?? "ok" });
        return;
      }
      if (event.kind === "job.update") {
        // The server withholds these from a guest, whose /jobs access is refused.
        // Guarded here too so a stray frame cannot start a request that 403s.
        if (isGuest()) return;
        void refreshJobs();
        if (!event.project || event.project === state.project) void loadProject();
        return;
      }
      if (event.project && event.project !== state.project) return;

      const id = `${event.eco ?? ""}:${event.id ?? ""}`;
      if (event.kind === "fetch.start" || event.kind === "fetch.progress") {
        // Only the start counts as a request. Progress is one frame per 64 KiB chunk,
        // so counting it would peg the traffic level on a single large layer; the
        // in-flight tally in trafficLevel() is what speaks for an ongoing copy.
        if (event.kind === "fetch.start") noteTraffic();
        const existing = pendingLive.get(id) ?? state.live.get(id);
        pendingLive.set(id, { ...event, received: event.size ?? existing?.received ?? 0 });
        // A backgrounded tab stops firing animation frames while the stream keeps
        // arriving, so the queue is bounded here rather than only at flush time.
        while (pendingLive.size > LIVE_LIMIT) pendingLive.delete(pendingLive.keys().next().value);
        flushLive();
        return;
      }
      if (event.kind === "fetch.done" || event.kind === "fetch.error" || event.kind === "cache.hit") {
        noteTraffic();
        // A terminal frame ends the transfer, so drop any progress still queued for it
        // rather than letting the next flush resurrect a row that has already finished.
        pendingLive.delete(id);
        const live = new Map(state.live);
        live.delete(id);
        set({
          live,
          recent: [event, ...state.recent].slice(0, RECENT_LIMIT),
        });
        if (event.kind === "fetch.done") refreshStatsSoon();
      }
    },
    (connected) => set({ connected }),
  );
}
