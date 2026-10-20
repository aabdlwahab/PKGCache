/* Transfer — "how do I move this cache, or fill it before I need it?"
 *
 * Checkpoints, packs, rollback and lockfile warming. Everything that moves content
 * across an air gap or ahead of demand. */

import { el, region, panel, fill, table, button, field, input, select, loading } from "../dom.js";
import { api } from "../api.js";
import { askConfirm } from "../dialog.js";
import { canBrowse, pickDirectory, pickPack } from "../picker.js";
import * as store from "../store.js";
import { bytes, count, ago, digest } from "../format.js";

export default {
  mount(node) {
    const history = region("div");
    const actions = region("div");
    const warm = region("div");
    const bars = region("div");
    const exported = region("div");
    const warmed = region("div");

    /* A pack takes minutes, and a form that goes quiet for minutes is a form somebody
     * presses a second time. */
    const drawBars = () => {
      const running = [...store.state.packs.values()];
      bars.set(
        running.length
          ? running.map((pack) => {
              const share = pack.total > 0 ? pack.done / pack.total : 0;
              return el(
                "div",
                { class: "prog" },
                el("div", { class: "prog-label" },
                  el("span", { text: pack.action === "import" ? "Importing" : "Exporting" }),
                  el("span", { class: "note", text: `${pack.done} / ${pack.total} blobs` })),
                el("div", { class: "prog-track", role: "progressbar",
                  "aria-valuemin": "0", "aria-valuemax": String(pack.total),
                  "aria-valuenow": String(pack.done) },
                  el("div", { class: "prog-fill", style: `width: ${Math.round(share * 100)}%` })),
              );
            })
          : null,
      );
    };

    fill(
      node,
      el("div", { class: "view-head" },
        el("h1", { text: "Transfer" }),
        el("p", { class: "note", text: `Moving ${store.state.project} in, out, and backwards.` })),
      el("div", { class: "panel-grid" },
        panel("Checkpoint and pack", { note: "a checkpoint is a manifest, a pack carries the bytes" },
          bars.node, actions.node, exported.node),
        panel("Warm from a lockfile", { note: "fetch everything a uv.lock pins, before anyone asks for it" },
          warm.node, warmed.node)),
      panel("History", { note: "newest first; the current checkpoint is what Export packs", wide: true },
        history.node),
    );

    // Four subscriptions rather than one, because the forms hold what somebody is typing.
    // Every job update reloads the project, and a warm form redrawn on each one emptied
    // the uv.lock pasted into it while an earlier warm was still running.
    const unsubscribe = [
      store.on(["snapshots", "head", "projects", "project"], () => history.set(renderHistory())),
      store.on(["snapshots", "head", "projects", "project"], () => actions.set(renderActions())),
      store.on(["projects", "project", "coordinates"], () => warm.set(renderWarm())),
      store.on(["jobs", "project"], () => {
        outcome(exported, "export", describeExport);
        outcome(warmed, "lockwarm", describeWarm);
      }),
      store.on(["packs"], drawBars),
    ];
    drawBars();
    history.set(renderHistory());
    actions.set(renderActions());
    warm.set(renderWarm());
    outcome(exported, "export", describeExport);
    outcome(warmed, "lockwarm", describeWarm);

    return { teardown: () => unsubscribe.forEach((off) => off()) };
  },
};

