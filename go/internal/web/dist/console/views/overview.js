/* Overview — "is it healthy, and what is it doing?"
 *
 * The one page that should answer a normal day's question without a click. */

import { el, region, panel, fill, loading, cacheKey } from "../dom.js";
import { api } from "../api.js";
import * as store from "../store.js";
import * as charts from "../charts.js";
import {
  bytes, count, percent, ago, clock,
  outcomeLabel, outcomeRank, outcomeColor, passedThrough,
} from "../format.js";

export default {
  mount(node) {
    const kpis = region("div", { class: "kpi-grid" });
    const traffic = region("div");
    const disk = region("div");
    const growth = region("div");
    const activity = region("div");
    // Asked once: which binary this is does not change while a page is open, and the
    // panel notes below are written in its terms.
    const local = store.isLocal();

    fill(
      node,
      el("div", { class: "view-head" },
        el("h1", { text: "Overview" }),
        el("p", { class: "note", text: `project ${store.state.project}` })),
      kpis.node,
      el("div", { class: "panel-grid" },
        panel("Request outcomes", { note: "column height is volume, composition is where it came from", wide: true }, traffic.node),
        // The header says "project X", and the hit rate beside these is that project's.
        // Disk and growth are not: blobs are shared across projects by digest, so there
        // is no per-project size to report — and the panel has to say so, or a reader
        // with several projects takes the whole store for this one's share of it.
        panel("Disk", {
          note: local
            ? "whole cache, measured now, against its limit and free-space floor"
            : "whole cache, measured now, against the eviction floor",
        }, disk.node),
        panel("Storage growth", { note: "whole cache, sampled hourly" }, growth.node),
        panel("Recent activity", { note: "from the event bus" }, activity.node)),
    );

    const drawKPIs = () => kpis.set(renderKPIs());
    const drawActivity = () => activity.set(renderActivity());
    const drawDisk = () => disk.set(renderDisk());

    const unsubscribe = [
      // projects and coordinates carry the two offline switches the Policy tile reads.
      store.on(["stats", "jobs", "live", "storage", "projects", "coordinates"], drawKPIs),
      store.on(["recent", "live"], drawActivity),
      store.on("storage", drawDisk),
    ];

    drawKPIs();
    drawActivity();
    drawDisk();
    // These two are fetched below rather than read from the store, so they start as a
    // loading state instead of an empty panel body.
    traffic.set(loading("Reading request outcomes"));
    growth.set(loading("Reading storage history"));

    // The series and the growth history are not part of the shared store: only this
    // view reads them, and refetching them on every bus frame would be wasteful.
    let cancelled = false;
    const loadSeries = async () => {
      try {
        const [series, storage] = await Promise.all([
          api.series({ project: store.state.project, by: "outcome" }),
          api.storage(),
        ]);
        if (cancelled) return;
        traffic.set(renderTraffic(series));
        growth.set(renderGrowth(storage));
      } catch (cause) {
        if (!cancelled) store.fail(cause);
      }
    };
    void loadSeries();
    // One slow refresh: the series is written by a 30-second flush, so anything
    // faster would redraw the same picture.
    const timer = setInterval(loadSeries, 60_000);

    return {
      teardown() {
        cancelled = true;
        clearInterval(timer);
        unsubscribe.forEach((off) => off());
      },
    };
  },
};

function kpi(label, value, sub, title) {
  return el(
    "div",
    { class: "kpi", title },
    el("span", { class: "kpi-label", text: label }),
    el("strong", { class: "kpi-value", text: value }),
    el("span", { class: "kpi-sub", text: sub }),
  );
}

