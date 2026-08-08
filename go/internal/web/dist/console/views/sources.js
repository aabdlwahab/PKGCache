/* Sources — "where do misses go, and is that working?"
 *
 * Upstreams, peers and the offline switch share a page because they are one
 * decision: what happens when the cache does not already have the bytes. */

import { el, region, panel, fill, table, button, field, input, select, loading } from "../dom.js";
import { api } from "../api.js";
import * as store from "../store.js";
import * as charts from "../charts.js";
import { bytes, count, percent, duration, ecoColor } from "../format.js";

export default {
  mount(node) {
    const offline = region("div");
    const list = region("div");
    const health = region("div");

    fill(
      node,
      el("div", { class: "view-head" },
        el("h1", { text: "Sources" }),
        el("p", { class: "note", text: `Where misses in ${store.state.project} are resolved.` })),
      el("div", { class: "panel-grid" },
        panel("Offline", { note: "serve from cache only" }, offline.node),
        panel("Upstream health", { note: "hourly, last 24h — mean and max only", wide: true }, health.node)),
      panel("Upstreams and peers", { note: "tried in priority order", wide: true }, list.node),
    );

    const draw = () => {
      offline.set(renderOffline());
      list.set(renderUpstreams());
    };
    const unsubscribe = [store.on(["upstreams", "projects", "project", "ecosystems"], draw)];
    draw();
    health.set(loading("Reading upstream health"));

    let cancelled = false;
    (async () => {
      try {
        const from = new Date(Date.now() - 24 * 3600 * 1000).toISOString();
        const result = await api.upstreamHealth({ project: store.state.project, from });
        if (!cancelled) health.set(renderHealth(result.points || []));
      } catch (cause) {
        if (!cancelled) store.fail(cause);
      }
    })();

    return {
      teardown() {
        cancelled = true;
        unsubscribe.forEach((off) => off());
      },
    };
  },
};

function renderOffline() {
  if (!store.hasLoaded("projects")) return loading("Reading project state");
  const project = store.state.projects.find((p) => p.name === store.state.project);
  const isOffline = Boolean(project?.offline);
  const canOperate = store.canOperate();

  return el(
    "div",
    { class: "stack" },
    el("div", { class: "status-line" },
      el("span", { class: `pill ${isOffline ? "warning" : "good"}`, text: isOffline ? "offline" : "online" }),
      el("span", { class: "note", text: isOffline
        ? "Misses fail instead of reaching an upstream. Cached content still serves."
        : "Misses are fetched from the sources below." })),
    canOperate
      ? button(isOffline ? "Go online" : "Go offline",
          async () => {
            // Going offline is fleet-wide and takes effect immediately: it is the one
            // switch here that changes what every machine using this project sees.
            // Coming back online restores the default, so only the outbound trip asks.
            if (!isOffline && !confirm(
              `Take ${store.state.project} offline?\n\n` +
              "Every machine using this project stops reaching upstreams — misses fail " +
              "instead of being fetched. Cached content still serves.\n" +
              "You can bring it back online from this page at any time.",
            )) return;
            await store.mutate(
              () => api.patchProject(store.state.project, { offline: !isOffline }),
              isOffline ? "Project is online" : "Project is offline");
          },
          { kind: isOffline ? "primary" : "danger" })
      : el("p", { class: "note", text: "Only the project owner or a superuser can change this." }),
  );
}

function renderHealth(points) {
  if (!points.length) {
    return el("p", { class: "empty", text: "No upstream traffic in the last 24 hours." });
  }
  // Fold the hourly buckets into one row per upstream: the question here is which
  // upstream is slow or failing, not what it was doing at 3am.
  const byName = new Map();
  for (const point of points) {
    const row = byName.get(point.upstream) || {
      upstream: point.upstream, requests: 0, errors: 0, bytes: 0, weighted: 0, max_ms: 0,
    };
    row.requests += point.requests;
    row.errors += point.errors;
    row.bytes += point.bytes;
    // Re-weight the per-bucket means by their own request counts; averaging the
    // averages would let a quiet hour count as much as a busy one.
    row.weighted += point.mean_ms * point.requests;
    row.max_ms = Math.max(row.max_ms, point.max_ms);
    byName.set(point.upstream, row);
  }
  const rows = [...byName.values()]
    .map((row) => ({ ...row, mean_ms: row.requests ? row.weighted / row.requests : 0 }))
    .sort((a, b) => b.requests - a.requests);

  return table(
    [
      { label: "Upstream", cell: (row) => row.upstream },
      { label: "Requests", numeric: true, cell: (row) => count(row.requests) },
      {
        label: "Errors",
        cell: (row) =>
          el("span", { class: "inline" },
            charts.segmentedBar([
              { label: "ok", value: row.requests - row.errors, color: "var(--status-good)" },
              { label: "errors", value: row.errors, color: "var(--status-critical)" },
            ], { format: count }),
            el("span", { class: "note", text: percent(row.errors, row.requests) })),
      },
      { label: "Mean", numeric: true, cell: (row) => duration(row.mean_ms) },
      { label: "Slowest", numeric: true, cell: (row) => duration(row.max_ms) },
      { label: "Fetched", numeric: true, cell: (row) => bytes(row.bytes) },
    ],
    rows,
  );
}

