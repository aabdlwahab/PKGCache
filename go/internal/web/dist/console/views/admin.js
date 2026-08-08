/* Admin — "who may do what, what is this project's policy, and what happened?" */

import { el, region, panel, fill, table, button, field, input, select, loading } from "../dom.js";
import { api } from "../api.js";
import * as store from "../store.js";
import * as charts from "../charts.js";
import { bytes, ago, duration } from "../format.js";

/* The outcome of the last GC or eviction run, kept beside the buttons that started it.
 *
 * A dry run whose result is invisible is a dry run nobody takes: the careful path costs
 * a click and answers nothing, so the operator learns to skip it and press the
 * destructive button instead. The numbers already exist — the maintenance service writes
 * "scanned=… reclaimed_bytes=…" to a durable job log — they were simply never fetched.
 * This region survives a redraw because it is created once, at module scope. */
const maintenanceResult = region("div", { class: "maintenance-result", "aria-live": "polite" });

/* Log keys in the order an operator reads them: how much was looked at, what would go,
 * what actually went. A dry run and a real run report the same keys, so the wording
 * carries the tense rather than the caller having to. */
const RESULT_LABEL = {
  scanned: ["scanned", "scanned"],
  candidates: ["would be removed", "eligible"],
  evicted_entries: ["entries would be evicted", "entries evicted"],
  deleted_blobs: ["blobs would be deleted", "blobs deleted"],
  deleted: ["would be deleted", "deleted"],
  reclaimed_bytes: ["would reclaim", "reclaimed"],
  pinned: ["pinned by a checkpoint", "pinned by a checkpoint"],
};

/** Parse one "key=value key=value" job log line into labelled pairs, in RESULT_LABEL order. */
function readResultLine(line, dry) {
  const seen = new Map();
  for (const token of String(line).trim().split(/\s+/)) {
    const split = token.indexOf("=");
    if (split < 1) continue;
    const key = token.slice(0, split);
    const value = Number(token.slice(split + 1));
    if (!RESULT_LABEL[key] || !Number.isFinite(value)) continue;
    seen.set(key, value);
  }
  return [...Object.keys(RESULT_LABEL)]
    .filter((key) => seen.has(key))
    .map((key) => [
      RESULT_LABEL[key][dry ? 0 : 1],
      key === "reclaimed_bytes" ? bytes(seen.get(key)) : String(seen.get(key)),
    ]);
}