function renderKPIs() {
  // Six zero-valued tiles read as a measurement, not as "not read yet".
  if (!store.hasLoaded("stats")) return loading("Reading project statistics");
  const stats = store.state.stats;
  const storage = store.state.storage;
  const byEco = stats?.by_eco || [];
  const requests = byEco.reduce((sum, row) => sum + row.hit_count + row.miss_count, 0);
  const hits = byEco.reduce((sum, row) => sum + row.hit_count, 0);
  const running = store.state.jobs.filter((job) => job.status === "running").length;

  return [
    // Stored and Deduplicated are the whole store's, not this project's: the catalog
    // totals them across every project, since one blob can back entries in several.
    kpi("Stored", bytes(storage?.blob_bytes ?? stats?.total_bytes ?? 0),
      `${count(storage?.blob_count ?? stats?.total_blobs ?? 0)} blobs, whole cache`),
    // The saving is the whole point of a content-addressed store, and nothing
    // surfaced it before.
    kpi("Deduplicated", storage ? bytes(Math.max(0, storage.entry_bytes - storage.blob_bytes)) : "—",
      storage?.entry_bytes
        ? `${percent(storage.entry_bytes - storage.blob_bytes, storage.entry_bytes)} of logical, whole cache`
        : "whole cache"),
    // The lifetime counters behind this keep two columns, hits (local, deduplicated and
    // peer) and misses, so that is what the number under it counts. It used to say
    // "requests", which a reader compares with the traffic chart's total and finds short
    // by every failure and every download served without being kept.
    kpi("Hit rate", requests ? percent(hits, requests) : "—",
      `${count(requests)} hits and misses, all time`,
      "Hits include deduplicated and peer answers. Failed requests, and downloads served " +
        "without being kept, are in the request outcomes chart but not in this rate."),
    kpi("In flight", count(store.state.live.size), "live transfers"),
    kpi("Jobs", count(running), "running, all projects"),
    policyKPI(),
  ];
}

/** Whether this project reaches upstream, and if not, which switch says so.
 *
 * There are two, and they are undone in different places. A project's own flag is a
 * setting in this console. The instance's — `serve -offline`, or upstream.offline in the
 * config — holds every project offline whatever its own flag says, and only a restart
 * lifts it. Reading only the project's flag drew "online" on an instance that could not
 * reach anything, which is the one state where the reader most needs the truth. */
function policyKPI() {
  const project = store.state.projects.find((p) => p.name === store.state.project);
  const auth = project?.data_plane_auth || "public";
  if (store.state.coordinates?.offline === true) {
    return kpi("Policy", "offline", `whole instance · ${auth}`,
      "Started offline: every project serves only what it already holds, until the " +
        "process is restarted without -offline.");
  }
  if (project?.offline) {
    return kpi("Policy", "offline", `this project · ${auth}`,
      "This project serves only what it already holds. Other projects still reach upstream.");
  }
  return kpi("Policy", "online", auth);
}

function renderTraffic(series) {
  const points = series.points || [];
  if (!points.length) {
    return charts.emptyPlot("No traffic recorded in this window yet.");
  }
  const buckets = charts.toBuckets(
    points,
    (point) => point.outcome,
    (point) => outcomeColor(point.outcome),
    (point) => outcomeLabel(point.outcome),
  );
  // Stack in pipeline order so the picture reads the same way every time: closest
  // content at the bottom, upstream and failure on top.
  const byRank = (a, b) => outcomeRank(a) - outcomeRank(b) || a.localeCompare(b);
  for (const bucket of buckets) {
    bucket.parts.sort((a, b) => byRank(a.key, b.key));
  }

  const totals = new Map();
  for (const point of points) {
    totals.set(point.outcome, (totals.get(point.outcome) || 0) + point.count);
  }
  const grand = [...totals.values()].reduce((sum, value) => sum + value, 0);
  // Legend and table are built from what is in the data, not from the outcomes this
  // file knows. Filtering by a known list is how "uncached" went missing from both
  // while its requests still counted in the total, so the shares summed to less than
  // the whole and the missing slice had no name.
  const outcomes = [...totals.keys()].sort(byRank);
  const legend = outcomes.map((outcome) => ({
    label: outcomeLabel(outcome),
    color: outcomeColor(outcome),
  }));

  return charts.figure(
    `Last ${describeWindow(series)}`,
    {
      note: series.span >= 86400 ? "daily buckets" : series.span >= 3600 ? "hourly buckets" : "5-minute buckets",
      legend,
      columns: [
        { label: "Outcome", cell: (row) => outcomeLabel(row.outcome) },
        { label: "Requests", numeric: true, cell: (row) => count(row.value) },
        { label: "Share", numeric: true, cell: (row) => percent(row.value, grand) },
      ],
      rows: outcomes.map((outcome) => ({ outcome, value: totals.get(outcome) })),
    },
    charts.stackedColumns(buckets, {
      step: series.span, format: count,
      tick: (at) => clock(at, series.span),
    }),
  );
}