function renderUpstreams() {
  // "No upstreams. Misses have nowhere to go." is a real diagnosis, and stating it
  // before the list has arrived would be a false alarm about a broken project.
  if (!store.hasLoaded("upstreams")) return loading("Reading upstreams");
  const rows = (store.state.upstreams || []).slice().sort((a, b) => a.priority - b.priority);
  const canOperate = store.canOperate();

  const ecoOptions = store.state.ecosystems.map((d) => ({ value: d.id, label: d.display }));
  const form = el(
    "form",
    { class: "form row-form" },
    field("Ecosystem", select("eco", ecoOptions, ecoOptions[0]?.value)),
    field("Name", input("name", { required: true, placeholder: "pypi.org" })),
    field("URL", input("url", { required: true, type: "url", placeholder: "https://pypi.org/simple" })),
    field("Kind", select("kind", [
      { value: "origin", label: "origin" },
      { value: "peer", label: "peer" },
      { value: "mirror", label: "mirror" },
    ], "origin")),
    field("Priority", input("priority", { type: "number", value: "10" }),
      "lower is tried first"),
    field("Token", input("credential", { type: "password", placeholder: "optional" }),
      "stored encrypted; never returned"),
    el("button", { class: "btn primary", type: "submit", text: "Add" }),
  );

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const data = new FormData(form);
    const token = String(data.get("credential") || "");
    const name = String(data.get("name"));
    await store.mutate(
      () => api.addUpstream(store.state.project, {
        eco: String(data.get("eco")),
        name,
        url: String(data.get("url")),
        kind: String(data.get("kind")),
        priority: Number(data.get("priority")),
        enabled: true,
        ...(token ? { credential: { label: name, kind: "bearer", token } } : {}),
      }),
      `Added ${name}`,
    );
    form.reset();
  });

  return el(
    "div",
    { class: "stack" },
    table(
      [
        { label: "Priority", numeric: true, cell: (row) => row.priority },
        { label: "Eco", cell: (row) =>
          el("span", { class: "badge" },
            el("span", { class: "swatch", style: `background:${ecoColor(row.eco)}` }),
            el("span", { text: row.eco })) },
        { label: "Name", cell: (row) => row.name },
        { label: "Kind", cell: (row) => el("span", { class: `pill ${row.kind}`, text: row.kind }) },
        { label: "URL", cell: (row) => el("code", { text: row.url, title: row.url }) },
        {
          label: "State",
          cell: (row) =>
            canOperate
              ? button(row.enabled ? "Disable" : "Enable",
                  () => store.mutate(
                    () => api.patchUpstream(store.state.project, row.id, { enabled: !row.enabled }),
                    `${row.name} ${row.enabled ? "disabled" : "enabled"}`),
                  { kind: "ghost small" })
              : el("span", { class: "pill", text: row.enabled ? "enabled" : "disabled" }),
        },
        {
          label: "",
          cell: (row) =>
            canOperate
              ? button("Remove",
                  async () => {
                    if (!confirm(
                      `Remove the upstream ${row.name}?\n\n` +
                      "Misses that would have been fetched from it start failing unless " +
                      "another upstream covers the same ecosystem. Cached content is untouched.",
                    )) return;
                    await store.mutate(
                      () => api.deleteUpstream(store.state.project, row.id), `Removed ${row.name}`);
                  },
                  { kind: "ghost small danger" })
              : "—",
        },
      ],
      rows,
      { empty: "No upstreams. Misses in this project have nowhere to go." },
    ),
    canOperate ? form : null,
  );
}
