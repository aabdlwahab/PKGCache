/* Admin — "who may do what, what is this project's policy, and what happened?"
 *
 * One view over two programs that answer those questions differently. pkgreg is a server:
 * it has accounts, owners and grants, and an eviction policy an operator configured.
 * pkgcache is one person's cache: it has no accounts by design and its eviction policy is
 * forced to zero (config/local.go), but it has a disk budget, a folder it can be moved
 * out of, and a console it can share behind a password. Each panel is drawn for the
 * program actually serving the page. Drawn for both, half of them were controls that
 * could only fail on one of them, or instructions naming commands it does not have. */

import { el, region, panel, fill, table, button, field, input, select, loading } from "../dom.js";
import { api } from "../api.js";
import { askChoice, askConfirm, openModal } from "../dialog.js";
import { pickDirectory } from "../picker.js";
import * as store from "../store.js";
import * as charts from "../charts.js";
import { bytes, ago, count, duration } from "../format.js";

/* The shortest password either program accepts: an account's (auth/accounts.go) and the
 * shared console's (local/share.go) are both eight. Checked here only so a short one is
 * refused beside what was typed rather than by a round trip; the server still decides. */
const MIN_PASSWORD = 8;

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
    await pause(500);
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
    const local = store.isLocal();
    const quota = region("div");
    const policy = region("div");
    const upkeep = region("div");
    const jobs = region("div");
    const people = region("div");
    const access = region("div");
    const audit = region("div");
    const lifecycle = region("div");
    // Read once: the audit log is superuser-only on the server (v1.go listAudit), and
    // every other reader was shown a panel holding nothing but the 403 it earned.
    const showAudit = store.isSuperuser();
    const policyBody = el("div", { class: "stack" }, quota.node, policy.node);

    const grid = local
      ? [
          panel("Project policy", { note: "quotas and rate limits" }, policyBody),
          panel("Disk", { note: "the limit, and reclaiming space" }, upkeep.node),
          panel("Where the cache is", { note: "this machine" }, moveRegion.node),
          panel("Share this console", { note: "other machines, with a password" }, shareRegion.node),
          panel("Job history", { note: "last 100 runs", wide: true }, jobs.node),
          panel("Project lifecycle", {}, lifecycle.node),
        ]
      : [
          panel("Project policy", { note: "quotas, auth and rate limits" }, policyBody),
          panel("Maintenance", { note: "reclaim space" }, upkeep.node),
          panel("Job history", { note: "last 100 runs", wide: true }, jobs.node),
          panel("Accounts", { note: accountsNote(), wide: true }, people.node),
          panel("Project access", { note: "owner or superuser" }, access.node),
          panel("Project lifecycle", {}, lifecycle.node),
        ];
    fill(
      node,
      el("div", { class: "view-head" },
        el("h1", { text: "Admin" }),
        el("p", { class: "note", text: `Policy and governance for ${store.state.project}.` })),
      el("div", { class: "panel-grid" }, grid),
      showAudit ? panel("Audit", { note: "newest first", wide: true }, audit.node) : null,
    );

    let cancelled = false;

    /* What the byte quota is measured against: the sum of this project's entry sizes,
     * which is what CommitEntry adds up before it will store anything more.
     * storage.entry_bytes is that sum over every project, and the by_eco sizes count
     * artifacts and not the metadata beside them, so neither is the figure. /stats/ages
     * buckets exactly this project's entries, so its bytes add up to the right one. */
    let usage = null;
    let measuring = "";
    async function loadUsage() {
      const project = currentProject();
      if (!project?.quota_bytes || measuring === project.name) return;
      measuring = project.name;
      try {
        const answer = await api.ages(project.name);
        const total = (answer.buckets || []).reduce((sum, bucket) => sum + (bucket.bytes || 0), 0);
        if (!cancelled && store.state.project === project.name) {
          usage = { project: project.name, bytes: total };
          drawQuota();
        }
      } catch {
        // The meter stays as it was; the quota itself is still enforced and still shown.
      } finally {
        measuring = "";
      }
    }

    // The form and the meters are separate regions because they change at different
    // rates. The meters move with every completed download; the form must not, or a
    // quota half typed while a build runs is wiped by the next stats refresh.
    const drawPolicy = () => policy.set(renderPolicy());
    const drawQuota = () => {
      const meters = renderQuota(usage);
      quota.set(meters);
      // Hidden when empty, or the stack around it spends a gap on nothing.
      quota.node.hidden = !meters;
    };
    const drawUpkeep = () => upkeep.set(local ? renderDisk() : renderMaintenance());
    const drawJobs = () => jobs.set(renderJobs());
    const drawAccess = () => access.set(renderAccess(() => loadGrants(true)));
    const unsubscribe = [
      store.on(["projects", "project"], () => {
        drawPolicy();
        lifecycle.set(renderLifecycle());
      }),
      store.on(["projects", "project", "stats"], () => {
        drawQuota();
        void loadUsage();
      }),
      store.on(["projects", "project", "storage"], drawUpkeep),
      store.on("jobs", drawJobs),
    ];
    drawPolicy();
    drawQuota();
    drawUpkeep();
    drawJobs();
    lifecycle.set(renderLifecycle());
    void loadUsage();

    if (local) {
      // Both are facts about this machine rather than the project on screen, and both
      // are held at module scope, so a move that outlives this view is still reported.
      void loadSharing();
      void loadMigration();
    } else {
      unsubscribe.push(
        store.on("users", () => people.set(renderUsers())),
        store.on(["grants", "projects", "project", "users"], drawAccess),
      );
      people.set(renderUsers());
      drawAccess();
    }
    if (showAudit) {
      unsubscribe.push(store.on("audit", () => audit.set(renderAudit())));
      audit.set(renderAudit());
    }

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
      if (local || !store.canGrant() || (!force && requested === project)) return;
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
    // a Promise.all meant one refusal took the other panel down with it: with no
    // accounts /users answers 404 auth_disabled, which used to replace the whole view
    // with an error and lose the audit table.
    //
    // Fetched for any signed-in account, not only for a superuser: /users narrows itself
    // to what the caller may see — every account for a superuser, an admin and their
    // reports for an admin — and those names are also what the access panel suggests.
    // Deliberately not gated on canGrant(), which cannot be answered until the project
    // list lands.
    (async () => {
      const me = store.state.me;
      if (local || !me?.auth_enabled || !me?.authenticated || store.isGuest()) return;
      try {
        const users = await api.users();
        if (!cancelled) store.set({ users: users.users || [] });
      } catch (cause) {
        if (!cancelled) people.set(el("p", { class: "empty", text: cause.message }));
      }
    })();
    if (showAudit) {
      (async () => {
        try {
          const log = await api.audit();
          if (!cancelled) store.set({ audit: log.audit || log.entries || [] });
        } catch (cause) {
          if (!cancelled) audit.set(el("p", { class: "empty", text: cause.message }));
        }
      })();
    }

    return {
      teardown() {
        cancelled = true;
        unsubscribe.forEach((off) => off());
      },
    };
  },
};