function describeWindow(series) {
  const hours = Math.round((Date.parse(series.to) - Date.parse(series.from)) / 3_600_000);
  if (hours < 48) return `${hours} hours`;
  return `${Math.round(hours / 24)} days`;
}

function renderDisk() {
  if (!store.hasLoaded("storage")) return loading("Measuring disk");
  const storage = store.state.storage;
  if (storage?.budget) return renderBudget(storage, storage.budget);
  if (!storage || !storage.fs_total) {
    return el("p", { class: "empty", text: "No filesystem reading available." });
  }
  const local = store.isLocal();
  const used = storage.fs_total - storage.fs_free;
  return el(
    "div",
    {},
    charts.meter(used, storage.fs_total, {
      threshold: storage.min_free_bytes,
      thresholdLabel: "eviction floor",
      format: bytes,
    }),
    el(
      "dl",
      { class: "facts" },
      fact("Blobs on disk", bytes(storage.blob_bytes)),
      fact("Logical size", bytes(storage.entry_bytes)),
      fact("Entries", count(storage.entry_count)),
      // A pkgcache that reports no budget is one older than the field, and it still
      // never evicts — so it is not told it lacks an eviction floor.
      local
        ? fact("Evicted", "never; pkgcache prune reclaims space when you ask")
        : storage.min_free_bytes
          ? fact("Eviction floor", `${bytes(storage.min_free_bytes)} free`)
          : fact("Eviction floor", "not configured"),
    ),
  );
}

/* pkgcache's disk policy, which is not a server's.
 *
 * Nothing is evicted. There is a limit somebody chose with `pkgcache limit` (or chose not
 * to have), a free-space floor under it, and — once either is reached — a cache that
 * keeps serving and stops storing. The eviction floor a server reports is always zero
 * here (config/local.go), so drawing against it told a cache whose policy is set that it
 * had none, and a full one looked exactly like a healthy one. */
function renderBudget(storage, budget) {
  const limit = budget.limit_bytes > 0 ? budget.limit_bytes : 0;
  const floor = budget.min_free_bytes;
  // With a limit, the gauge is the cache against it: that is the number the guard
  // compares. With none, it is the disk, read against the floor the guard keeps free.
  // The two gauges look alike and measure different things, so each says which it is.
  let gauge = null;
  if (limit) {
    gauge = [
      el("p", { class: "note", text: "This cache, against the limit set with pkgcache limit" }),
      charts.meter(budget.used_bytes, limit, {
        format: bytes, remaining: "left under the limit", full: budget.full,
      }),
    ];
  } else if (storage.fs_total) {
    gauge = [
      el("p", { class: "note", text: "No limit set, so this is the whole disk, against the floor pkgcache keeps free" }),
      charts.meter(storage.fs_total - storage.fs_free, storage.fs_total, {
        threshold: floor, thresholdLabel: "free-space floor", format: bytes, full: budget.full,
      }),
    ];
  }
  return el(
    "div",
    { class: "stack" },
    budget.full ? fullNotice(budget.reason) : null,
    gauge,
    el(
      "dl",
      { class: "facts" },
      fact("Limit", limit ? bytes(limit) : "none; the floor still applies"),
      fact("Cache on disk", bytes(budget.used_bytes)),
      fact("Free-space floor", `${bytes(floor)} kept free, ${bytes(budget.free_bytes)} free now`),
      fact("Logical size", bytes(storage.entry_bytes)),
      fact("Evicted", "never; pkgcache prune reclaims space when you ask"),
    ),
  );
}