async function runMaintenance(operation, started) {
  maintenanceResult.set(el("p", { class: "note", text: `${started}…` }));
  const record = await store.mutate(operation, started);
  if (!record?.id) return maintenanceResult.set();

  // Poll rather than wait on an event: job.update arrives over SSE, but a maintenance
  // pass on a small store can finish before the subscription delivers anything.
  for (let attempt = 0; attempt < 40; attempt += 1) {
    let job;
    try {
      job = await api.job(record.id);
    } catch (cause) {
      return maintenanceResult.set(
        el("p", { class: "form-error", text: `Could not read the result: ${cause.message}` }),
      );
    }
    if (job.status !== "running" && job.status !== "queued") {
      return maintenanceResult.set(renderResult(job));
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  maintenanceResult.set(
    el("p", { class: "note", text: "Still running. It will appear in Job history when it finishes." }),
  );
}

function renderResult(job) {
  if (job.error) {
    return el("p", { class: "form-error", text: job.error });
  }
  const dry = job.params?.dry_run === true;
  const line = String(job.log || "").trim().split("\n").filter(Boolean).pop();
  const pairs = line ? readResultLine(line, dry) : [];
  if (!pairs.length) {
    return el("p", { class: "note", text: `${job.action} finished.` });
  }
  return el(
    "div",
    { class: "result-card" },
    el("p", { class: "result-head", text: dry ? "Dry run — nothing was deleted" : `${job.action} complete` }),
    el("dl", { class: "result-figures" },
      pairs.flatMap(([label, value]) => [
        el("dt", { text: label }),
        el("dd", { text: value }),
      ])),
  );
}

export default {
  mount(node) {
    const policy = region("div");
    const upkeep = region("div");
    const jobs = region("div");
    const people = region("div");
    const access = region("div");
    const audit = region("div");
    const lifecycle = region("div");

    fill(
      node,
      el("div", { class: "view-head" },
        el("h1", { text: "Admin" }),
        el("p", { class: "note", text: `Policy and governance for ${store.state.project}.` })),
      el("div", { class: "panel-grid" },
        panel("Project policy", { note: "quotas, auth and rate limits" }, policy.node),
        panel("Maintenance", { note: "reclaim space" }, upkeep.node),
        panel("Job history", { note: "last 100 runs", wide: true }, jobs.node),
        panel("Accounts", { note: "superuser only" }, people.node),
        panel("Project access", { note: "owner or superuser" }, access.node),
        panel("Project lifecycle", {}, lifecycle.node)),
      panel("Audit", { note: "newest first", wide: true }, audit.node),
    );

    const draw = () => {
      policy.set(renderPolicy());
      upkeep.set(renderMaintenance());
      lifecycle.set(renderLifecycle());
    };
    const drawJobs = () => jobs.set(renderJobs());
    const drawAccess = () => access.set(renderAccess(() => loadGrants(true)));
    const unsubscribe = [
      store.on(["projects", "project", "storage"], draw),
      store.on("jobs", drawJobs),
      store.on("users", () => people.set(renderUsers())),
      store.on(["grants", "projects", "project", "users"], drawAccess),
      store.on("audit", () => audit.set(renderAudit())),
    ];
    draw();
    drawJobs();
    people.set(renderUsers());
    audit.set(renderAudit());
    drawAccess();

    let cancelled = false;

    /* Grants are per project, so this reruns when the project switcher moves — and
       when the project list itself lands, because whether this actor may read the
       access list depends on who owns the project, which is not known until then.
       Re-entry is cheap to get wrong, so the last-requested project is tracked: the
       list arriving would otherwise fire a second identical fetch every mount.

       The project is captured in the stored value, not only in the request, so a slow
       answer for the project you just left cannot paint over the one you are on. */
    let requested = null;
    async function loadGrants(force) {
      const project = store.state.project;
      if (!store.canGrant() || (!force && requested === project)) return;
      requested = project;
      try {
        const answer = await api.grants(project);
        if (!cancelled && store.state.project === project) {
          store.set({ grants: { project, rows: answer.grants || [] } });
        }
      } catch (cause) {
        requested = null;
        if (!cancelled) access.set(el("p", { class: "empty", text: cause.message }));
      }
    }
    // Wrapped rather than passed directly: store.on hands the subscriber the whole
    // state object, which would arrive here as a truthy `force`.
    unsubscribe.push(store.on(["project", "projects"], () => loadGrants()));
    loadGrants();
    // Accounts and the audit log are only read here, so they are fetched on arrival
    // rather than kept warm for every other view — and fetched independently. Sharing
    // a Promise.all meant one refusal took the other panel down with it: in open mode
    // /users answers 401 (there are no accounts), which used to replace the whole view
    // with an error and lose the audit table.
    //
    // Fetched for any signed-in account, not only for a superuser: /users already
    // narrows itself to what the caller may see, and those names are what the access
    // panel offers as suggestions. A refusal only costs the suggestions. Deliberately
    // not gated on canGrant(), which cannot be answered until the project list lands.
    (async () => {
      if (!store.state.me?.auth_enabled || !store.state.me?.authenticated || store.isGuest()) return;
      try {
        const users = await api.users();
        if (!cancelled) store.set({ users: users.users || [] });
      } catch (cause) {
        if (!cancelled && accountsAvailable()) {
          people.set(el("p", { class: "empty", text: cause.message }));
        }
      }
    })();
    (async () => {
      try {
        const log = await api.audit();
        if (!cancelled) store.set({ audit: log.audit || log.entries || [] });
      } catch (cause) {
        if (!cancelled) audit.set(el("p", { class: "empty", text: cause.message }));
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

function renderPolicy() {
  if (!store.hasLoaded("projects")) return loading("Reading project policy");
  const project = store.state.projects.find((p) => p.name === store.state.project);
  if (!project) return el("p", { class: "empty", text: "Project not found." });
  if (!store.canOperate()) {
    return el(
      "dl",
      { class: "facts" },
      fact("Byte quota", project.quota_bytes ? bytes(project.quota_bytes) : "unlimited"),
      fact("Artifact quota", project.quota_artifacts || "unlimited"),
      fact("Data-plane auth", project.data_plane_auth || "public"),
      fact("Offline", project.offline ? "yes" : "no"),
    );
  }

  const form = el(
    "form",
    { class: "form" },
    field("Byte quota", input("quota_bytes", { type: "number", min: "0", value: project.quota_bytes || 0 }),
      "0 means unlimited"),
    field("Artifact quota", input("quota_artifacts", { type: "number", min: "0", value: project.quota_artifacts || 0 })),
    field("Rate limit", input("rate_limit", { type: "number", min: "0", value: project.rate_limit || 0 }),
      "requests per second, 0 to disable"),
    field("Burst", input("rate_burst", { type: "number", min: "0", value: project.rate_burst || 0 })),
    field("Data-plane auth", select("data_plane_auth", [
      { value: "public", label: "public — anyone who can reach it" },
      { value: "token", label: "token — a valid token required" },
    ], project.data_plane_auth || "public")),
    el("button", { class: "btn primary", type: "submit", text: "Save policy" }),
  );
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const data = new FormData(form);
    await store.mutate(
      () => api.patchProject(store.state.project, {
        quota_bytes: Number(data.get("quota_bytes")),
        quota_artifacts: Number(data.get("quota_artifacts")),
        rate_limit: Number(data.get("rate_limit")),
        rate_burst: Number(data.get("rate_burst")),
        data_plane_auth: String(data.get("data_plane_auth")),
      }),
      "Policy updated",
    );
  });

  const storage = store.state.storage;
  return el(
    "div",
    { class: "stack" },
    project.quota_bytes && storage
      ? charts.meter(storage.entry_bytes, project.quota_bytes, { format: bytes })
      : null,
    form,
  );
}

function fact(label, value) {
  return el("div", { class: "fact" }, el("dt", { text: label }), el("dd", { text: String(value) }));
}

function renderMaintenance() {
  const canOperate = store.canOperate();
  const superuser = store.isSuperuser();
  return el(
    "div",
    { class: "stack" },
    el("h3", { class: "sub-heading", text: "Garbage collection" }),
    el("p", { class: "note", text: "Removes blobs nothing references. Instance-wide, and safe to run while traffic is flowing." }),
    el("div", { class: "link-row" },
      button("Dry run", () => runMaintenance(() => api.gc(true), "GC dry run started"),
        { kind: "ghost", disabled: !superuser }),
      button("Collect", async () => {
        if (!confirm(
          "Collect garbage across the whole instance?\n\n" +
          "Blobs that nothing references are deleted permanently. This cannot be undone.\n" +
          "Run a dry run first to see what would go.",
        )) return;
        await runMaintenance(() => api.gc(false), "GC started");
      }, { kind: "danger", disabled: !superuser })),

    el("h3", { class: "sub-heading", text: "Eviction" }),
    el("p", { class: "note", text: "Drops the least recently used entries in this project until the policy holds. See the Cache view for what would go first." }),
    el("div", { class: "link-row" },
      button("Dry run", () => runMaintenance(() => api.evict(store.state.project, true), "Eviction dry run started"),
        { kind: "ghost", disabled: !canOperate }),
      button("Evict", async () => {
        if (!confirm(
          `Evict from ${store.state.project}?\n\n` +
          "The least recently used entries are deleted until the policy holds. " +
          "This cannot be undone.\nRun a dry run first to see what would go.",
        )) return;
        await runMaintenance(() => api.evict(store.state.project, false), "Eviction started");
      }, { kind: "danger", disabled: !canOperate })),
    maintenanceResult.node,
  );
}

function renderJobs() {
  if (!store.hasLoaded("jobs")) return loading("Reading job history");
  const rows = store.state.jobs || [];
  if (!rows.length) return el("p", { class: "empty", text: "No jobs have run." });

  const finished = rows.filter((job) => job.started_at && job.finished_at);
  const timings = finished.slice(0, 40).reverse().map((job) => ({
    label: job.action,
    value: Math.max(1, Date.parse(job.finished_at) - Date.parse(job.started_at)),
    // Status is a reserved palette and never a series colour; the pill beside each
    // row carries the word, so the bar is not the only signal.
    color: job.status === "failed" ? "var(--status-critical)" : "var(--series-1)",
  }));

  return el(
    "div",
    { class: "stack" },
    timings.length
      ? charts.figure("How long jobs took", {
          note: "most recent 40 runs",
          columns: [
            { label: "Action", cell: (row) => row.label },
            { label: "Duration", numeric: true, cell: (row) => duration(row.value) },
          ],
          rows: timings,
        }, charts.barRows(timings.slice(-12), { format: duration, height: 20 }))
      : null,
    table(
      [
        { label: "ID", numeric: true, cell: (row) => row.id },
        { label: "Action", cell: (row) => row.action },
        { label: "Project", cell: (row) => row.project || "instance" },
        { label: "Status", cell: (row) => el("span", { class: `pill ${row.status}`, text: row.status }) },
        { label: "Actor", cell: (row) => row.actor || "—" },
        { label: "Started", cell: (row) => ago(row.started_at) },
        {
          label: "Took",
          numeric: true,
          cell: (row) =>
            row.started_at && row.finished_at
              ? duration(Date.parse(row.finished_at) - Date.parse(row.started_at))
              : "—",
        },
        { label: "Error", cell: (row) => row.error || "—", title: (row) => row.error },
      ],
      rows.slice(0, 25),
    ),
  );
}

/* Who else may reach this project.
 *
 * Ownership answers "who is responsible for this tenant" and stays a single name. This
 * answers "who else works on it", which is a list — and until it existed the only ways
 * to add a second admin to a project were to hand ownership over, losing the first, or
 * to make them a superuser, which hands them every other tenant, the audit log and
 * account management at the same time. */
const GRANT_LEVELS = [
  { value: "operate", label: "operate — read and change" },
  { value: "view", label: "view — read only" },
];

function renderAccess(reload) {
  const me = store.state.me;
  if (!me?.auth_enabled) {
    return el("p", { class: "note", text: "Authentication is off, so every caller already reaches every project." });
  }
  if (!store.canGrant()) {
    return el("p", {
      class: "note",
      text: "Only a superuser or this project's owner can change who reaches it.",
    });
  }
  const held = store.state.grants;
  if (!held || held.project !== store.state.project) return loading("Reading project access");

  const project = store.state.projects.find((p) => p.name === store.state.project);
  const owner = project?.owner;
  /* A typed name with suggestions, not a picker.
   *
   * The obvious control here is a <select> of accounts, and it cannot work: /users
   * shows an admin only themselves and their own reports, so the very case this panel
   * exists for — an admin sharing their project with a *different* admin — would find
   * an empty list. The suggestions are whatever this actor can see, which is the full
   * roster for a superuser and a useful shortlist otherwise, and anything else can be
   * typed. The server decides, and says plainly when the account does not exist. */
  const granted = new Set(held.rows.map((row) => row.username));
  const suggestions = (store.state.users || []).filter(
    (row) => row.role !== "superuser" && row.username !== owner && !granted.has(row.username),
  );
  const listID = "grant-candidates";

  const form = el(
    "form",
    { class: "form" },
    field("Account", input("username", { required: true, list: listID, autocomplete: "off" }),
      "an existing account; superusers already reach every project"),
    el("datalist", { id: listID },
      suggestions.map((row) => el("option", { value: row.username, label: row.role }))),
    field("Level", select("level", GRANT_LEVELS, "operate")),
    el("button", { class: "btn primary", type: "submit", text: "Grant access" }),
  );
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const data = new FormData(form);
    const username = String(data.get("username")).trim();
    if (!username) return;
    const done = await store.mutate(
      () => api.setGrant(store.state.project, username, String(data.get("level"))),
      `${username} can now reach ${store.state.project}`,
    );
    if (done) form.reset();
    await reload();
  });

  return el(
    "div",
    { class: "stack" },
    el("p", { class: "note", text: owner ? `Owned by ${owner}.` : "This project has no owner, so only a superuser can share it." }),
    table(
      [
        { label: "Account", cell: (row) => row.username },
        {
          label: "Level",
          cell: (row) => {
            const picker = select("level", GRANT_LEVELS, row.level);
            picker.addEventListener("change", async () => {
              await store.mutate(
                () => api.setGrant(store.state.project, row.username, picker.value),
                `${row.username} can now ${picker.value} ${store.state.project}`);
              await reload();
            });
            return picker;
          },
        },
        { label: "Granted by", cell: (row) => row.granted_by || "—" },
        { label: "Since", cell: (row) => ago(row.created_at) },
        {
          label: "",
          cell: (row) =>
            button("Revoke", async () => {
              if (!confirm(`Revoke ${row.username}'s access to ${store.state.project}?`)) return;
              await store.mutate(
                () => api.deleteGrant(store.state.project, row.username),
                `Revoked ${row.username}`);
              await reload();
            }, { kind: "ghost small danger" }),
        },
      ],
      held.rows,
      { empty: "Only the owner and superusers reach this project." },
    ),
    form,
  );
}

/** Accounts exist only when authentication is enabled. With it off there is nothing to
 *  manage and the endpoint refuses, so the view should say so rather than ask. */
function accountsAvailable() {
  return Boolean(store.state.me?.auth_enabled) && store.isSuperuser();
}

function renderUsers() {
  if (!store.state.me?.auth_enabled) {
    return el("p", { class: "note", text: "Authentication is off, so there are no accounts. Set PKGREG_ROOT_USER and PKGREG_ROOT_PASSWORD to enable it." });
  }
  if (!store.isSuperuser()) {
    return el("p", { class: "note", text: "Only a superuser can manage accounts." });
  }
  const rows = store.state.users || [];
  const roles = [
    { value: "user", label: "user" },
    { value: "admin", label: "admin" },
    { value: "superuser", label: "superuser" },
  ];
  /* Only an admin or superuser can be a manager, and nobody can manage themselves.
     There is no "none" option: a user must report to someone, the server refuses an
     empty manager, and offering a choice it will reject is how a form teaches people
     that its controls are unreliable. */
  const managers = (subject) =>
    rows
      .filter((row) => row.username !== subject && (row.role === "admin" || row.role === "superuser"))
      .map((row) => ({ value: row.username, label: `${row.username} (${row.role})` }));
  const refresh = async () => {
    if (accountsAvailable()) store.set({ users: (await api.users()).users || [] });
  };

  const form = el(
    "form",
    { class: "form" },
    field("Username", input("username", { required: true })),
    field("Password", input("password", { type: "password", required: true, minlength: "10" })),
    field("Role", select("role", roles, "user")),
    field("Reports to", select("reports_to", managers("")),
      "the admin this user answers to; they see that admin's projects"),
    el("button", { class: "btn primary", type: "submit", text: "Create account" }),
  );
  /* reports_to belongs to the user role alone: an admin or superuser has no manager,
     and the server rejects one. Hiding the field rather than ignoring it means the form
     never shows a value that will be silently dropped. */
  const reportsField = form.querySelector('select[name="reports_to"]').closest(".field");
  const syncReports = () => {
    reportsField.hidden = form.querySelector('select[name="role"]').value !== "user";
  };
  form.querySelector('select[name="role"]').addEventListener("change", syncReports);
  syncReports();
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const data = new FormData(form);
    const role = String(data.get("role"));
    await store.mutate(
      () => api.createUser({
        username: String(data.get("username")),
        password: String(data.get("password")),
        role,
        reports_to: role === "user" ? String(data.get("reports_to")) : "",
      }),
      "Account created",
    );
    await refresh();
    form.reset();
    syncReports();
  });

  return el(
    "div",
    { class: "stack" },
    table(
      [
        { label: "User", cell: (row) => row.username },
        {
          label: "Role",
          cell: (row) => {
            const picker = select("role", roles, row.role);
            picker.addEventListener("change", async () => {
              await store.mutate(
                () => api.patchUser(row.username, { role: picker.value }),
                `${row.username} is now ${picker.value}`);
              await refresh();
            });
            return picker;
          },
        },
        {
          label: "Reports to",
          cell: (row) => {
            // Only a user has a manager, and the built-in root account is not stored,
            // so neither offers a picker — the dash is the whole truth for both.
            if (row.role !== "user" || row.builtin) return "—";
            const picker = select("reports_to", managers(row.username), row.reports_to || "");
            picker.addEventListener("change", async () => {
              await store.mutate(
                () => api.patchUser(row.username, { reports_to: picker.value }),
                `${row.username} now reports to ${picker.value}`);
              await refresh();
            });
            return picker;
          },
        },
        // The configured root account is not a stored row: it has no creation time to
        // report, and deleting it is refused because it lives in the configuration.
        // Both used to render anyway, as "739833d ago" and a button that only 403s.
        { label: "Created", cell: (row) => (row.builtin ? "—" : ago(row.created_at)) },
        {
          label: "",
          cell: (row) =>
            row.builtin ? "—" : button("Delete", async () => {
              if (!confirm(`Delete the account ${row.username}?`)) return;
              await store.mutate(() => api.deleteUser(row.username), `Deleted ${row.username}`);
              await refresh();
            }, { kind: "ghost small danger" }),
        },
      ],
      rows,
      { empty: "No accounts. The root credentials from the environment are the only way in." },
    ),
    form,
  );
}

function renderAudit() {
  const rows = store.state.audit || [];
  return table(
    [
      { label: "When", cell: (row) => ago(row.time) },
      { label: "Actor", cell: (row) => row.actor || "—" },
      { label: "Action", cell: (row) => row.action },
      { label: "Target", cell: (row) => row.target || "—", title: (row) => row.target },
      { label: "From", cell: (row) => row.client_ip || "—" },
      // detail is a free-form object, so it needs rendering rather than coercing —
      // String() on it yields the useless "[object Object]".
      { label: "Detail", cell: (row) => detailText(row.detail), title: (row) => detailText(row.detail) },
    ],
    rows.slice(0, 50),
    { empty: "Nothing recorded yet." },
  );
}

function detailText(detail) {
  if (!detail) return "—";
  if (typeof detail === "string") return detail;
  const pairs = Object.entries(detail).filter(([, value]) => value !== "" && value !== null);
  return pairs.length ? pairs.map(([key, value]) => `${key}=${value}`).join(" ") : "—";
}

function renderLifecycle() {
  const canCreate = store.isSuperuser() || store.state.me?.role === "admin" || !store.state.me?.auth_enabled;
  const form = el(
    "form",
    { class: "form" },
    field("New project", input("name", { required: true, placeholder: "team-b" }),
      "becomes a URL prefix, so keep it short"),
    el("button", { class: "btn primary", type: "submit", text: "Create", disabled: !canCreate }),
  );
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const name = String(new FormData(form).get("name"));
    const created = await store.mutate(() => api.createProject(name), `Created ${name}`);
    if (created) {
      await store.loadInstance();
      store.setProject(name);
    }
    form.reset();
  });

  const current = store.state.project;
  return el(
    "div",
    { class: "stack" },
    form,
    store.isSuperuser() && current !== "global"
      ? button(`Delete ${current}`, async () => {
          if (!confirm(`Delete the project ${current}?\n\nIts cached entries stop being served. Blobs shared with other projects are untouched.`)) return;
          await store.mutate(() => api.deleteProject(current), `Deleted ${current}`);
          await store.loadInstance();
        }, { kind: "danger" })
      : el("p", { class: "note", text: current === "global" ? "The global project cannot be deleted." : "Only a superuser can delete a project." }),
  );
}