function currentProject() {
  return store.state.projects.find((p) => p.name === store.state.project) ?? null;
}

function pause(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/* ---- project policy -------------------------------------------------------- */

/* Data-plane auth on pkgcache.
 *
 * Token mode makes every package request show a token, and nothing on this machine sends
 * one to its own cache: npm, pip, uv, docker and apt are all pointed at a loopback
 * address with no credential, so every one of them gets 401 and every build stops. On a
 * server the clients are given a token by the setup steps; here there is nobody to give
 * one to. So pkgcache is not offered the choice — except on a project already in token
 * mode, which is shown as the cause it is, with the way back. */
function offersDataPlaneAuth(project) {
  return !store.isLocal() || project?.data_plane_auth === "token";
}

function renderPolicy() {
  if (!store.hasLoaded("projects")) return loading("Reading project policy");
  const project = currentProject();
  if (!project) return el("p", { class: "empty", text: "Project not found." });
  const offerAuth = offersDataPlaneAuth(project);
  const lockedOut = store.isLocal() && project.data_plane_auth === "token"
    ? el("div", { class: "token-needed", role: "status" },
        el("strong", { text: "Every tool on this machine is refused here." }),
        el("p", {
          text: "This project asks each package request for a token, and nothing on this " +
            "machine sends one to its own cache — npm, pip, uv, docker and apt all get 401. " +
            "Set data-plane auth to public and save to let them back in.",
        }))
    : null;
  if (!store.canOperate()) {
    return el(
      "div",
      { class: "stack" },
      lockedOut,
      el("dl", { class: "facts" },
        fact("Byte quota", project.quota_bytes ? bytes(project.quota_bytes) : "unlimited"),
        fact("Artifact quota", project.quota_artifacts || "unlimited"),
        offerAuth ? fact("Data-plane auth", project.data_plane_auth || "public") : null,
        fact("Offline", project.offline ? "yes" : "no")),
    );
  }

  const form = el(
    "form",
    { class: "form" },
    field("Byte quota", input("quota_bytes", { type: "number", min: "0", value: project.quota_bytes || 0 }),
      "0 means unlimited. Counts this project's entries; bytes shared with another project count in both"),
    field("Artifact quota", input("quota_artifacts", { type: "number", min: "0", value: project.quota_artifacts || 0 }),
      "0 means unlimited"),
    field("Rate limit", input("rate_limit", { type: "number", min: "0", value: project.rate_limit || 0 }),
      "requests per second, 0 to disable"),
    field("Burst", input("rate_burst", { type: "number", min: "0", value: project.rate_burst || 0 })),
    offerAuth
      ? field("Data-plane auth", select("data_plane_auth", [
          { value: "public", label: "public — anyone who can reach it" },
          { value: "token", label: "token — a valid token required" },
        ], project.data_plane_auth || "public"))
      : null,
    el("button", { class: "btn primary", type: "submit", text: "Save policy" }),
  );
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const data = new FormData(form);
    const patch = {
      quota_bytes: Number(data.get("quota_bytes")),
      quota_artifacts: Number(data.get("quota_artifacts")),
      rate_limit: Number(data.get("rate_limit")),
      rate_burst: Number(data.get("rate_burst")),
    };
    // Sent only when it was offered: a field that is not on the form must not be
    // written, or saving a quota would quietly reset it.
    if (offerAuth) patch.data_plane_auth = String(data.get("data_plane_auth"));
    const done = await store.mutate(
      () => api.patchProject(store.state.project, patch),
      "Policy updated",
    );
    // The project list is where the policy is read back from, and mutate() reloads the
    // project's slice, not the list.
    if (done) await store.loadInstance();
  });

  return el("div", { class: "stack" }, lockedOut, form);
}

/** The project against its quotas, drawn only where one is set. */
function renderQuota(usage) {
  const project = currentProject();
  if (!project || (!project.quota_bytes && !project.quota_artifacts)) return null;
  const parts = [];
  if (project.quota_bytes) {
    parts.push(
      el("p", { class: "note", text: "This project's cached entries, against its byte quota" }),
      usage?.project === project.name
        ? charts.meter(usage.bytes, project.quota_bytes, { format: bytes, remaining: "left under the quota" })
        : loading("Measuring this project"),
    );
  }
  // by_eco counts the artifacts table for this project, which is what the artifact quota
  // counts too.
  if (project.quota_artifacts && store.hasLoaded("stats")) {
    const held = (store.state.stats?.by_eco || []).reduce((sum, row) => sum + (row.count || 0), 0);
    parts.push(
      el("p", { class: "note", text: "Its packages, against the artifact quota" }),
      charts.meter(held, project.quota_artifacts, { format: count, remaining: "left under the quota" }),
    );
  }
  return el("div", { class: "stack" }, parts);
}

function fact(label, value) {
  return el("div", { class: "fact" }, el("dt", { text: label }), el("dd", {}, value));
}

/** A command to copy. connect.js has the same block for setup steps. */
function commandBlock(command) {
  return el(
    "div",
    { class: "command" },
    el("pre", {}, el("code", { text: command })),
    button("Copy", async () => {
      try {
        await navigator.clipboard.writeText(command);
        store.notify("Copied");
      } catch {
        // Clipboard access can be refused; the text is on screen either way.
        store.fail(new Error("Clipboard was refused — select and copy manually."));
      }
    }, { kind: "ghost small" }),
  );
}

/* ---- maintenance: pkgreg --------------------------------------------------- */

