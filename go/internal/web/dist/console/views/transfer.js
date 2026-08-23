/* Transfer — "how do I move this cache, or fill it before I need it?"
 *
 * Checkpoints, packs, rollback and lockfile warming. Everything that moves content
 * across an air gap or ahead of demand. */

import { el, region, panel, fill, table, button, field, input, loading } from "../dom.js";
import { api } from "../api.js";
import * as store from "../store.js";
import { bytes, count, ago, digest } from "../format.js";

export default {
  mount(node) {
    const history = region("div");
    const actions = region("div");
    const warm = region("div");

    fill(
      node,
      el("div", { class: "view-head" },
        el("h1", { text: "Transfer" }),
        el("p", { class: "note", text: `Moving ${store.state.project} in, out, and backwards.` })),
      el("div", { class: "panel-grid" },
        panel("Checkpoint and pack", { note: "a checkpoint is a manifest, a pack carries the bytes" }, actions.node),
        panel("Warm from a lockfile", { note: "fetch everything a uv.lock pins, before anyone asks for it" }, warm.node)),
      panel("History", { note: "newest first", wide: true }, history.node),
    );

    const draw = () => {
      history.set(renderHistory());
      actions.set(renderActions());
      warm.set(renderWarm());
    };
    const unsubscribe = [store.on(["snapshots", "projects", "project"], draw)];
    draw();

    return { teardown: () => unsubscribe.forEach((off) => off()) };
  },
};

function renderHistory() {
  if (!store.hasLoaded("snapshots")) return loading("Reading checkpoints");
  const rows = store.state.snapshots || [];
  const canOperate = store.canOperate();
  return table(
    [
      { label: "Checkpoint", cell: (row) => el("code", { text: digest(row.id), title: row.id }) },
      { label: "Subject", cell: (row) => row.subject || "—", title: (row) => row.subject },
      { label: "Entries", numeric: true, cell: (row) => count(row.entry_count) },
      { label: "Size", numeric: true, cell: (row) => bytes(row.total_bytes) },
      { label: "Author", cell: (row) => row.author || "—" },
      { label: "When", cell: (row) => ago(row.created_at) },
      {
        label: "",
        cell: (row) =>
          canOperate
            ? button("Roll back", async () => {
                // Rollback rewrites what the project serves. Ask before doing it.
                if (!confirm(`Roll ${store.state.project} back to ${digest(row.id)}?\n\nEntries added since this checkpoint stop being served.`)) return;
                await store.mutate(() => api.rollback(store.state.project, row.id),
                  `Rolling back to ${digest(row.id)}`);
              }, { kind: "ghost small danger" })
            : "—",
      },
    ],
    rows,
    { empty: "No checkpoints yet. Take one before you need it." },
  );
}

function renderActions() {
  if (!store.canOperate()) {
    return el("p", { class: "note", text: "Only the project owner or a superuser can transfer." });
  }

  const checkpointForm = el(
    "form",
    { class: "form" },
    field("Message", input("message", { required: true, placeholder: "before the 2026-Q3 freeze" })),
    el("button", { class: "btn primary", type: "submit", text: "Take checkpoint" }),
  );
  checkpointForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    const message = String(new FormData(checkpointForm).get("message"));
    await store.mutate(() => api.checkpoint(store.state.project, message), "Checkpoint started");
    checkpointForm.reset();
  });

  const exportForm = el(
    "form",
    { class: "form" },
    // The job takes a basename and writes it into shuttle/out — it refuses a path, and
    // this asked for one, so every export here has quietly been named by the server
    // instead. A browser cannot move a file off the machine anyway; naming it is the
    // most this form can honestly offer.
    field("File name", input("file", { placeholder: "leave empty to name it after the checkpoint" }),
      "written into shuttle/out on the server, not to this machine"),
    field("Since checkpoint", input("base", { placeholder: "leave empty for a full pack" }),
      "a delta pack carries only what changed"),
    el("button", { class: "btn", type: "submit", text: "Export pack" }),
  );
  exportForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    const data = new FormData(exportForm);
    await store.mutate(
      () => api.exportPack(store.state.project, {
        file: String(data.get("file") || ""),
        base: String(data.get("base") || ""),
      }),
      "Export started",
    );
  });

  const importForm = el(
    "form",
    { class: "form" },
    field("File name", input("file", { placeholder: "leave empty if there is only one" }),
      "a .tar already in shuttle/in on the server"),
    el("button", { class: "btn", type: "submit", text: "Import pack" }),
  );
  importForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    const file = String(new FormData(importForm).get("file") || "");
    await store.mutate(() => api.importPack(store.state.project, { file }), "Import started");
  });

  return el(
    "div",
    { class: "stack" },
    el("h3", { class: "sub-heading", text: "Checkpoint" }),
    el("p", { class: "note", text: "Records what the project holds right now. Cheap: it is a sorted manifest, not a copy." }),
    checkpointForm,
    el("h3", { class: "sub-heading", text: "Export" }),
    exportForm,
    el("h3", { class: "sub-heading", text: "Import" }),
    el("p", { class: "note", text: "Content is verified by digest as it lands, so a corrupted pack fails rather than poisoning the store." }),
    importForm,
  );
}

function renderWarm() {
  if (!store.canOperate()) {
    return el("p", { class: "note", text: "Only the project owner or a superuser can warm the cache." });
  }
  const project = store.state.projects.find((p) => p.name === store.state.project);
  if (project?.offline) {
    return el("p", { class: "empty", text: "The project is offline, so nothing can be fetched. Bring it online first." });
  }

  const lock = el("textarea", {
    name: "lock", rows: "8", required: true,
    placeholder: "Paste the contents of uv.lock",
  });
  const form = el(
    "form",
    { class: "form" },
    field("Cache host", input("host", { required: true, placeholder: "cache.internal" }),
      "the name clients will use, bare — no scheme or port"),
    field("uv.lock", lock),
    el("button", { class: "btn primary", type: "submit", text: "Warm" }),
  );
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const data = new FormData(form);
    await store.mutate(
      () => api.lockwarm(store.state.project, {
        lock: String(data.get("lock")),
        host: String(data.get("host")),
      }),
      "Warming started — watch it in the activity rail",
    );
  });

  return el(
    "div",
    { class: "stack" },
    el("p", { class: "note", text: "Every registry-sourced package the lock pins is fetched and cached. Run this before going offline, or before a build that must not miss." }),
    form,
  );
}
