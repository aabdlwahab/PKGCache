/* Overview — "is it healthy, and what is it doing?"
 *
 * The one page that should answer a normal day's question without a click. */

import { el, region, panel, fill, loading } from "../dom.js";
import { api } from "../api.js";
import * as store from "../store.js";
import * as charts from "../charts.js";
import {
  bytes, count, percent, ago, clock,
  OUTCOME_ORDER, OUTCOME_LABEL, outcomeColor,
} from "../format.js";

export default {
  mount(node) {
    const kpis = region("div", { class: "kpi-grid" });
    const traffic = region("div");
    const disk = region("div");
    const growth = region("div");
    const activity = region("div");

    fill(
      node,
      el("div", { class: "view-head" },
        el("h1", { text: "Overview" }),
        el("p", { class: "note", text: `project ${store.state.project}` })),
      kpis.node,
      el("div", { class: "panel-grid" },
        panel("Request outcomes", { note: "column height is volume, composition is where it came from", wide: true }, traffic.node),
        panel("Disk", { note: "measured now, against the eviction floor" }, disk.node),
        panel("Storage growth", { note: "sampled hourly" }, growth.node),
        panel("Recent activity", { note: "from the event bus" }, activity.node)),
    );

    const drawKPIs = () => kpis.set(renderKPIs());
    const drawActivity = () => activity.set(renderActivity());
    const drawDisk = () => disk.set(renderDisk());

    const unsubscribe = [
      store.on(["stats", "jobs", "live", "storage"], drawKPIs),
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

function kpi(label, value, sub) {
  return el(
    "div",
    { class: "kpi" },
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
  const project = store.state.projects.find((p) => p.name === store.state.project);

  return [
    kpi("Stored", bytes(storage?.blob_bytes ?? stats?.total_bytes ?? 0),
      `${count(storage?.blob_count ?? stats?.total_blobs ?? 0)} blobs`),
    // The saving is the whole point of a content-addressed store, and nothing
    // surfaced it before.
    kpi("Deduplicated", storage ? bytes(Math.max(0, storage.entry_bytes - storage.blob_bytes)) : "—",
      storage?.entry_bytes ? `${percent(storage.entry_bytes - storage.blob_bytes, storage.entry_bytes)} of logical` : "—"),
    kpi("Hit rate", requests ? percent(hits, requests) : "—", `${count(requests)} requests, all time`),
    kpi("In flight", count(store.state.live.size), "live transfers"),
    kpi("Jobs", count(running), "running"),
    kpi("Policy", project?.offline ? "offline" : "online", project?.data_plane_auth || "public"),
  ];
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
    (point) => OUTCOME_LABEL[point.outcome] || point.outcome,
  );
  // Stack in pipeline order so the picture reads the same way every time: closest
  // content at the bottom, upstream and failure on top.
  for (const bucket of buckets) {
    bucket.parts.sort((a, b) => OUTCOME_ORDER.indexOf(a.key) - OUTCOME_ORDER.indexOf(b.key));
  }

  const present = new Set(points.map((point) => point.outcome));
  const legend = OUTCOME_ORDER.filter((outcome) => present.has(outcome)).map((outcome) => ({
    label: OUTCOME_LABEL[outcome],
    color: outcomeColor(outcome),
  }));

  const totals = new Map();
  for (const point of points) {
    totals.set(point.outcome, (totals.get(point.outcome) || 0) + point.count);
  }
  const grand = [...totals.values()].reduce((sum, value) => sum + value, 0);

  return charts.figure(
    `Last ${describeWindow(series)}`,
    {
      note: series.span >= 86400 ? "daily buckets" : series.span >= 3600 ? "hourly buckets" : "5-minute buckets",
      legend,
      columns: [
        { label: "Outcome", cell: (row) => OUTCOME_LABEL[row.outcome] || row.outcome },
        { label: "Requests", numeric: true, cell: (row) => count(row.value) },
        { label: "Share", numeric: true, cell: (row) => percent(row.value, grand) },
      ],
      rows: OUTCOME_ORDER.filter((outcome) => totals.has(outcome)).map((outcome) => ({
        outcome,
        value: totals.get(outcome),
      })),
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
  if (!storage || !storage.fs_total) {
    return el("p", { class: "empty", text: "No filesystem reading available." });
  }
  const used = storage.fs_total - storage.fs_free;
  return el(
    "div",
    {},
    charts.meter(used, storage.fs_total, {
      threshold: storage.min_free_bytes,
      format: bytes,
    }),
    el(
      "dl",
      { class: "facts" },
      fact("Blobs on disk", bytes(storage.blob_bytes)),
      fact("Logical size", bytes(storage.entry_bytes)),
      fact("Entries", count(storage.entry_count)),
      storage.min_free_bytes
        ? fact("Eviction floor", `${bytes(storage.min_free_bytes)} free`)
        : fact("Eviction floor", "not configured"),
    ),
  );
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
        el("code", { text: event.id || "—" }),
        el("span", { class: "note", text: `${event.eco || ""} · in flight` })),
    ),
    recent.slice(0, 12).map((event) =>
      el("li", { class: "event" },
        el("span", { class: `dot ${event.kind === "fetch.error" ? "bad" : "ok"}` }),
        el("code", { text: event.id || event.kind }),
        el("span", { class: "note", text: [event.eco, event.size ? bytes(event.size) : null, event.at ? ago(event.at) : null].filter(Boolean).join(" · ") })),
    ),
  );
}