function renderMaintenance() {
  const canOperate = store.canOperate();
  const superuser = store.isSuperuser();
  const storage = store.state.storage;
  return el(
    "div",
    { class: "stack" },
    el("h3", { class: "sub-heading", text: "Garbage collection" }),
    el("p", {
      class: "note",
      text: "Deletes stored blobs that no project's entries and no checkpoint refer to. " +
        "Blobs written within maintenance.gc_grace (an hour by default) are left alone, so " +
        "a download still being written is never taken. Instance-wide, safe while traffic " +
        "flows, and superuser only.",
    }),
    el("div", { class: "link-row" },
      button("Dry run", () => runMaintenance(() => api.gc(true), "GC dry run started"),
        { kind: "ghost", disabled: !superuser }),
      button("Collect", async () => {
        if (!await askConfirm({
          title: "Collect garbage",
          body: "Collect garbage across the whole instance?\n" +
            "Blobs that nothing references are deleted permanently. This cannot be undone.\n" +
            "Run a dry run first to see what would go.",
          confirmLabel: "Collect", danger: true,
        })) return;
        await runMaintenance(() => api.gc(false), "GC started");
      }, { kind: "danger", disabled: !superuser })),

    el("h3", { class: "sub-heading", text: "Eviction" }),
    /* What maintenance/service.go Evict actually does. The policy is the instance's, the
       usage it is compared against is the whole store's, and only the candidates are this
       project's — so the quota beside it, which is the one number on this page about this
       project's size, plays no part, and with the policy unset nothing goes at all. */
    el("p", {
      class: "note",
      text: `Deletes ${store.state.project}'s entries, least recently read first, for as long ` +
        "as the whole store is over maintenance.evict_target_bytes or the disk has less free " +
        "than maintenance.evict_min_free_bytes. With maintenance.evict_ttl set, entries not " +
        "read within it go too. The instance's usage decides, not this project's: its byte " +
        "quota plays no part, and with all three unset nothing is removed. The Cache view's " +
        "cold-content chart shows what goes first.",
    }),
    el("dl", { class: "facts" },
      fact("Store on disk", storage
        ? `${bytes(storage.blob_bytes)}, with ${bytes(storage.fs_free)} free`
        : "—"),
      fact("Free-space floor", storage?.min_free_bytes
        ? `${bytes(storage.min_free_bytes)} (evict_min_free_bytes)`
        : "not set"),
      // Not reported by any endpoint, so this says where they live rather than guessing.
      fact("Target size and TTL", "maintenance: in pkgreg.yaml, or PKGREG_EVICT_TARGET_BYTES and PKGREG_EVICT_TTL")),
    el("div", { class: "link-row" },
      button("Dry run", () => runMaintenance(() => api.evict(store.state.project, true), "Eviction dry run started"),
        { kind: "ghost", disabled: !canOperate }),
      button("Evict", async () => {
        if (!await askConfirm({
          title: "Evict",
          body: `Evict from ${store.state.project}?\n` +
            "Its least recently read entries are deleted while the instance is over its " +
            "eviction policy. This cannot be undone.\nRun a dry run first to see what would go.",
          confirmLabel: "Evict", danger: true,
        })) return;
        await runMaintenance(() => api.evict(store.state.project, false), "Eviction started");
      }, { kind: "danger", disabled: !canOperate })),
    maintenanceResult.node,
  );
}

/* ---- disk: pkgcache ----------------------------------------------------------
 *
 * Eviction is what a server does to hold a size; pkgcache never evicts, and its policy
 * values are zero, so the Evict button here removed nothing however often it was
 * pressed. What pkgcache has instead is a limit somebody chose, a free-space floor under
 * it, a cache that stops storing (and keeps serving) when either is reached, and
 * reclaiming what nothing refers to any more — which is what `pkgcache prune` runs. */

function renderDisk() {
  if (!store.hasLoaded("storage")) return loading("Measuring disk");
  const storage = store.state.storage;
  const budget = storage?.budget ?? null;
  return el(
    "div",
    { class: "stack" },
    budget
      ? renderBudget(storage, budget)
      : el("p", { class: "note", text: "This cache reports no disk budget; it is older than this page." }),

    el("h3", { class: "sub-heading", text: "Changing the limit" }),
    // There is no endpoint that writes the budget: it is a file `pkgcache limit` keeps
    // beside the cache, read once when the daemon starts (local/daemon.go). So the
    // command is the control, and the restart is part of it.
    el("p", { class: "note", text: "From a terminal on this machine; nothing on this page changes it." }),
    commandBlock("pkgcache limit 25G"),
    el("p", {
      class: "note",
      text: "pkgcache limit none removes the cap and keeps the floor; -min-free 10G moves the " +
        "floor. The running cache keeps the limit it started with until pkgcache stop, and the " +
        "next command starts it with the new one. PKGCACHE_LIMIT, where set, overrides both.",
    }),

    el("h3", { class: "sub-heading", text: "Reclaiming space" }),
    el("p", {
      class: "note",
      text: "Deletes stored files that no project and no checkpoint refers to any more — what " +
        "removing packages, deleting a project or rolling back leaves behind. Files written " +
        "within the last hour are left alone, so a download in progress is never taken. " +
        "Packages still cached are never touched, so a cache full of packages in use frees " +
        "nothing here: remove some, or raise the limit. The same as pkgcache prune, without " +
        "stopping the cache.",
    }),
    el("div", { class: "link-row" },
      button("Dry run", () => runMaintenance(() => api.gc(true), "Looking for space to reclaim"),
        { kind: "ghost" }),
      button("Reclaim space", async () => {
        if (!await askConfirm({
          title: "Reclaim space",
          body: "Delete what nothing in this cache refers to any more?\n" +
            "It is gone for good: getting it back means downloading it again.\n" +
            "A dry run first shows how much would go.",
          confirmLabel: "Reclaim space", danger: true,
        })) return;
        await runMaintenance(() => api.gc(false), "Reclaiming space");
      }, { kind: "danger" })),
    maintenanceResult.node,
  );
}

/* The same reading the Overview gives (overview.js renderBudget), in the same words: two
 * pages describing one limit differently would leave the reader to decide which is right. */
