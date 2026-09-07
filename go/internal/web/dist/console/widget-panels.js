/* The three panels that make the widget a control surface rather than a monitor.
 *
 * They live apart from widget.js for the reason the console's views do: the glance view is
 * the thing that must stay small and fast, and it should not grow every time a control is
 * added somewhere else in the window.
 *
 * Each panel is a mount function returning { node, refresh }. The shell calls refresh when
 * the panel is entered and after any mutation, and never renders a panel that is not on
 * screen — at 420px only one is, and drawing the other two would cost three fetches to
 * paint one.
 */

import { api } from "./api.js";
import { askChoice, askConfirm, askText } from "./dialog.js";
import { canBrowse, pickDirectory, pickPack } from "./picker.js";
import * as store from "./store.js";
import { el, region, button, fill } from "./dom.js";
import { bytes, ago } from "./format.js";

/* ---- packages ------------------------------------------------------------- */

/** What this project is holding, largest first, with the ones you can drop.
 *
 * Largest first and not newest: the question this panel answers is "what is taking the
 * room", and the answer is almost never the most recent thing fetched. */
export function packagesPanel({ notice, reload }) {
  const rows = region("div", { class: "wg-rows" });
  const summary = region("div", { class: "wg-panel-note" });
  const selected = new Set();
  let sort = "size";

  const sortToggle = el(
    "div",
    { class: "wg-seg wg-seg-small", role: "group", "aria-label": "Order" },
    ...[
      ["size", "largest"],
      ["date", "newest"],
    ].map(([value, label]) =>
      el("button", {
        class: "wg-seg-item" + (sort === value ? " on" : ""),
        text: label,
        dataset: { sort: value },
        onclick: (event) => {
          sort = event.target.dataset.sort;
          for (const item of sortToggle.children) {
            item.classList.toggle("on", item.dataset.sort === sort);
          }
          void refresh();
        },
      }),
    ),
  );

  const remove = button("Remove selected", () => removeSelected(), { kind: "danger" });
  const actions = el(
    "div",
    { class: "wg-actions" },
    remove,
    button("Reclaim space", () =>
      run(() => api.gc(false), "Reclaimed what nothing referenced."),
    ),
  );

  async function removeSelected() {
    if (!selected.size) {
      notice("Nothing is selected.");
      return;
    }
    const digests = [...selected];
    // Asked once, in the language of what will happen. Removal is reversible only by
    // fetching the packages again, which is exactly what somebody short of disk does not
    // want to discover afterwards.
    if (!await askConfirm({
      title: "Remove packages",
      body: `Remove ${digests.length} package${digests.length === 1 ? "" : "s"} from this project?\nThe bytes go if nothing else holds them. Anything a checkpoint holds is kept.`,
      confirmLabel: "Remove", danger: true,
    })) {
      return;
    }
    await run(async () => {
      const result = await api.removeArtifacts(store.state.project, digests);
      selected.clear();
      let said = `Removed ${result.entries}, reclaiming ${bytes(result.reclaimed_bytes)}.`;
      if (result.pinned) {
        said += ` ${result.pinned} kept: a checkpoint holds them.`;
      }
      return said;
    });
  }

  async function run(operation) {
    try {
      const said = await operation();
      await reload();
      await refresh();
      notice(typeof said === "string" ? said : "Done.");
    } catch (cause) {
      notice(cause?.message || String(cause), true);
    }
  }

  async function refresh() {
    const project = store.state.project;
    const page = await api.artifacts(project, { sort, page: 1 });
    if (store.state.project !== project) return;
    const listed = page.artifacts ?? [];
    summary.set(
      el("span", {
        text: listed.length
          ? `${page.total} in ${project}, ${listed.length} shown`
          : `Nothing cached in ${project} yet.`,
      }),
    );
    rows.set(listed.map((artifact) => packageRow(artifact, selected, syncRemove)));
    syncRemove();
  }

  function syncRemove() {
    remove.disabled = selected.size === 0;
    remove.textContent = selected.size
      ? `Remove ${selected.size} selected`
      : "Remove selected";
  }

  return {
    node: el(
      "div",
      { class: "wg-panel" },
      el("div", { class: "wg-panel-head" }, el("span", { class: "wg-label", text: "Packages" }), sortToggle),
      summary.node,
      rows.node,
      actions,
    ),
    refresh,
  };
}

