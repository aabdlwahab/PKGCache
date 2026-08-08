/* Cache — "what is in it?"
 *
 * Inventory, composition, and the age histogram that previews what eviction would
 * take next. */

import { el, region, panel, fill, table, button, field, input, select, loading } from "../dom.js";
import { api } from "../api.js";
import * as store from "../store.js";
import * as charts from "../charts.js";
import { setParams } from "../router.js";
import { bytes, count, percent, ago, digest, ecoColor, outcomeColor } from "../format.js";

export default {
  mount(node, params) {
    // page lives alongside the other filters so a position in a large inventory is
    // shareable and survives a reload, the same way eco/q/sort already do.
    let filters = {
      eco: params.eco || "", q: params.q || "", sort: params.sort || "date",
      page: Math.max(1, Number(params.page) || 1),
    };

    const composition = region("div");
    const ages = region("div");
    const inventory = region("div");
    const leaders = region("div");

    const ecoSelect = select(
      "eco",
      [{ value: "", label: "All ecosystems" },
        ...store.state.ecosystems.map((d) => ({ value: d.id, label: d.display }))],
      filters.eco,
    );
    const search = input("q", { value: filters.q, placeholder: "Search artifact names" });
    const sortSelect = select("sort", [
      { value: "date", label: "Newest first" },
      { value: "size", label: "Largest first" },
      { value: "name", label: "By name" },
    ], filters.sort);

    // These three go through field() like every other form control in the console. As
    // bare controls they had no accessible name at all — the two selects announced only
    // "combo box", and a placeholder is not a name (it also disappears once you type).
    const form = el(
      "form",
      { class: "toolbar" },
      field("Ecosystem", ecoSelect),
      field("Search", search),
      field("Sort", sortSelect),
      el("button", { class: "btn", type: "submit", text: "Apply" }),
    );
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      // A changed query invalidates the position in the old result set.
      filters = { eco: ecoSelect.value, q: search.value, sort: sortSelect.value, page: 1 };
      // Filters live in the URL so a view of the cache can be shared or bookmarked.
      setParams(filters);
      void loadInventory();
    });

    fill(
      node,
      el("div", { class: "view-head" },
        el("h1", { text: "Cache" }),
        el("p", { class: "note", text: `project ${store.state.project}` })),
      el("div", { class: "panel-grid" },
        panel("By ecosystem", { note: "bytes held and where requests were answered", wide: true }, composition.node),
        panel("Cold content", { note: "time since last read — what eviction takes first" }, ages.node),
        panel("Most requested", { note: "all time" }, leaders.node)),
      panel("Inventory", { wide: true }, form, inventory.node),
    );

    const drawComposition = () => composition.set(renderComposition());
    const drawLeaders = () => leaders.set(renderLeaders());
    const unsubscribe = [store.on("stats", () => { drawComposition(); drawLeaders(); })];
    drawComposition();
    drawLeaders();
    // Fetched below, so they start as loading rather than as "Nothing cached yet."
    inventory.set(loading("Reading inventory"));
    ages.set(loading("Reading content ages"));

    let cancelled = false;

    async function loadInventory() {
      try {
        const result = await api.artifacts(store.state.project, filters);
        if (cancelled) return;
        inventory.set(renderInventory(result, filters, goToPage));
      } catch (cause) {
        if (!cancelled) store.fail(cause);
      }
    }

    async function loadAges() {
      try {
        const result = await api.ages(store.state.project);
        if (cancelled) return;
        ages.set(renderAges(result.buckets || []));
      } catch (cause) {
        if (!cancelled) store.fail(cause);
      }
    }

    function goToPage(page) {
      // Only the URL is written here. The page number always changes, so the hashchange
      // always fires and the router's params() hook does the reload — unlike the filter
      // form, which can be resubmitted unchanged and so has to fetch directly.
      setParams({ ...filters, page });
    }

    void loadInventory();
    void loadAges();

    return {
      teardown() {
        cancelled = true;
        unsubscribe.forEach((off) => off());
      },
      params(next) {
        filters = {
          eco: next.eco || "", q: next.q || "", sort: next.sort || "date",
          page: Math.max(1, Number(next.page) || 1),
        };
        ecoSelect.value = filters.eco;
        search.value = filters.q;
        sortSelect.value = filters.sort;
        void loadInventory();
      },
    };
  },
};