function renderBudget(storage, budget) {
  const limit = budget.limit_bytes > 0 ? budget.limit_bytes : 0;
  const floor = budget.min_free_bytes;
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
    budget.full
      ? el("div", { class: "alert error", role: "status" },
          el("span", {},
            el("strong", { text: "Full: not storing new content. " }),
            sentence(budget.reason || "still serving what it holds, and keeping nothing new")))
      : null,
    gauge,
    el("dl", { class: "facts" },
      fact("Limit", limit ? bytes(limit) : "none; the floor still applies"),
      fact("Cache on disk", bytes(budget.used_bytes)),
      fact("Free-space floor", `${bytes(floor)} kept free, ${bytes(budget.free_bytes)} free now`)),
  );
}

/** The daemon's reason as a sentence. It is written to follow a colon on stderr, so it
 *  starts lower-case and has no full stop; shown as written, beside a heading, it read
 *  as a fragment. overview.js does the same to the same text. */
function sentence(text) {
  const trimmed = String(text).trim();
  const capital = trimmed.charAt(0).toUpperCase() + trimmed.slice(1);
  return /[.!?]$/.test(capital) ? capital : `${capital}.`;
}

/* ---- moving the cache: pkgcache ----------------------------------------------
 *
 * The one thing on this page that takes the cache away while it happens. The daemon
 * serving the page cannot move the directory it holds a lock on, so it hands the move to
 * a process of its own and is stopped by it; the page stays loaded, loses its connection,
 * and asks again until a daemon answers with how the move ended. The state is at module
 * scope for the same reason maintenanceResult is: leaving this view must not lose a move
 * in progress, and coming back must not start a second wait beside the first. */

const moveRegion = region("div", { class: "stack", "aria-live": "polite" });
let migration = null; // where the cache is and whether it may move; null until known
let migrationError = "";
let moving = null; // { to, began } while a move is being waited out
let moveOutcome = null; // { ok, text }: how the last move ended, until the next one
let moveClock = null; // the elapsed-time and hint nodes of the move in progress

async function loadMigration() {
  if (moving) return drawMove();
  try {
    migration = await api.migration();
    migrationError = "";
  } catch (cause) {
    migration = null;
    migrationError = cause.message;
  }
  drawMove();
  const last = migration?.last;
  if (!last) return;
  if (last.state === "moving") return waitForMove(last);
  // A move finished while nothing was watching, or this page was reloaded partway.
  // Said once, then forgotten.
  await reportMove(last);
}

function drawMove() {
  moveRegion.set(renderMove());
}

function renderMove() {
  if (moving) return renderMoving();
  if (!migration) {
    return migrationError
      ? el("p", { class: "empty", text: migrationError })
      : loading("Finding the cache");
  }
  return [
    moveOutcome
      ? el("p", { class: moveOutcome.ok ? "session-result" : "form-error", text: moveOutcome.text })
      : null,
    el("dl", { class: "facts" },
      fact("Folder", el("code", { text: migration.data_dir })),
      migration.moved_from ? fact("Moved here from", el("code", { text: migration.moved_from })) : null),
    migration.movable
      ? el("p", {
          class: "note",
          text: "Moving it to a bigger disk keeps everything it holds. The cache stops while " +
            "it moves, and the old copy is removed only once the new one has been counted and matches.",
        })
      // Why not, in the daemon's own sentence — which names what to do instead.
      : el("p", { class: "note", text: migration.reason || "This cache cannot be moved from here." }),
    migration.movable
      ? button("Move to another disk…", () => moveCache(), { kind: "primary" })
      : null,
  ];
}

function renderMoving() {
  const elapsed = el("span", { class: "note" });
  const hint = el("p", { class: "note" });
  moveClock = { elapsed, hint };
  tickMove();
  return el(
    "div",
    { class: "prog", role: "status" },
    el("p", {}, "Moving the cache to ", el("code", { text: moving.to })),
    el("div", { class: "prog-label" },
      el("span", { text: "The cache is stopped while it moves" }), elapsed),
    el("div", { class: "prog-track is-indeterminate" }, el("div", { class: "prog-fill" })),
    hint,
  );
}