function packageRow(artifact, selected, onChange) {
  const digest = artifact.digest;
  const checkbox = el("input", {
    type: "checkbox",
    checked: selected.has(digest),
    "aria-label": `Select ${artifact.name}`,
    onchange: (event) => {
      if (event.target.checked) selected.add(digest);
      else selected.delete(digest);
      onChange();
    },
  });
  return el(
    "label",
    { class: "wg-row wg-row-pick" },
    checkbox,
    el(
      "span",
      { class: "wg-row-main" },
      el("span", { class: "wg-row-name", text: artifact.name }),
      el("span", {
        class: "wg-row-sub",
        text: [artifact.eco, artifact.version, artifact.arch].filter(Boolean).join(" · "),
      }),
    ),
    el(
      "span",
      { class: "wg-row-right" },
      el("span", { class: "wg-row-size", text: bytes(artifact.size) }),
      el("span", { class: "wg-row-sub", text: ago(artifact.cached_at) }),
    ),
  );
}

/* ---- transfer ------------------------------------------------------------- */

/** Checkpoints, packs, and going back to one.
 *
 * The head is marked because it is the fact both ends of a transfer need: a pack is
 * accepted only where its starting point is the receiver's checkpoint. */
export function transferPanel({ notice, reload, settle }) {
  const list = region("div", { class: "wg-rows" });
  const barRegion = region("div", {});
  let head = "";

  /* A pack is the one thing this window does that takes minutes. Without a bar the
   * button goes quiet and the only evidence anything is happening is the disk light. */
  function drawBars() {
    const running = [...store.state.packs.values()];
    barRegion.set(
      running.length
        ? running.map((pack) => {
            const share = pack.total > 0 ? pack.done / pack.total : 0;
            return el(
              "div",
              { class: "wg-bar" },
              el("div", { class: "wg-bar-label" },
                el("span", { text: pack.action === "import" ? "Importing" : "Exporting" }),
                el("span", { class: "wg-bar-count",
                  text: `${pack.done} / ${pack.total}` })),
              el("div", { class: "wg-bar-track", role: "progressbar",
                "aria-valuemin": "0", "aria-valuemax": String(pack.total),
                "aria-valuenow": String(pack.done) },
                el("div", { class: "wg-bar-fill", style: `width: ${Math.round(share * 100)}%` })),
            );
          })
        : null,
    );
  }
  store.on(["packs"], drawBars);
  drawBars();

  async function refresh() {
    const project = store.state.project;
    const answer = await api.snapshots(project);
    if (store.state.project !== project) return;
    head = answer.head ?? "";
    const snapshots = answer.snapshots ?? [];
    list.set(
      snapshots.length
        ? snapshots.map((snapshot) => checkpointRow(snapshot, head, rollTo))
        : el("div", { class: "wg-quiet", text: "No checkpoints yet." }),
    );
  }

  async function rollTo(id) {
    // The one action in this window that discards something. Named in the question, not
    // softened: what goes is whatever the project has cached since that checkpoint.
    if (!await askConfirm({
      title: "Roll back",
      body: `Go back to checkpoint ${id.slice(0, 12)}?\nWhat this project has cached since then stops being served until it is fetched again. The bytes are shared, so nothing else loses them.`,
      confirmLabel: "Roll back", danger: true,
    })) {
      return;
    }
    await guard(async () => {
      await settle(await api.rollback(store.state.project, id));
      return `Back at ${id.slice(0, 12)}.`;
    });
  }

  async function checkpoint() {
    const message = prompt("What is this checkpoint for?", "before a trip");
    if (message === null) return;
    if (!message.trim()) {
      notice("A checkpoint nobody labelled is one nobody can choose later.", true);
      return;
    }
    await guard(async () => {
      await settle(await api.checkpoint(store.state.project, message.trim()));
      return "Checkpoint recorded.";
    });
  }

  async function exportPack() {
    // Where it goes is asked before anything is built, because a pack is gigabytes and
    // the question is cheap. Declining the picker keeps the old behaviour — the cache's
    // own outbox — rather than cancelling the export.
    let dir = null;
    if (await canBrowse()) {
      dir = await pickDirectory();
      if (dir === null) return;
    }
    await guard(async () => {
      // Reads the head; it does not make one. This used to take a checkpoint before every
      // export, which guaranteed there was something to export and that it was current —
      // and left a row labelled "widget export" behind every single time, identical in
      // content to the checkpoint before it and distinguishable only by the timestamp
      // baked into its manifest, which is what gives two identical checkpoints two ids.
      // A list of checkpoints somebody chose is worth reading; one mostly composed of
      // exports is not, and exporting is a read that has no business writing history.
      //
      // Read fresh rather than trusted from the last refresh, because the answer decides
      // whether a job is submitted at all.
      const answer = await api.snapshots(store.state.project);
      const target = answer.head ?? "";
      if (!target) {
        // Reachable now, and it was not before — the checkpoint above always made one.
        // The server's own words for this are "project has no checkpoint to export",
        // which is true and does not say what to do about it.
        throw new Error(
          "No checkpoint to export. Take one first: a pack is built from a checkpoint " +
            "and carries what that checkpoint captured.",
        );
      }
      const job = await settle(
        await api.exportPack(store.state.project, dir ? { dir } : {}));
      const wrote = /wrote (\S+)/.exec(job.log ?? "");
      // Names the checkpoint it packed. What a pack contains is now a question with an
      // answer the person can check, rather than "everything, probably".
      const from = `, from checkpoint ${target.slice(0, 12)}`;
      return wrote
        ? ["The pack is at ", el("code", { text: wrote[1] }), from]
        : `Pack written${from}.`;
    });
  }

  async function importPack() {
    // Which pack, rather than "whatever single .tar is in the inbox" — which failed
    // outright with two of them there, and silently read the wrong one when somebody had
    // put a second one in since.
    let file = null;
    if (await canBrowse()) {
      file = await pickPack();
      if (file === null) return;
    }
    await guard(async () => {
      const job = await settle(
        await api.importPack(store.state.project, file ? { file } : {}));
      const applied = /imported checkpoint (\S+)/.exec(job.log ?? "");
      return applied ? `Applied ${applied[1].slice(0, 12)}.` : "The pack was applied.";
    });
  }

  async function guard(operation) {
    try {
      const said = await operation();
      await reload();
      await refresh();
      notice(said);
    } catch (cause) {
      notice(cause?.message || String(cause), true);
    }
  }

  return {
    node: el(
      "div",
      { class: "wg-panel" },
      el("div", { class: "wg-panel-head" }, el("span", { class: "wg-label", text: "Checkpoints" })),
      barRegion.node,
      list.node,
      el(
        "div",
        { class: "wg-actions" },
        button("Checkpoint now", () => checkpoint()),
        button("Export a pack", () => exportPack()),
        button("Import a pack", () => importPack()),
      ),
      el("div", {
        class: "wg-panel-note",
        text: "A pack carries the latest checkpoint, so take one to include what has been cached since the last. It is applied as it is read, and refused unless it continues from this project's checkpoint — so importing one cannot lose what is here.",
      }),
    ),
    refresh,
  };
}