function renderHistory() {
  if (!store.hasLoaded("snapshots")) return loading("Reading checkpoints");
  const rows = store.state.snapshots || [];
  const head = store.state.head;
  const canOperate = store.canOperate();
  return table(
    [
      {
        label: "Checkpoint",
        // Marked, because it cannot be read off the list: a rollback moves the project
        // back without removing what came after, so the newest row is not necessarily the
        // one an export packs, or the one an arriving pack must continue from.
        cell: (row) => el("span", { class: "inline" },
          el("code", { text: digest(row.id), title: row.id }),
          row.id === head ? el("span", { class: "pill good", text: "current" }) : null),
      },
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
                if (!await askConfirm({
                  title: "Roll back",
                  body: `Roll ${store.state.project} back to ${digest(row.id)}?\nEntries added since this checkpoint stop being served.`,
                  confirmLabel: "Roll back", danger: true,
                })) return;
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
  if (!store.hasLoaded("snapshots")) return loading("Reading checkpoints");
  const project = store.state.project;
  const head = store.state.head;
  // The same tree is served by a laptop's cache and by the team's server, and where a
  // pack lands is the one thing here that differs: on a cache, this machine; on a server,
  // a machine the reader is probably not sitting at.
  const local = store.isLocal();

  const checkpointForm = el(
    "form",
    { class: "form" },
    field("Message", input("message", { required: true, placeholder: "before the 2026-Q3 freeze" })),
    el("button", { class: "btn primary", type: "submit", text: "Take checkpoint" }),
  );
  checkpointForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    const message = String(new FormData(checkpointForm).get("message"));
    await store.mutate(() => api.checkpoint(project, message), "Checkpoint started");
    checkpointForm.reset();
  });

  // Declared before the forms that hold them: these are referenced while the form is
  // being built, and a const referenced above its declaration is a ReferenceError rather
  // than a warning.
  //
  // Beside the fields rather than in them, because choosing a place is a different act
  // from naming a file — a picker that submitted the form would export before anybody
  // pressed Export.
  let chosenDir = null;
  let chosenPack = null;
  const whereRegion = region("div", { class: "note" });
  const packRegion = region("div", { class: "note" });
  const whereButton = button("Choose a folder…", async () => {
    const picked = await pickDirectory();
    if (picked === null) return;
    chosenDir = picked;
    drawWhere();
  });
  const clearWhere = button("Use the cache's outbox", () => {
    chosenDir = null;
    drawWhere();
  });
  const packButton = button("Choose a pack…", async () => {
    const picked = await pickPack();
    if (picked === null) return;
    chosenPack = picked;
    drawPack();
  });
  const clearPack = button("Use the inbox", () => {
    chosenPack = null;
    drawPack();
  });
  function drawWhere() {
    whereRegion.set(
      chosenDir
        ? el("div", {}, el("span", { text: "Writing to " }), el("code", { text: chosenDir }))
        : el("span", { text: local
            ? "Written into shuttle/out in this cache's data directory."
            : "Written into shuttle/out in the server's data directory." }),
    );
    clearWhere.hidden = !chosenDir;
  }
  function drawPack() {
    packRegion.set(
      chosenPack
        ? el("div", {}, el("span", { text: "Reading " }), el("code", { text: chosenPack }))
        : el("span", { text: local
            ? "Read from shuttle/in in this cache's data directory."
            : "Read from shuttle/in in the server's data directory." }),
    );
    clearPack.hidden = !chosenPack;
  }
  drawWhere();
  drawPack();
  // Hidden where the daemon browses no machine of its own — a pkgreg — so the buttons
  // are absent rather than present and always failing.
  void canBrowse().then((able) => {
    for (const node of [whereButton, clearWhere, packButton, clearPack]) {
      node.hidden = node.hidden || !able;
    }
    if (!able) {
      whereButton.hidden = true;
      packButton.hidden = true;
    }
  });

  // A pack carries a checkpoint, not the project: what it holds is what the project held
  // when that checkpoint was taken. So the choice is offered where there is one, and made
  // where there is not — an export of a project nobody has checkpointed used to be
  // "Export started" followed by a job failing with "no checkpoint to export".
  // `pkgcache export` checkpoints every time; here it is asked for, because an export is
  // otherwise a read, and a checkpoint per click is a history made of exports.
  const fresh = el("input", { type: "checkbox", checked: !head, disabled: !head });
  const since = select("base", [
    { value: "", label: "nothing — a full pack" },
    ...(store.state.snapshots || []).map((row) => ({
      value: row.id,
      label: `${digest(row.id)}${row.id === head ? " (current)" : ""} ${row.subject || ""}`.trim(),
    })),
  ], "");

  const exportForm = el(
    "form",
    { class: "form" },
    // The job takes a basename; which directory is a separate question, answered by the
    // cache's outbox or, where the daemon browses its own machine, by the folder picker
    // below. Two fields, so "which pack" and "which place" cannot be confused.
    field("File name", input("file", { placeholder: "leave empty for the standard name" }),
      local
        ? "written into this cache's outbox, or the folder chosen below"
        : "written on the server, not to this machine"),
    el("p", { class: "note",
      text: `Unless named, a full pack is pkgreg-${project}-full-<time>-<checkpoint>.tar ` +
        `and a delta pkgreg-${project}-delta-<time>-<since>-<checkpoint>.tar.` }),
    field("Since checkpoint", since,
      "the checkpoint the receiving side is on; a delta carries only what changed after it"),
    el("label", { class: "field-check" }, fresh,
      el("span", { text: head
        ? `checkpoint first, to include what has been cached since ${digest(head)}`
        : "no checkpoint yet, so one is taken first" })),
    whereRegion.node,
    el("div", { class: "field-actions" }, whereButton, clearWhere),
    el("button", { class: "btn", type: "submit", text: "Export pack" }),
  );
  exportForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    const data = new FormData(exportForm);
    // Read at the click rather than from when the form was drawn: the head is what
    // decides whether there is anything to pack at all.
    const first = fresh.checked || !store.state.head;
    await store.mutate(
      () => api.exportPack(project, {
        file: String(data.get("file") || ""),
        base: String(data.get("base") || ""),
        ...(first ? { checkpoint: true } : {}),
        ...(chosenDir ? { dir: chosenDir } : {}),
      }),
      first
        ? "Export started: checkpointing first, so the pack holds what the project has now"
        : `Export started, from checkpoint ${digest(store.state.head)}`,
    );
  });

  const importForm = el(
    "form",
    { class: "form" },
    field("File name", input("file", { placeholder: "leave empty if there is only one" }),
      local ? "a .tar already in this cache's inbox" : "a .tar already in the server's inbox"),
    packRegion.node,
    el("div", { class: "field-actions" }, packButton, clearPack),
    el("button", { class: "btn", type: "submit", text: "Import pack" }),
  );
  importForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    // A chosen pack is an absolute path and wins over the name field, which addresses
    // the inbox. Both at once is a contradiction, and the one the person clicked is the
    // more recent statement of what they meant.
    const file = chosenPack || String(new FormData(importForm).get("file") || "");
    await store.mutate(() => api.importPack(project, { file }), "Import started");
  });

  return el(
    "div",
    { class: "stack" },
    el("h3", { class: "sub-heading", text: "Checkpoint" }),
    el("p", { class: "note", text: "Records what the project holds right now. Cheap: it is a sorted manifest, not a copy." }),
    checkpointForm,
    el("h3", { class: "sub-heading", text: "Export" }),
    el("p", { class: "note", text: head
      ? `A pack carries a checkpoint — ${digest(head)}, the current one, unless you checkpoint first.`
      : "A pack carries a checkpoint, and this project has none yet." }),
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
  const local = store.isLocal();
  const coordinates = store.state.coordinates;
  // The port and scheme the rewritten lock will carry, as this instance reports them.
  // Only the host is asked for: it is the one part that depends on who installs from it.
  const port = String(coordinates?.unified || "").split(":").pop();
  const scheme = coordinates?.scheme || (local ? "http" : "https");

  const lock = el("textarea", {
    name: "lock", rows: "8", required: true,
    placeholder: "Paste the contents of uv.lock",
  });
  const host = input("host", {
    required: true, placeholder: "cache.internal", value: coordinates?.host || "",
    autocomplete: "off", spellcheck: "false",
  });
  const preview = el("p", { class: "field-hint" });
  const drawPreview = () => {
    const name = host.value.trim() || "<host>";
    preview.textContent =
      `its URLs become ${scheme}://${name}${port ? `:${port}` : ""}/${store.state.project}/pypi/…`;
  };
  host.addEventListener("input", drawPreview);
  drawPreview();

  const form = el(
    "form",
    { class: "form" },
    field("uv.lock", lock),
    field("Cache host", host,
      "the name that machines installing from the rewritten lock reach this cache by — bare, no scheme or port"),
    preview,
    el("button", { class: "btn primary", type: "submit", text: "Warm" }),
  );
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const data = new FormData(form);
    await store.mutate(
      () => api.lockwarm(store.state.project, {
        lock: String(data.get("lock")),
        host: String(data.get("host")).trim(),
      }),
      "Warming started — the rewritten lock is offered below when it finishes",
    );
  });

  return el(
    "div",
    { class: "stack" },
    el("p", { class: "note", text:
      "Every registry-sourced file the lock pins is fetched and cached, then a copy of the " +
      "lock is rewritten to install from this cache and offered for download here. The lock " +
      "you paste is not changed. Run this before going offline, or before a build that must not miss." }),
    el("p", { class: "note", text:
      "uv.lock only, and every file it lists: a uv.lock names each package's wheels for every " +
      "platform and Python, not only the ones any one machine installs, so this can fetch far " +
      "more than a single install would. A lock is rewritten only once all of it is cached." }),
    local
      ? el("p", { class: "note" },
          "On this machine the address is usually its loopback, so a rewritten lock works here " +
            "and not in a Docker build or on another machine. For package-lock.json, yarn.lock or " +
            "pnpm-lock.yaml, or to fetch only what one platform installs, run ",
          el("code", { text: "pkgcache warmlock -eager -target auto" }),
          " in the project's directory instead — it leaves the lock alone unless given ",
          el("code", { text: "-rewrite" }), ".")
      : null,
    form,
  );
}