function tickMove() {
  if (!moving || !moveClock) return;
  const seconds = Math.max(0, Math.floor((Date.now() - moving.began) / 1000));
  moveClock.elapsed.textContent = `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
  // A large cache on a slow disk takes minutes, and a bar with no end in sight needs to
  // say so before somebody decides it has hung.
  moveClock.hint.textContent = seconds > 120
    ? "Still going — a large cache takes a while. pkgcache status in a terminal says where it is."
    : "";
}

async function moveCache() {
  if (!migration?.movable || moving) return;
  const chosen = await pickDirectory({ title: "Move the cache to…", confirm: "Choose this place" });
  if (chosen === null) return;
  let plan;
  try {
    plan = await api.planMigration(chosen);
  } catch (cause) {
    store.fail(cause);
    return;
  }
  if (!(await confirmMove(plan))) return;
  try {
    await api.startMigration(chosen);
  } catch (cause) {
    store.fail(cause);
    return;
  }
  await waitForMove({ to: plan.to, started: new Date().toISOString() });
}

/** Show what the move would do, and what stands in its way. Resolves true to go ahead.
 *
 * Every problem is shown at once, beside a disabled button, rather than one at a time on
 * the way to a failure: the reader is choosing a disk, and what is wrong with it is the
 * information they need to choose another. */
function confirmMove(plan) {
  return new Promise((resolve) => {
    let close = () => {};
    const done = (answer) => {
      close();
      resolve(answer);
    };
    const problems = plan.problems || [];
    const lines = [
      el("p", { class: "dlg-line" },
        el("strong", { text: bytes(plan.bytes) }), ` in ${count(plan.files)} files`),
      el("p", { class: "dlg-line" }, "from ", el("code", { text: plan.from })),
      el("p", { class: "dlg-line" }, "to ", el("code", { text: plan.to })),
    ];
    if (plan.same_disk) {
      lines.push(el("p", {
        class: "dlg-line dlg-warn",
        text: "That is the disk it is already on, so this moves it without freeing any space.",
      }));
    } else if (plan.free_bytes >= 0) {
      lines.push(el("p", { class: "dlg-line", text: `That disk has ${bytes(plan.free_bytes)} free.` }));
    }
    for (const problem of problems) {
      lines.push(el("p", { class: "dlg-line dlg-problem", text: problem }));
    }
    if (!problems.length) {
      lines.push(el("p", {
        class: "dlg-line note",
        text: "The cache stops while it moves and this page reconnects when it is back. The old copy is removed only once the new one has been counted and matches.",
      }));
    }
    const go = button("Move the cache", () => done(true), {
      kind: "primary",
      disabled: problems.length > 0,
    });
    close = openModal({
      title: "Move the cache",
      wide: true,
      body: el("div", { class: "dlg-body" }, ...lines),
      onCancel: () => resolve(false),
      actions: [button("Cancel", () => done(false)), go],
    });
    // The safe answer holds focus, as in every other question this console asks.
    go.parentElement?.firstElementChild?.focus();
  });
}

/** Wait out a move this page cannot watch, and report it when a daemon answers. */
async function waitForMove(record) {
  if (moving) return;
  moving = { to: record.to, began: Date.parse(record.started) || Date.now() };
  moveOutcome = null;
  drawMove();
  const timer = setInterval(tickMove, 1000);
  try {
    for (;;) {
      await pause(1500);
      let location;
      try {
        location = await api.migration();
      } catch {
        // Expected: this is the stretch where nothing is answering.
        continue;
      }
      const last = location.last;
      if (last && last.state !== "moving") {
        migration = location;
        moving = null;
        await reportMove(last);
        return;
      }
      // Answered with no record at all, from the place it was going: another window saw
      // the outcome first and acknowledged it.
      if (!last && location.data_dir === record.to) {
        migration = location;
        moveOutcome = { ok: true, text: `The cache is now in ${record.to}.` };
        return;
      }
    }
  } finally {
    clearInterval(timer);
    moving = null;
    moveClock = null;
    drawMove();
  }
}

async function reportMove(last) {
  try {
    await api.acknowledgeMigration();
    migration = await api.migration();
  } catch {
    // Shown again by the next window to ask, which is harmless.
  }
  try {
    await store.loadInstance();
    await store.loadProject();
  } catch {
    // The event stream reconnects on its own; the figures follow with it.
  }
  if (last.state === "done") {
    moveOutcome = {
      ok: true,
      text: `The cache is now in ${last.to}` + (last.renamed
        ? " — the same disk, so nothing was copied."
        : `. ${bytes(last.bytes || 0)} moved, and the old copy is gone.`),
    };
    store.notify(moveOutcome.text);
  } else {
    moveOutcome = { ok: false, text: `The cache was not moved: ${last.error || "the move failed"}.` };
    store.fail(new Error(moveOutcome.text));
  }
  drawMove();
}

/* ---- sharing the console: pkgcache -------------------------------------------
 *
 * The cache stays on 127.0.0.1 for this machine whatever happens here. Sharing opens a
 * second address other machines can reach, serving this console — and only the console —
 * behind a password, and stopping closes it again. pkgcache has no accounts, so this is
 * the whole of its access control, and the reason there is no Accounts panel above. */

const shareRegion = region("div", { class: "stack" });
let sharing = null;
let sharingError = "";

async function loadSharing() {
  try {
    sharing = await api.sharing();
    sharingError = "";
  } catch (cause) {
    sharing = null;
    sharingError = cause.message;
  }
  drawShare();
}

function drawShare() {
  shareRegion.set(renderShare());
}

function renderShare() {
  if (!sharing) {
    return sharingError
      ? el("p", { class: "empty", text: sharingError })
      : loading("Asking who can open this console");
  }
  // Somebody reading this through the shared address is about to act on their own way in.
  const remote = store.state.me?.shared === true;
  const urls = sharing.urls || [];
  let state;
  if (!sharing.enabled) {
    state = el("p", {
      class: "note",
      text: "Only this machine can open this console. Sharing opens a second address, on " +
        `port ${sharing.port}, where other machines open it with a password. Packages stay ` +
        "this machine's: that address serves the console and nothing else.",
    });
  } else if (sharing.error) {
    state = el("p", {
      class: "form-error",
      text: `Meant to be shared on port ${sharing.port}, but other machines cannot reach it: ${sharing.error}`,
    });
  } else if (!urls.length) {
    state = el("p", {
      class: "note",
      text: `Shared on port ${sharing.port}, but this machine has no network address right now.`,
    });
  } else {
    state = el("div", {},
      el("p", { class: "note", text: "Other machines open it at" }),
      el("ul", { class: "share-urls" },
        urls.map((url) => el("li", {}, el("code", { text: `${url}/console` })))));
  }
  return [
    state,
    sharing.enabled
      ? el("p", {
          class: "note",
          text: "Anyone with the password can do what you can here, moving the cache " +
            "included. Plain HTTP: the password crosses the network readable.",
        })
      : null,
    remote
      ? el("p", {
          class: "note",
          text: "You are using the shared address, so changing the password or stopping sharing signs you out too.",
        })
      : null,
    el("div", { class: "link-row" },
      sharing.enabled
        ? [
            button("Change password…", () => shareConsole()),
            button("Stop sharing", () => stopSharing(), { kind: "danger" }),
          ]
        : button("Share with a password…", () => shareConsole(), { kind: "primary" })),
  ];
}

/** Share the console behind a password, or change the password of one that is shared.
 *  The same dialog, because it is the same request. */
async function shareConsole() {
  const changing = sharing?.enabled === true;
  const answer = await askPassword({
    title: changing ? "Change the password" : "Share this console",
    wide: !changing,
    lines: changing
      ? ["Everybody signed in with the old password is signed out."]
      : [
          `Anyone on your network with this password can open this console on port ${sharing.port} and do what you can here, moving the cache included. Packages stay this machine's.`,
          el("p", { class: "dlg-line dlg-warn", text: "Plain HTTP: the password crosses the network readable." }),
        ],
    confirmLabel: changing ? "Change it" : "Share the console",
    submit: (password) => api.share(password),
  });
  if (!answer) return;
  sharing = answer;
  drawShare();
  store.notify(changing
    ? "Password changed. Everybody else has to sign in again."
    : sharing.urls?.length
      ? `Shared. Other machines open ${sharing.urls[0]}/console.`
      : `Shared on port ${sharing.port}.`);
}