function renderComposition() {
  if (!store.hasLoaded("stats")) return loading("Reading cache composition");
  const rows = (store.state.stats?.by_eco || []).slice().sort((a, b) => b.size - a.size);
  if (!rows.length) return el("p", { class: "empty", text: "Nothing cached yet." });

  const sizeChart = charts.figure(
    "Bytes by ecosystem",
    {
      columns: [
        { label: "Ecosystem", cell: (row) => row.eco },
        { label: "Bytes", numeric: true, cell: (row) => bytes(row.size) },
        { label: "Artifacts", numeric: true, cell: (row) => count(row.count) },
      ],
      rows,
    },
    charts.barRows(
      rows.map((row) => ({ label: row.eco, value: row.size, color: ecoColor(row.eco) })),
      { format: bytes },
    ),
  );

  const served = table(
    [
      { label: "Ecosystem", cell: (row) => row.eco },
      {
        label: "Where it came from",
        cell: (row) =>
          charts.segmentedBar(
            [
              { label: "local", value: row.hit_count, color: outcomeColor("hit") },
              { label: "upstream", value: row.miss_count, color: outcomeColor("miss") },
            ],
            { format: count },
          ),
      },
      {
        label: "Hit rate",
        numeric: true,
        cell: (row) => percent(row.hit_count, row.hit_count + row.miss_count),
      },
      { label: "Served", numeric: true, cell: (row) => bytes(row.hit_bytes + row.miss_bytes) },
    ],
    rows,
  );

  return el("div", { class: "stack" }, sizeChart,
    el("h3", { class: "sub-heading", text: "Requests, lifetime" }), served);
}

function renderAges(buckets) {
  const total = buckets.reduce((sum, bucket) => sum + bucket.bytes, 0);
  if (!total) return el("p", { class: "empty", text: "Nothing cached yet." });

  return charts.figure(
    "Bytes by time since last read",
    {
      note: "the last bars are the eviction queue",
      columns: [
        { label: "Age", cell: (row) => row.label },
        { label: "Entries", numeric: true, cell: (row) => count(row.entries) },
        { label: "Bytes", numeric: true, cell: (row) => bytes(row.bytes) },
        { label: "Share", numeric: true, cell: (row) => percent(row.bytes, total) },
      ],
      rows: buckets,
    },
    charts.barRows(
      buckets.map((bucket) => ({
        label: bucket.label,
        value: bucket.bytes,
        // One measure, one hue: this is a distribution, not six identities. Colder
        // buckets are the ones eviction reaches first, so they carry the warning.
        color: bucket.max_age_days === 0 || bucket.max_age_days > 90
          ? "var(--status-warning)"
          : "var(--series-1)",
      })),
      { format: bytes },
    ),
  );
}

function renderLeaders() {
  if (!store.hasLoaded("stats")) return loading("Reading request counts");
  const rows = store.state.stats?.leaderboard || [];
  if (!rows.length) return el("p", { class: "empty", text: "No requests recorded yet." });
  const peak = Math.max(...rows.map((row) => row.count), 1);
  return table(
    [
      { label: "Package", cell: (row) => row.name, title: (row) => row.name },
      { label: "Eco", cell: (row) => row.eco },
      {
        label: "Requests",
        numeric: true,
        // A bar inside the table rather than beside it: the ranking is the chart.
        cell: (row) =>
          el("span", { class: "cell-bar" },
            el("span", { class: "cell-bar-fill", style: `width:${(row.count / peak) * 100}%` }),
            el("span", { class: "cell-bar-text", text: count(row.count) })),
      },
      { label: "Last read", cell: (row) => ago(row.last_access) },
    ],
    rows.slice(0, 15),
  );
}

// The server pages at 100. Without a pager the console printed the real total and then
// showed page one forever, so on a cache holding tens of thousands of artifacts —
// which is the whole point of the product — anything outside the first page of any
// ordering was unreachable through the UI.
const PAGE_SIZE = 100;

function renderInventory(result, filters, goToPage) {
  const rows = result.artifacts || [];
  const total = Number(result.total) || 0;
  const page = Math.max(1, Number(result.page) || filters?.page || 1);
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE));
  const first = total ? (page - 1) * PAGE_SIZE + 1 : 0;
  const last = Math.min(total, (page - 1) * PAGE_SIZE + rows.length);

  return el(
    "div",
    {},
    el("p", { class: "note", text: total
      ? `${first}–${last} of ${count(total)} matching`
      : "0 matching" }),
    table(
      [
        { label: "Name", cell: (row) => row.name, title: (row) => row.name },
        { label: "Version", cell: (row) => row.version || "—" },
        { label: "Eco", cell: (row) => badge(row.eco) },
        { label: "Arch", cell: (row) => row.arch || "—" },
        { label: "Size", numeric: true, cell: (row) => bytes(row.size) },
        { label: "Digest", cell: (row) => el("code", { text: digest(row.digest), title: row.digest }) },
        { label: "Cached", cell: (row) => ago(row.cached_at) },
      ],
      rows,
      { empty: "No artifacts match this filter." },
    ),
    pages > 1
      ? el(
          "nav",
          { class: "pager", "aria-label": "Inventory pages" },
          button("Previous", () => goToPage(page - 1),
            { kind: "ghost small", disabled: page <= 1 }),
          el("span", { class: "note", text: `Page ${count(page)} of ${count(pages)}` }),
          button("Next", () => goToPage(page + 1),
            { kind: "ghost small", disabled: page >= pages }),
        )
      : null,
  );
}

function badge(eco) {
  return el("span", { class: "badge" },
    el("span", { class: "swatch", style: `background:${ecoColor(eco)}` }),
    el("span", { text: eco }));
}