/** The one state on this page that means the cache has stopped doing its job. The
 *  daemon's own sentence is shown as written — it already names the numbers and the
 *  commands that get out of it, and a paraphrase here would be a second thing to keep
 *  true. */
function fullNotice(reason) {
  // One child, so the heading and the sentence flow as a paragraph: .alert is a flex row,
  // and as two children the heading was squeezed into a column one word wide.
  return el(
    "div",
    { class: "alert error", role: "status" },
    el("span", {},
      el("strong", { text: "Full: not storing new content. " }),
      sentence(reason || "still serving what it holds, and keeping nothing new")),
  );
}

function sentence(text) {
  const trimmed = String(text).trim();
  const capital = trimmed.charAt(0).toUpperCase() + trimmed.slice(1);
  return /[.!?]$/.test(capital) ? capital : `${capital}.`;
}

function fact(label, value) {
  return el("div", { class: "fact" },
    el("dt", { text: label }), el("dd", { text: value }));
}

function renderGrowth(storage) {
  const samples = storage.samples || [];
  if (samples.length < 2) {
    return charts.emptyPlot("Growth needs a couple of hours of samples.");
  }
  const series = samples.map((sample) => ({
    at: new Date(sample.bucket),
    value: sample.blob_bytes,
  }));
  const capacity = storage.current?.fs_total || 0;

  return charts.figure(
    "Bytes on disk",
    {
      note: "gaps are unsampled, not empty",
      columns: [
        { label: "When", cell: (row) => new Date(row.bucket).toLocaleString() },
        { label: "Stored", numeric: true, cell: (row) => bytes(row.blob_bytes) },
        { label: "Free", numeric: true, cell: (row) => bytes(row.fs_free) },
      ],
      rows: samples.slice(-24).reverse(),
    },
    charts.lineChart(series, {
      step: 3600,
      format: bytes,
      tick: (at) => clock(at, 3600),
      reference: capacity ? { value: capacity, label: "filesystem capacity" } : undefined,
    }),
  );
}

function renderActivity() {
  const recent = store.state.recent;
  if (!recent.length && !store.state.live.size) {
    return el("p", { class: "empty", text: "Nothing has happened since this page opened." });
  }
  return el(
    "ul",
    { class: "event-list" },
    [...store.state.live.values()].slice(0, 5).map((event) =>
      el("li", { class: "event" },
        el("span", { class: "dot pulse" }),
        cacheKey(event.id),
        el("span", { class: "note", text: `${event.eco || ""} · in flight` })),
    ),
    recent.slice(0, 12).map(activityRow),
  );
}

/* One finished request. Events carry `time` (obs.Event); this read `at`, which no event
 * has ever had, so no row said when it happened.
 *
 * A download pkgcache served without keeping arrives as an ordinary fetch.done, and was
 * drawn as a green "ok" beside the ones that were stored — on the one page that should
 * make a full cache obvious. It gets the outcome's own colour and its words, with the
 * daemon's reason on hover. */
function activityRow(event) {
  const uncached = passedThrough(event);
  const failed = event.kind === "fetch.error";
  // A download somebody stopped is not a failure anybody needs to look into.
  const cancelled = failed && event.status === "cancelled";
  let dot = el("span", { class: "dot ok" });
  let word = null;
  if (uncached) {
    dot = el("span", { class: "dot", style: `background:${outcomeColor("uncached")}` });
    word = outcomeLabel("uncached");
  } else if (cancelled) {
    dot = el("span", { class: "dot" });
    word = "cancelled";
  } else if (failed) {
    dot = el("span", { class: "dot bad" });
    word = outcomeLabel("fail");
  }
  return el(
    "li",
    { class: "event", title: (uncached || failed) && event.detail ? event.detail : null },
    dot,
    cacheKey(event.id || event.kind),
    el("span", {
      class: "note",
      text: [event.eco, word, event.size ? bytes(event.size) : null, event.time ? ago(event.time) : null]
        .filter(Boolean).join(" · "),
    }),
  );
}