/* What the last export and the last warm came to, read from the job that ran them.
 *
 * The activity rail shows a job only while it runs, and both of these end with something
 * the reader needs next — where the pack went, or the rewritten lock — which until now
 * was in the job's log and nowhere on this page. */
const ended = new Map(); // job id → the full record, log included, once it has ended

function latestJob(action) {
  let newest = null;
  for (const job of store.state.jobs) {
    if (job.action !== action || job.project !== store.state.project) continue;
    if (!newest || job.id > newest.id) newest = job;
  }
  return newest;
}

function outcome(slot, action, describe) {
  const job = latestJob(action);
  if (!job) {
    slot.set();
    return;
  }
  if (job.status === "queued" || job.status === "running") {
    slot.set(el("p", { class: "status-line" },
      el("span", { class: `pill ${job.status}`, text: job.status }),
      el("span", { class: "note", text: job.started_at
        ? `job ${job.id}, started ${ago(job.started_at)}`
        : `job ${job.id}, waiting to start` })));
    return;
  }
  if (job.status !== "done") {
    slot.set(el("p", { class: "status-line" },
      el("span", { class: "pill failed", text: job.status }),
      el("span", { class: "note", text: job.error || `job ${job.id} did not finish` })));
    return;
  }
  // The list carries no log, so a finished job is read once, by id, and remembered.
  const known = ended.get(job.id);
  if (known) {
    slot.set(describe(known));
    return;
  }
  slot.set(loading("Reading the job"));
  api.job(job.id).then((record) => {
    ended.set(job.id, record);
    if (latestJob(action)?.id === job.id) slot.set(describe(record));
  }).catch(() => slot.set());
}