async function stopSharing() {
  const remote = store.state.me?.shared === true;
  if (!await askConfirm({
    title: "Stop sharing",
    body: "Other machines lose the console straight away, including anyone using it now.\n" +
      (remote ? "That includes this page: you are using the shared address." : "This machine is not affected."),
    confirmLabel: "Stop sharing",
    danger: true,
  })) return;
  try {
    sharing = await api.unshare();
  } catch (cause) {
    store.fail(cause);
    return;
  }
  drawShare();
  store.notify("Only this machine can open the console now.");
}

/** Ask for a new password, twice. `submit` is handed it and may throw; the refusal is
 *  shown in place, beside what was typed, rather than closing the dialog — a taken port or
 *  a password the server will not accept is something to read and correct, not a reason
 *  to start over. Resolves to what submit returned, or null if cancelled. */
function askPassword({ title, lines = [], confirmLabel, submit, wide = false }) {
  return new Promise((resolve) => {
    const first = el("input", {
      class: "dlg-input", type: "password", autocomplete: "new-password", required: true,
      "aria-label": "Password",
    });
    const second = el("input", {
      class: "dlg-input", type: "password", autocomplete: "new-password", required: true,
      "aria-label": "The same password again",
    });
    const errorRegion = region("div", { class: "dlg-error", role: "alert" });
    let close = () => {};

    async function send() {
      if (first.value.length < MIN_PASSWORD) {
        errorRegion.set(el("span", { text: `At least ${MIN_PASSWORD} characters.` }));
        first.focus();
        return;
      }
      if (first.value !== second.value) {
        errorRegion.set(el("span", { text: "The two passwords are different." }));
        second.focus();
        return;
      }
      let answer;
      try {
        answer = await submit(first.value);
      } catch (cause) {
        errorRegion.set(el("span", { text: cause?.message || String(cause) }));
        return;
      }
      close();
      resolve(answer ?? true);
    }

    const form = el("form", { class: "dlg-body" },
      lines.map((line) => (typeof line === "string" ? el("p", { class: "dlg-line", text: line }) : line)),
      el("span", { class: "dlg-label", text: "Password" }), first,
      el("span", { class: "dlg-label", text: "The same again" }), second,
      errorRegion.node);
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void send();
    });
    // Enter is wired by hand: a form with two fields and no submit button of its own —
    // the dialog's buttons sit outside it — gets no implicit submission at all, so Enter
    // in either field did nothing.
    for (const box of [first, second]) {
      box.addEventListener("keydown", (event) => {
        if (event.key !== "Enter") return;
        event.preventDefault();
        void send();
      });
    }
    close = openModal({
      title,
      wide,
      body: form,
      onCancel: () => resolve(null),
      actions: [
        button("Cancel", () => { close(); resolve(null); }),
        button(confirmLabel, send, { kind: "primary" }),
      ],
    });
    first.focus();
  });
}

/* ---- jobs ------------------------------------------------------------------- */

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
        // The job manager's own words: queued, running, done, failed, cancelled.
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

/* ---- project access: pkgreg -------------------------------------------------
 *
 * Ownership answers "who is responsible for this tenant" and stays a single name. Grants
 * answer "who else works on it", which is a list — and until they existed the only ways
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

  const project = currentProject();
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
    renderOwner(project),
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
              if (!await askConfirm({
                title: "Revoke access",
                body: `Revoke ${row.username}'s access to ${store.state.project}?`,
                confirmLabel: "Revoke", danger: true,
              })) return;
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

/* Who owns the project, and — for a superuser — a way to hand it to somebody else.
 *
 * The server lets only a superuser change an owner, and only to an existing admin or
 * superuser (v1.go patchProject). Without this control an owner could be named and never
 * changed, which dead-ended deleting or demoting any admin who owned a project: the
 * server refuses both until "this account's projects" are reassigned, and nothing on the
 * page could reassign them. */
function renderOwner(project) {
  const me = store.state.me;
  const owner = project?.owner || "";
  if (me?.role !== "superuser") {
    return el("p", {
      class: "note",
      text: owner === me?.username
        ? "Owned by you. Only a superuser can hand it to another account."
        : owner ? `Owned by ${owner}.` : "This project has no owner, so only a superuser can share it.",
    });
  }
  const heirs = (store.state.users || []).filter((row) => row.role === "admin" || row.role === "superuser");
  const options = heirs.map((row) => ({ value: row.username, label: `${row.username} (${row.role})` }));
  if (!owner) options.unshift({ value: "", label: "no owner" });
  else if (!heirs.some((row) => row.username === owner)) options.unshift({ value: owner, label: owner });
  const picker = select("owner", options, owner);
  picker.addEventListener("change", async () => {
    const next = picker.value;
    if (!next || next === owner) {
      picker.value = owner;
      return;
    }
    const ok = await askConfirm({
      title: "Change owner",
      body: `Hand ${project.name} to ${next}?\n` +
        (owner
          ? `${owner} stops being its owner: unless they are a superuser or hold a grant below, they lose access to it.\n`
          : "") +
        `${next} decides who else reaches it from now on.`,
      confirmLabel: "Change owner",
    });
    if (!ok) {
      picker.value = owner;
      return;
    }
    const done = await store.mutate(
      () => api.patchProject(project.name, { owner: next }), `${next} now owns ${project.name}`);
    if (done) await store.loadInstance();
    else picker.value = owner;
  });
  return field("Owner", picker, "an admin or superuser; responsible for the project and who reaches it");
}

/* ---- accounts: pkgreg --------------------------------------------------------
 *
 * Scoped to what the server lets this actor do (auth/accounts.go), so nothing is drawn
 * that can only answer 403: a superuser manages every account; an admin creates users who
 * report to them, resets those users' passwords and deletes them; anyone can change their
 * own password. pkgcache never reaches this — it has no accounts by design. */

function accountsNote() {
  const me = store.state.me;
  if (!me?.auth_enabled) return "authentication is off";
  if (me.role === "superuser") return "every account";
  if (me.role === "admin") return "you and your reports";
  return "your own";
}