function checkpointRow(snapshot, head, rollTo) {
  const current = snapshot.id === head;
  return el(
    "div",
    { class: "wg-row" + (current ? " is-current" : "") },
    el("span", { class: "wg-row-mark", text: current ? "*" : "", "aria-hidden": "true" }),
    el(
      "span",
      { class: "wg-row-main" },
      el("span", { class: "wg-row-name", text: snapshot.subject || snapshot.id.slice(0, 12) }),
      el("span", {
        class: "wg-row-sub",
        text: `${snapshot.id.slice(0, 12)} · ${snapshot.entry_count} entries · ${bytes(snapshot.total_bytes)}`,
      }),
    ),
    current
      ? el("span", { class: "wg-row-sub", text: "here" })
      : button("Go back", () => rollTo(snapshot.id), { kind: "danger" }),
  );
}

/* ---- sources -------------------------------------------------------------- */

/** Where this project's misses go, and how to change it.
 *
 * The fingerprint field is not optional and not remembered: it is the whole trust
 * decision, and it comes from a person who was told it, not from this network. */
export function sourcesPanel({ notice, reload }) {
  const current = region("div", { class: "wg-rows" });
  const formRegion = region("div", {});
  const peerRegion = region("div", {});

  async function refresh() {
    void refreshPeers();
    let answer;
    try {
      answer = await api.sources();
    } catch (cause) {
      // 404 is the honest answer on an instance that has no local sources — a server.
      current.set(el("div", { class: "wg-quiet", text: cause?.message || String(cause) }));
      formRegion.set();
      return;
    }
    const states = answer.sources ?? [];
    const mine = states.find((state) => state.project === store.state.project);
    current.set(sourceRows(states, store.state.project));
    formRegion.set(sourceForm(mine));
  }

  /* The other machines this project borrows from. A team cache is a server somebody
   * runs; these are laptops on the same desk, and the window is where somebody sitting
   * at one of them would think to add the other. */
  async function refreshPeers() {
    const project = store.state.project;
    let peers;
    try {
      peers = (await api.peers(project)).peers ?? [];
    } catch {
      peerRegion.set();
      return;
    }
    if (store.state.project !== project) return;
    peerRegion.set(el("div", { class: "wg-peers" },
      el("div", { class: "wg-label", text: "other machines" }),
      ...(peers.length
        ? peers.map((peer) => el("div", { class: "wg-peer" },
            el("div", { class: "wg-peer-head" },
              el("span", { text: peer.name }),
              button("Forget", () => forgetPeer(project, peer), { kind: "danger" })),
            el("div", { class: "wg-quiet", text: `${peer.url} → ${peer.their_project}` }),
            el("div", { class: "wg-quiet", text: `through ${peer.through.join(", ") || "nothing"}` }),
            el("div", { class: "wg-quiet",
              text: peer.offline.length
                ? `offline ${peer.offline.join(", ")}`
                : "offline nothing — no token" })))
        : [el("div", { class: "wg-quiet", text: "None. Add one to borrow from a machine beside you." })]),
      button("Add a machine…", () => void addPeer(project))));
  }

  async function addPeer(project) {
    const address = await askText({
      title: "Borrow from another machine",
      label: "Its address",
      placeholder: "sams-laptop or 192.168.1.4:41780",
      confirmLabel: "Next",
    });
    if (address === null) return;

    // That machine is asked what it has before anybody is asked to name one. Two
    // machines number their projects independently, so this is a real question — and a
    // menu of names it actually has cannot be answered with a typo.
    let theirProject = "global";
    try {
      notice("Reaching that machine…");
      const reached = await api.reach(project, { address });
      const choices = reached.projects || [];
      if (choices.length > 1) {
        const picked = await askChoice({
          title: "Which of their projects?",
          label: `${reached.url} has these`,
          choices,
          confirmLabel: "Borrow from it",
        });
        if (picked === null) return;
        theirProject = picked;
      } else if (choices.length === 1) {
        theirProject = choices[0];
      } else {
        // It answered but would not list them, which a cache with accounts does. Fall
        // back to asking, rather than assuming a name it may not have.
        const typed = await askText({
          title: "Which project on their side?",
          label: reached.reason || "That machine did not list its projects",
          placeholder: "global", value: "global", confirmLabel: "Borrow from it",
        });
        if (typed === null) return;
        theirProject = typed;
      }
    } catch (cause) {
      notice(cause?.message || String(cause), true);
      return;
    }

    try {
      const added = await api.addPeer(project, {
        address, their_project: theirProject,
      });
      await reload();
      await refresh();
      notice(`${project} fetches through ${added.name}.`);
    } catch (cause) {
      notice(cause?.message || String(cause), true);
    }
  }

  async function forgetPeer(project, peer) {
    if (!await askConfirm({
      title: "Forget this machine",
      body: `Stop borrowing from ${peer.name}?\nMisses go to the public registries instead. Nothing already cached here is lost.`,
      confirmLabel: "Forget", danger: true,
    })) return;
    try {
      await api.forgetPeer(project, peer.name);
      await reload();
      await refresh();
      notice(`No longer borrowing from ${peer.name}.`);
    } catch (cause) {
      notice(cause?.message || String(cause), true);
    }
  }

  function sourceForm(state) {
    // A project always has a state; having a *source* is a different question. Reading the
    // object as "configured" put Update and Forget on a project that goes straight to the
    // registries, where there is nothing to update and nothing to forget.
    const configured = Boolean(state?.server);
    const server = el("input", {
      name: "server",
      placeholder: "https://cache.internal:8443",
      value: state?.server ?? "",
      autocomplete: "off",
      spellcheck: "false",
    });
    const fingerprint = el("input", {
      name: "fingerprint",
      placeholder: "AB:CD:… the fingerprint you were given",
      autocomplete: "off",
      spellcheck: "false",
    });
    const teamProject = el("input", {
      name: "team_project",
      placeholder: "global",
      value: state?.team_project ?? "",
      autocomplete: "off",
    });
    const direct = el("input", { type: "checkbox", checked: state ? state.direct : true });

    const form = el(
      "form",
      { class: "wg-form" },
      field("Team cache", server),
      field("CA fingerprint", fingerprint, "from your colleague, not from this network"),
      field("Project on their side", teamProject, "empty means their global project"),
      el(
        "label",
        { class: "wg-check" },
        direct,
        el("span", { text: "fall back to the public registry when it is unreachable" }),
      ),
      el(
        "div",
        { class: "wg-actions" },
        el("button", {
          class: "btn",
          type: "submit",
          text: configured ? "Update" : "Use this cache",
        }),
        // Only where there is something of this project's own to remove. An inherited
        // source belongs to another project, and forgetting it from here would either do
        // nothing or take it away from everyone.
        configured && !state.inherited
          ? button("Forget", () => forget(), { kind: "danger" })
          : null,
      ),
    );
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      await apply({
        server: server.value.trim(),
        ca_sha256: fingerprint.value.trim(),
        team_project: teamProject.value.trim(),
        direct: direct.checked,
      });
    });
    return form;
  }

  async function apply(spec) {
    try {
      notice("Verifying the cache…");
      const state = await api.putSource(store.state.project, spec);
      await reload();
      await refresh();
      notice(`${store.state.project} now goes through ${state.server}.`);
    } catch (cause) {
      notice(cause?.message || String(cause), true);
    }
  }

  async function forget() {
    try {
      await api.deleteSource(store.state.project);
      await reload();
      await refresh();
      notice("Forgotten.");
    } catch (cause) {
      notice(cause?.message || String(cause), true);
    }
  }

  return {
    node: el(
      "div",
      { class: "wg-panel" },
      el("div", { class: "wg-panel-head" }, el("span", { class: "wg-label", text: "Every project" })),
      current.node,
      formRegion.node,
      peerRegion.node,
    ),
    refresh,
  };
}

function sourceRows(states, project) {
  return states.map((state) =>
    el(
      "div",
      { class: "wg-row" + (state.project === project ? " is-current" : "") },
      el("span", { class: "wg-row-mark", text: state.project === project ? "*" : "", "aria-hidden": "true" }),
      el(
        "span",
        { class: "wg-row-main" },
        el("span", { class: "wg-row-name", text: state.project }),
        el("span", {
          class: "wg-row-sub",
          text: state.server
            ? host(state.server) + (state.inherited ? " ↑ inherited" : "") +
              (state.direct ? "" : " · never direct")
            : "straight to the registries",
        }),
      ),
      state.server
        ? el("span", {
            class: "wg-row-sub " + (state.reachable ? "is-up" : "is-down"),
            text: state.reachable ? "up" : "down",
          })
        : null,
    ),
  );
}

function host(server) {
  return String(server).replace(/^https?:\/\//, "");
}

function field(label, control, hint) {
  return el(
    "label",
    { class: "wg-field" },
    el("span", { class: "wg-field-label", text: label }),
    control,
    hint ? el("span", { class: "wg-field-hint", text: hint }) : null,
  );
}