function describeExport(record) {
  const log = record.log || "";
  const wrote = /wrote (\S+)/.exec(log);
  const packed = /packed checkpoint (\S+)/.exec(log);
  return el("p", { class: "note" },
    `Last export, ${ago(record.finished_at)}: `,
    wrote ? el("code", { text: wrote[1] }) : "written",
    packed ? `, checkpoint ${digest(packed[1])}` : "",
    store.isLocal() ? "." : ", on the server.");
}

function describeWarm(record) {
  const log = record.log || "";
  const cached = (log.match(/^cached /gm) || []).length;
  const base = /rewritten lock points at (\S+)/.exec(log);
  if (!/rewritten lock available at /.test(log)) {
    // Finished without writing one: a lock that pins nothing from a registry.
    return el("p", { class: "note", text: `Last warm, ${ago(record.finished_at)}: nothing to fetch, so no lock was rewritten.` });
  }
  return el("div", { class: "stack" },
    el("p", { class: "note",
      text: `Last warm, ${ago(record.finished_at)}: ${count(cached)} file(s) cached` +
        (base ? `; the rewritten lock installs from ${base[1]}.` : ".") }),
    el("div", { class: "field-actions" },
      el("a", {
        class: "btn ghost",
        href: `/api/v1/projects/${encodeURIComponent(record.project)}/lockwarm/uv.lock`,
        download: "uv.lock",
        text: "Download the rewritten uv.lock",
      })));
}