function renderUsers() {
  const me = store.state.me;
  if (!me?.auth_enabled) return renderNoAccounts();
  if (store.isGuest() || !me.authenticated) {
    return el("p", { class: "note", text: "Sign in with an account to see accounts." });
  }
  if (me.role !== "superuser" && me.role !== "admin") {
    return el(
      "div",
      { class: "stack" },
      el("p", {
        class: "note",
        text: `Your account is managed by ${me.reports_to || "an admin"} or a superuser. Your password is yours to change.`,
      }),
      button("Change your password…", () => setPassword({ username: me.username })),
    );
  }
  const superuser = me.role === "superuser";
  const rows = store.state.users || [];
  const roles = [
    { value: "user", label: "user" },
    { value: "admin", label: "admin" },
    { value: "superuser", label: "superuser" },
  ];
  const refresh = async () => {
    try {
      store.set({ users: (await api.users()).users || [] });
    } catch (cause) {
      store.fail(cause);
    }
  };

  /* reports_to belongs to the user role alone: an admin or superuser has no manager,
     and the server rejects one. Hiding the field rather than ignoring it means the form
     never shows a value that will be silently dropped. An admin is not offered a role or
     a manager at all: the server makes everything an admin creates a user reporting to
     them, whatever is sent. */
  const form = el(
    "form",
    { class: "form" },
    field("Username", input("username", { required: true, autocomplete: "off" })),
    field("Password", input("password", {
      type: "password", required: true, minlength: String(MIN_PASSWORD), autocomplete: "new-password",
    }), `at least ${MIN_PASSWORD} characters`),
    superuser ? field("Role", select("role", roles, "user")) : null,
    superuser
      ? field("Reports to", select("reports_to", managerOptions("")),
          "the admin this user answers to; they see that admin's projects")
      : el("p", { class: "note", text: "Creates a user who reports to you and sees your projects." }),
    el("button", { class: "btn primary", type: "submit", text: "Create account" }),
  );
  const syncReports = () => {
    if (!superuser) return;
    form.querySelector('select[name="reports_to"]').closest(".field").hidden =
      form.querySelector('select[name="role"]').value !== "user";
  };
  form.querySelector('select[name="role"]')?.addEventListener("change", syncReports);
  syncReports();
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const data = new FormData(form);
    const role = superuser ? String(data.get("role")) : "user";
    const created = await store.mutate(
      () => api.createUser({
        username: String(data.get("username")),
        password: String(data.get("password")),
        role,
        reports_to: role !== "user" ? "" : superuser ? String(data.get("reports_to")) : me.username,
      }),
      "Account created",
    );
    await refresh();
    if (created) {
      form.reset();
      syncReports();
    }
  });

  const self = (row) => row.username === me.username;
  return el(
    "div",
    { class: "stack" },
    superuser
      ? null
      : el("p", {
          class: "note",
          text: "You manage the users who report to you. Roles, reporting lines and other admins belong to a superuser.",
        }),
    table(
      [
        { label: "User", cell: (row) => (self(row) ? `${row.username} (you)` : row.username) },
        {
          label: "Role",
          cell: (row) => {
            // Not the built-in root, which lives in configuration, and not your own: the
            // server refuses both (accounts.go Update), and a picker that can only fail
            // is how a form teaches people its controls are unreliable.
            if (!superuser || row.builtin || self(row)) return row.role;
            const picker = select("role", roles, row.role);
            picker.addEventListener("change", () => changeRole(row, picker, refresh));
            return picker;
          },
        },
        {
          label: "Reports to",
          cell: (row) => {
            // Only a user has a manager, and the built-in root account is not stored,
            // so neither offers a picker — the dash is the whole truth for both.
            if (row.role !== "user" || row.builtin) return "—";
            if (!superuser) return row.reports_to || "—";
            const picker = select("reports_to", managerOptions(row.username), row.reports_to || "");
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
        { label: "Created", cell: (row) => (row.builtin ? "—" : ago(row.created_at)) },
        {
          label: "",
          cell: (row) => {
            const mayReset = !row.builtin &&
              (superuser || self(row) || row.reports_to === me.username);
            const mayDelete = !row.builtin && !self(row) &&
              (superuser || (row.role === "user" && row.reports_to === me.username));
            if (!mayReset && !mayDelete) return "—";
            return el("div", { class: "link-row" },
              mayReset
                ? button(self(row) ? "Change password…" : "Reset password…",
                    () => setPassword(row), { kind: "ghost small" })
                : null,
              mayDelete
                ? button("Delete", () => deleteAccount(row, superuser, refresh), { kind: "ghost small danger" })
                : null);
          },
        },
      ],
      rows,
      { empty: "No accounts." },
    ),
    form,
  );
}

/** pkgreg with no accounts. pkgcache never gets here: it has no accounts by design and
 *  never reads PKGREG_ROOT_USER, so the instruction this used to give it was false. */
function renderNoAccounts() {
  return el(
    "div",
    { class: "stack" },
    el("p", {
      class: "note",
      text: "Authentication is off: there are no accounts, and anyone who can reach this console or the API can do everything here.",
    }),
    el("p", {
      class: "note",
      text: "Run this on the server, with the -data-dir or -config it serves with. It creates a superuser and prints its password once; authentication is on from that moment.",
    }),
    commandBlock("pkgreg init"),
    el("p", {
      class: "note",
      text: "auth.root_user and PKGREG_ROOT_USER are a break-glass login read from configuration, not the way to set up accounts.",
    }),
  );
}

/* Only an admin or superuser can be a manager, and nobody can manage themselves. There
   is no "none" option: a user must report to someone, the server refuses an empty
   manager, and offering a choice it will reject is how a form teaches people that its
   controls are unreliable. */
function managerOptions(subject) {
  return managerNames(subject).map((name) => {
    const row = (store.state.users || []).find((user) => user.username === name);
    return { value: name, label: `${name} (${row?.role})` };
  });
}

function managerNames(subject) {
  return (store.state.users || [])
    .filter((row) => row.username !== subject && (row.role === "admin" || row.role === "superuser"))
    .map((row) => row.username);
}

/* What an account still answers for. The server will not orphan either: it refuses to
 * delete an account that has reports or owns projects, and to demote one to a user for
 * the same reasons (accounts.go Update, Delete). Read from what a superuser can see,
 * which is every account and every project — the only actor these refusals reach. */
function holdings(username) {
  return {
    reports: (store.state.users || []).filter((row) => row.reports_to === username).map((row) => row.username),
    projects: store.state.projects.filter((project) => project.owner === username).map((project) => project.name),
  };
}

function describeHoldings(held) {
  const parts = [];
  if (held.reports.length) parts.push(`manages ${held.reports.join(", ")}`);
  if (held.projects.length) parts.push(`owns ${held.projects.join(", ")}`);
  return parts.join(" and ");
}

async function handOver(held, to) {
  for (const report of held.reports) await api.patchUser(report, { reports_to: to });
  for (const project of held.projects) await api.patchProject(project, { owner: to });
}

async function deleteAccount(row, superuser, refresh) {
  const held = superuser ? holdings(row.username) : { reports: [], projects: [] };
  const owes = held.reports.length + held.projects.length > 0;
  if (!owes) {
    if (!await askConfirm({
      title: "Delete account",
      body: `Delete the account ${row.username}?`,
      confirmLabel: "Delete", danger: true,
    })) return;
    await store.mutate(() => api.deleteUser(row.username), `Deleted ${row.username}`);
    await refresh();
    return;
  }
  // The refusal the server would give, turned into the question that answers it.
  const heirs = managerNames(row.username);
  if (!heirs.length) {
    store.fail(new Error(
      `${row.username} ${describeHoldings(held)}, and there is no other admin or superuser to hand them to. Create one first.`));
    return;
  }
  const to = await askChoice({
    title: `Delete ${row.username}`,
    label: `${row.username} ${describeHoldings(held)}. Those go to another account before the account is deleted. Which one?`,
    choices: heirs,
    confirmLabel: "Hand over and delete",
  });
  if (to === null) return;
  await store.mutate(async () => {
    await handOver(held, to);
    return api.deleteUser(row.username);
  }, `Deleted ${row.username}; ${to} took over what they had`);
  await store.loadInstance();
  await refresh();
}

/* A role change. Promotion is one request; demotion to user is not, because a user must
 * report to somebody and the server refuses one that does not — so the role picker on its
 * own could never demote anybody. The manager is asked for here, and whatever the account
 * still answers for goes to the same person. */
async function changeRole(row, picker, refresh) {
  const next = picker.value;
  if (next === row.role) return;
  if (next !== "user") {
    const done = await store.mutate(
      () => api.patchUser(row.username, { role: next }), `${row.username} is now ${next}`);
    if (!done) picker.value = row.role;
    await refresh();
    return;
  }
  const held = holdings(row.username);
  const owes = held.reports.length + held.projects.length > 0;
  const heirs = managerNames(row.username);
  if (!heirs.length) {
    store.fail(new Error(`A user reports to an admin or superuser, and there is no other one for ${row.username} to report to.`));
    picker.value = row.role;
    return;
  }
  const to = await askChoice({
    title: `Make ${row.username} a user`,
    label: `A user reports to an admin or superuser. ${row.username} will report to:` +
      (owes ? ` ${row.username} ${describeHoldings(held)}; those go to the same account.` : ""),
    choices: heirs,
    confirmLabel: "Make a user",
  });
  if (to === null) {
    picker.value = row.role;
    return;
  }
  const done = await store.mutate(async () => {
    await handOver(held, to);
    return api.patchUser(row.username, { role: "user", reports_to: to });
  }, `${row.username} is now a user reporting to ${to}`);
  if (!done) picker.value = row.role;
  if (owes) await store.loadInstance();
  await refresh();
}

/** Set an account's password: your own, or — for an admin — a report's, or anyone's for a
 *  superuser. PATCH /users/{name} takes `password` and checks which (accounts.go Update). */
async function setPassword(row) {
  const self = row.username === store.state.me?.username;
  const changed = await askPassword({
    title: self ? "Change your password" : `Reset ${row.username}'s password`,
    // Sessions are not ended by a password change (auth/sessions.go), so this says so
    // rather than letting a reset read as a sign-out.
    lines: [self
      ? "Sessions you already have, here and elsewhere, stay signed in."
      : `${row.username} signs in with this from now on. Nobody is told: hand it to them yourself. Sessions they already have stay signed in until they expire.`],
    confirmLabel: self ? "Change it" : "Reset it",
    submit: (password) => api.patchUser(row.username, { password }),
  });
  if (changed) store.notify(self ? "Password changed." : `${row.username}'s password was reset.`);
}

/* ---- audit ------------------------------------------------------------------ */

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
  return pairs.length ? pairs.map(([key, value]) => `${key}=${typeof value === "object" ? JSON.stringify(value) : value}`).join(" ") : "—";
}

/* ---- lifecycle ---------------------------------------------------------------- */

function renderLifecycle() {
  const canCreate = store.isSuperuser() || store.state.me?.role === "admin";
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
      await store.setProject(name);
    }
    form.reset();
  });

  const current = store.state.project;
  const owner = currentProject()?.owner;
  // The server's rule (api.go requireOwner): a superuser or the project's own owner, and
  // with no accounts, anyone. A grant to operate is not enough to remove the project.
  const mayDelete = store.isSuperuser() || store.canGrant();
  let refusal = "";
  if (current === "global") refusal = "The global project cannot be deleted.";
  else if (!mayDelete) {
    refusal = owner
      ? `Only a superuser or its owner, ${owner}, can delete this project.`
      : "Only a superuser can delete this project.";
  }
  return el(
    "div",
    { class: "stack" },
    form,
    refusal
      ? el("p", { class: "note", text: refusal })
      : button(`Delete ${current}`, async () => {
          if (!await askConfirm({
            title: "Delete project",
            body: `Delete the project ${current}?\nIts cached entries stop being served. Blobs shared with other projects are untouched.`,
            confirmLabel: "Delete", danger: true,
          })) return;
          // Not through store.mutate: that reloads the current project's slice afterwards,
          // and the current project is the one just deleted — so a delete that worked was
          // reported as "no such project", and the page stayed on it.
          try {
            await api.deleteProject(current);
          } catch (cause) {
            store.fail(cause);
            return;
          }
          store.notify(`Deleted ${current}`);
          await store.loadInstance();
          // The page must not go on naming a project that is gone — every panel would
          // ask for it and be told 404. The daemon has already moved the machine's own
          // choice off it for the same reason; this is the same move, here. Global when
          // this actor can see it, and otherwise a project they can: an admin who just
          // deleted their own project was sent to global and met a wall of 403s.
          const visible = store.state.projects.map((project) => project.name);
          await store.setProject(visible.includes("global") ? "global" : (visible[0] ?? "global"));
        }, { kind: "danger" }),
  );
}
