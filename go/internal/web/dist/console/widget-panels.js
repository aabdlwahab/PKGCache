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
import { askChoice, askConfirm, askText, openModal } from "./dialog.js";
import { canBrowse, pickDirectory, pickPack } from "./picker.js";
import * as store from "./store.js";
import { el, region, button } from "./dom.js";
import { bytes, ago } from "./format.js";

/* ---- packages ------------------------------------------------------------- */

/** What this project is holding, largest first, with the ones you can drop.
 *
 * Largest first and not newest: the question this panel answers is "what is taking the
 * room", and the answer is almost never the most recent thing fetched. */
export function packagesPanel({ notice, reload, reclaim }) {
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
  const copy = button("Copy to…", () => transfer(false));
  const move = button("Move to…", () => transfer(true));
  const actions = el(
    "div",
    { class: "wg-actions" },
    copy,
    move,
    remove,
    // The shell's, because the banner on the glance view offers the same thing and the two
    // must report it alike. It resolves once the collection has run, not when it is queued.
    button("Reclaim space", () => run(reclaim)),
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

  /* Copy or move what is selected into another project.
   *
   * The project is chosen from a list of the ones that exist, so the question cannot be
   * answered with a name that is not there. A move asks nothing further: the packages are
   * still in the cache, one choice away from coming back. */
  async function transfer(moving) {
    // A git mirror is a directory, not a digest, and cannot be carried like a file.
    const digests = [...selected].filter(Boolean);
    if (!digests.length) {
      notice("Nothing is selected.");
      return;
    }
    const here = store.state.project;
    const others = store.state.projects.map((project) => project.name).filter((name) => name !== here);
    if (!others.length) {
      notice("There is no other project to put them in. Make one with + at the top.", true);
      return;
    }
    const counted = `${digests.length} package${digests.length === 1 ? "" : "s"}`;
    const to = await askChoice({
      title: `${moving ? "Move" : "Copy"} ${counted}`,
      label: moving
        ? `Into which project? They leave ${here}.`
        : `Into which project? They stay in ${here} as well.`,
      choices: others,
      confirmLabel: moving ? "Move" : "Copy",
    });
    if (to === null) return;
    await run(async () => {
      const result = await api.transferArtifacts(here, to, digests, moving);
      selected.clear();
      return transferred(result, to, moving);
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
    } finally {
      // After the button's own finally, which re-enables whatever was clicked: the
      // selection those buttons act on is usually empty by now, and a Copy to… that looks
      // ready with nothing selected is a button that answers "Nothing is selected".
      setTimeout(syncRemove, 0);
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
    copy.disabled = selected.size === 0;
    move.disabled = selected.size === 0;
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

/** What a transfer did, in the order somebody needs to hear it: what arrived, then what did
 *  not and why. */
function transferred(result, to, moving) {
  const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;
  const said = [`${moving ? "Moved" : "Copied"} ${plural(result.packages, "package")} to ${to}.`];
  if (result.companions) {
    // Named for what they are to the reader: the things that let the other project answer
    // for these packages without going back to the network.
    const them = result.packages === 1 ? "it" : "them";
    said.push(`What ${them} ${result.packages === 1 ? "is" : "are"} found through went too, so ${to} can serve ${them} offline.`);
  }
  if (result.conflicts) {
    said.push(`${plural(result.conflicts, "file")} left alone: ${to} already holds a different file under the same name.`);
  }
  if (result.pinned) {
    said.push(`${plural(result.pinned, "file")} also kept here: a checkpoint holds them.`);
  }
  if (result.missing) {
    said.push(`${plural(result.missing, "package")} had already gone from this project.`);
  }
  return said.join(" ");
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
    // Not window.prompt: WKWebView answers null without asking, so on a Mac this button
    // did nothing at all. See dialog.js.
    const message = await askText({
      title: "Checkpoint now",
      label: "What is this checkpoint for?",
      placeholder: "before a trip",
      confirmLabel: "Checkpoint",
      validate: (answer) =>
        answer ? "" : "A checkpoint nobody labelled is one nobody can choose later.",
    });
    if (message === null) return;
    await guard(async () => {
      await settle(await api.checkpoint(store.state.project, message));
      return "Checkpoint recorded.";
    });
  }

  async function exportPack() {
    const project = store.state.project;
    // Read fresh rather than trusted from the last refresh: which checkpoint is current
    // decides what a pack can start from, and a terminal may have moved it since.
    let answer;
    let last;
    try {
      [answer, last] = await Promise.all([api.snapshots(project), lastPacked(project)]);
    } catch (cause) {
      notice(cause?.message || String(cause), true);
      return;
    }
    const plan = await askWhatToPack(answer, last);
    if (plan === null) return;
    // Where it goes is asked before anything is built, because a pack is gigabytes and
    // the question is cheap. Declining the picker cancels; a browser with no picker gets
    // the cache's own outbox.
    let dir = null;
    if (await canBrowse()) {
      dir = await pickDirectory();
      if (dir === null) return;
    }
    await guard(async () => {
      const job = await settle(await api.exportPack(project, {
        base: plan.base,
        // A checkpoint is taken only when asked for. Every export used to take one, which
        // left a row behind each time identical in content to the one before it; a
        // history made of exports is not one anybody can choose from. Otherwise the head
        // is pinned rather than left to the daemon to read, so the pack is the checkpoint
        // this window named — and the job record says which, for the next export to
        // start from.
        ...(plan.first ? { checkpoint: true } : { target: plan.target }),
        ...(dir ? { dir } : {}),
      }));
      const log = job.log ?? "";
      const wrote = /wrote (\S+)/.exec(log);
      const packed = /packed checkpoint (\S+)/.exec(log)?.[1] || plan.target;
      const holds = packed ? `checkpoint ${short(packed)}` : "a new checkpoint";
      // What it holds, and where it will be taken: the second is the half the person
      // carrying it needs to know before they set off, not when the import is refused.
      const what = plan.base
        ? `${holds}, since ${short(plan.base)}. It imports where the project is on ${short(plan.base)}.`
        : `${holds}, everything. It imports into a project with no checkpoints.`;
      return wrote
        ? ["The pack is at ", el("code", { text: wrote[1] }), ` — ${what}`]
        : `Pack written — ${what}`;
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
    const project = store.state.project;
    await guard(async () => {
      // This project's history, for telling a pack that was already taken from one that
      // starts somewhere this project has never been. Only for the explanation, so not
      // having it is no reason to stop: an import creates a project that is not here yet.
      const before = new Set(((await api.snapshots(project).catch(() => ({}))).snapshots ?? [])
        .map((row) => row.id));
      let job;
      try {
        job = await settle(await api.importPack(project, file ? { file } : {}));
      } catch (cause) {
        throw explainImport(cause, project, before);
      }
      const applied = /imported checkpoint (\S+)/.exec(job.log ?? "");
      return applied
        ? `${project} is now at checkpoint ${short(applied[1])}.`
        : "The pack was applied.";
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
        text: "A pack carries a checkpoint, not what has been cached since it — Export offers to take one first. The other machine takes a pack only if it starts where that machine's project is: everything, into a project with no checkpoints; or since the checkpoint it is on, marked * in its window. Anything else is refused before a byte is written, so an import cannot lose what is here.",
      }),
    ),
    refresh,
  };
}

/** The checkpoint this project's last pack carried, or "" when none is known.
 *
 * Read from the daemon's job history rather than remembered by this window: the last pack
 * may have come from the console, the terminal, or a window since closed. It is the best
 * guess at where the receiving machine is, because that is where it would be had it taken
 * the pack — which only that machine can confirm, so it is offered, never assumed. */
async function lastPacked(project) {
  try {
    // Newest first, so the first match is the latest.
    const job = ((await api.jobs()).jobs ?? []).find((row) =>
      row.action === "export" && row.status === "done" && row.project === project);
    if (!job) return "";
    if (job.params?.target) return job.params.target;
    // An export that read the head, or checkpointed first, names what it packed only in
    // its log — which the list leaves out. A generated file name ends in the same id.
    const log = (await api.job(job.id)).log ?? "";
    return /packed checkpoint (\S+)/.exec(log)?.[1] ?? /-([0-9a-f]{12})\.tar\b/.exec(log)?.[1] ?? "";
  } catch {
    // A guess, so failing to make one is no reason to stop an export.
    return "";
  }
}

/** Ask what a pack should carry. Resolves to { base, first, target }, or null if cancelled.
 *
 * "What goes in it" is really "which checkpoint is the other machine on", because a pack
 * is taken only where it starts from the receiver's checkpoint. Only that machine knows,
 * so the default is the checkpoint the last pack ended at — where it is if it took that
 * pack — with everything else this project has been through one choice away.
 *
 * Only the head and its ancestors are offered. A pack from anywhere else is refused by the
 * export itself, after the person has been told it was coming. */
function askWhatToPack(answer, last) {
  const head = answer.head ?? "";
  const byID = new Map((answer.snapshots ?? []).map((row) => [row.id, row]));
  const lineage = [];
  for (let id = head; id && byID.has(id) && lineage.length <= byID.size; id = byID.get(id).parent) {
    lineage.push(id);
  }
  const ended = last ? lineage.find((id) => id.startsWith(last)) ?? "" : "";

  return new Promise((resolve) => {
    // Checked where leaving it off would leave nothing to send: no checkpoint at all, or
    // none since the last pack. Otherwise off, as in the console — an export is a read.
    const first = el("input", { type: "checkbox", checked: !head || ended === head, disabled: !head });
    const choice = el("select", { class: "dlg-input", "aria-label": "Start from" });
    const hint = el("p", { class: "dlg-line wg-move-note" });

    function draw() {
      // What the reader chose, kept across a toggle of the box below where it is still on
      // offer. Nothing on the first draw, which takes the default: the first option.
      const kept = choice.options.length ? choice.value : null;
      // With a new checkpoint the head itself becomes a starting point; without one it
      // is what the pack ends at, and a pack from its own end carries nothing.
      const starts = first.checked ? lineage : lineage.slice(1);
      const options = [];
      if (ended && starts.includes(ended)) {
        options.push([ended, `what is new since ${short(ended)}, where the last pack ended`]);
      }
      options.push(["", "everything — for a project with no checkpoints"]);
      for (const id of starts) {
        if (id === ended) continue;
        const subject = byID.get(id)?.subject;
        options.push([id, `since ${short(id)}${subject ? ` — ${subject}` : ""}`]);
      }
      choice.replaceChildren(...options.map(([value, label]) => el("option", { value, text: label })));
      choice.value = options.some(([value]) => value === kept) ? kept : options[0][0];
      choice.disabled = options.length === 1;
      hint.textContent = ended === head && !first.checked
        ? `The last pack already carried ${short(head)}, so without a new checkpoint it can only be sent again from an earlier starting point.`
        : "";
    }
    draw();
    first.addEventListener("change", draw);

    let close = () => {};
    const done = (value) => {
      close();
      resolve(value);
    };
    const go = () => done({ base: choice.value, first: first.checked, target: first.checked ? "" : head });
    const form = el("form", { class: "dlg-body" },
      el("p", {
        class: "dlg-line",
        text: "The other machine takes a pack only if it starts where its project is: on the checkpoint marked * in its window, or from nothing if it has none.",
      }),
      el("label", { class: "dlg-label", text: "Start from" }),
      choice,
      el("label", { class: "wg-check" }, first,
        el("span", {
          text: head
            ? `Checkpoint first, to include what has been cached since ${short(head)}`
            : "This project has no checkpoint yet, so one is taken first",
        })),
      hint);
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      go();
    });
    close = openModal({
      title: "Export a pack",
      wide: true,
      body: form,
      onCancel: () => resolve(null),
      actions: [button("Cancel", () => done(null)), button("Export", go, { kind: "primary" })],
    });
    choice.focus();
  });
}

/** Say why an import was refused, in terms of what to do about it.
 *
 * The engine's sentence — non-fast-forward import: local HEAD is "…", pack requires base
 * "…" — is precise and names no way forward. `pkgcache import` rewrites it (explainImport
 * in cmd/pkgcache/shuttle.go); this is the same rewrite for the window, where the way
 * forward is the other machine's Export dialog rather than a flag. It does not suggest
 * importing into a second project: a pack names its project, and any other refuses it. */
function explainImport(cause, project, history) {
  const text = cause?.message || String(cause);
  const refused = /local HEAD is "([^"]*)", pack requires base "([^"]*)"/.exec(text);
  if (refused) {
    const [, here, needs] = refused;
    const why = !needs
      ? "It carries everything from the start, which only a project with no checkpoints takes."
      : history.has(needs)
        ? `It starts from ${short(needs)}, which ${project} has been on but is not now — most often because this pack was imported already.`
        : `It starts from ${short(needs)}, a checkpoint ${project} has never been on.`;
    const ask = here
      ? `Ask whoever made it for a pack since ${short(here)}: in their window, Export a pack and start from ${short(here)}.`
      : "Ask whoever made it for a pack of everything.";
    return new Error(
      `This pack does not continue from where ${project} is (${here ? `checkpoint ${short(here)}` : "no checkpoint"}), ` +
        `so nothing was changed. ${why} ${ask}`);
  }
  const elsewhere = /pack belongs to project (\S+), not (\S+)/.exec(text);
  if (elsewhere) {
    const [, theirs, ours] = elsewhere;
    return new Error(
      `This pack is for ${theirs}, not ${ours}, so nothing was changed. Switch to ${theirs} ` +
        "at the top — make it with + if this cache has none — and import it there.");
  }
  return cause;
}

/** The prefix people exchange: what `pkgcache snapshots` prints and what a row shows. */
function short(id) {
  return String(id).slice(0, 12);
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
    const project = store.state.project;
    // Every project's siblings, because a sibling is the first stop for a miss and a
    // summary without them called a project that borrows from the laptop beside it
    // "straight to the registries". One small local request per project.
    const borrowed = new Map(await Promise.all(states.map(async (state) => [
      state.project,
      await api.peers(state.project).then((body) => body?.peers ?? []).catch(() => []),
    ])));
    if (store.state.project !== project) return;
    const mine = states.find((state) => state.project === project);
    current.set(sourceRows(states, project, borrowed));
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
          confirmLabel: "Next",
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
          placeholder: "global", value: "global", confirmLabel: "Next",
        });
        if (typed === null) return;
        theirProject = typed;
      }
    } catch (cause) {
      notice(cause?.message || String(cause), true);
      return;
    }

    // What the console's form asks for too. The digest half authenticates with it, and
    // left empty that machine is asked for one — which works where it has no accounts
    // and is refused where it has. Without this the window could not borrow from a cache
    // that asks for a token at all.
    const token = await askText({
      title: "Its token",
      label: "Only if that cache asks for one. Left empty, this cache asks it for one.",
      type: "password",
      confirmLabel: "Borrow from it",
      validate: () => "",
    });
    if (token === null) return;

    try {
      const added = await api.addPeer(project, {
        address, their_project: theirProject, ...(token ? { token } : {}),
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
      body: `Stop borrowing from ${peer.name}?\n` +
        afterForgettingPeer(peer, await sourcesNow(project), project),
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
    // Write-only, as in the console: the daemon never sends a stored token back, so there
    // is nothing to prefill. Empty keeps the stored one while the cache and the project on
    // it are unchanged; the box beneath is how to drop it.
    const hasToken = Boolean(state?.token_configured) && !state.inherited;
    const token = el("input", {
      name: "token",
      type: "password",
      placeholder: hasToken ? "stored — leave empty to keep it" : "only if their project requires one",
      autocomplete: "new-password",
      spellcheck: "false",
    });
    const clearToken = el("input", { type: "checkbox" });

    const form = el(
      "form",
      { class: "wg-form" },
      field("Team cache", server),
      field("CA fingerprint", fingerprint, "from your colleague, not from this network"),
      field("Project on their side", teamProject, "empty means their global project"),
      field("Read token", token, "for a project that serves downloads only to a token"),
      hasToken
        ? el("label", { class: "wg-check" }, clearToken,
            el("span", { text: "stop sending the stored token" }))
        : null,
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
          ? button("Forget", () => forget(state), { kind: "danger" })
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
        ...(token.value.trim() ? { token: token.value.trim() } : {}),
        ...(clearToken.checked ? { clear_token: true } : {}),
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
      notice(`${store.state.project} now goes through ${state.server} → ${state.team_project || "global"}.`);
    } catch (cause) {
      notice(cause?.message || String(cause), true);
    }
  }

  async function forget(state) {
    const project = store.state.project;
    // Asked, as the console asks, and in the same words: where misses go next depends on
    // the global project's team cache and on every sibling, so it is read now.
    if (!await askConfirm({
      title: "Forget the team cache",
      body: `Stop sending ${project} through ${state.server}?\n` +
        afterForgettingTeam(project, await sourcesNow(project)),
      confirmLabel: "Forget", danger: true,
    })) return;
    try {
      await api.deleteSource(project);
      await reload();
      await refresh();
      notice(`${project} no longer uses a team cache.`);
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

/* One row per project: whose cache and whose project on it, then the order a miss walks.
 *
 * `server → project` leads, as it does in the console, because the project on the far side
 * is the field whose being wrong looks exactly like its being right: pointed at a team
 * project that does not exist, everything is fetched from upstream while the row reads as
 * configured. */
function sourceRows(states, project, borrowed) {
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
            ? `${host(state.server)} → ${state.team_project || "global"}` +
              (state.inherited ? " ↑ inherited" : "") +
              (state.token_configured ? " · token configured" : "")
            : "no team cache",
          title: state.server || null,
        }),
        el("span", {
          class: "wg-row-sub",
          text: missRoute(state, borrowed.get(state.project) ?? []),
        }),
      ),
      // Only when it was asked: "not probed" is not "down".
      state.server && state.reachable !== undefined
        ? el("span", {
            class: "wg-row-sub " + (state.reachable ? "is-up" : "is-down"),
            text: state.reachable ? "up" : "down",
          })
        : null,
    ),
  );
}

// The order a miss walks: siblings first (they sit ahead of the team cache in the chain),
// then the team cache, then the public registries — behind a team cache only where it
// falls back to them.
function missRoute(state, peers) {
  const steps = peers.map((peer) => peer.name);
  if (state.server) steps.push("team cache");
  if (!state.server || state.direct) steps.push("public registries");
  return `misses → ${steps.join(" → ")}` +
    (state.server && !state.direct ? ", never the public registries" : "");
}

function host(server) {
  return String(server).replace(/^https?:\/\//, "");
}

/* Where a project's misses go once a team cache or a machine is forgotten, in the console's
 * words (views/sources.js), so the two windows give one answer.
 *
 * Read from the daemon when the question is put rather than from what this panel last
 * drew: the answer depends on the global project's team cache and on every sibling, and
 * either may have changed from another window or a terminal since. A failure to read
 * answers with nothing rather than blocking the Forget it was only describing. */
async function sourcesNow(project) {
  const [sources, peers] = await Promise.all([
    api.sources().then((body) => body?.sources ?? []).catch(() => []),
    api.peers(project).then((body) => body?.peers ?? []).catch(() => []),
  ]);
  return { sources, peers };
}

// "the machine it borrows from (sams-laptop), then " — or nothing, when there is none.
function throughMachines(names) {
  if (!names.length) return "";
  const which = names.length === 1 ? "the machine" : "the machines";
  return `${which} it borrows from (${names.join(", ")}), then `;
}

function afterForgettingTeam(project, { sources, peers }) {
  const global = sources.find((row) => row.project === "global" && row.server);
  const first = throughMachines(peers.map((peer) => peer.name));
  const lines = [
    // A project with no team cache of its own follows global's, so for every other
    // project forgetting its own is switching to that one, not to the internet.
    project !== "global" && global
      ? `Its misses then go to ${first}the global project's team cache, ` +
        `${global.server} → ${global.team_project || "global"}.`
      : `Its misses then go to ${first}the public registries, or to upstreams of its own ` +
        "where it has added any.",
  ];
  if (project === "global") {
    const followers = sources
      .filter((row) => row.project !== "global" && row.server && row.inherited)
      .map((row) => row.project);
    if (followers.length) lines.push(`Projects that follow it lose it too: ${followers.join(", ")}.`);
  }
  lines.push("If this project is offline, misses fail either way. Nothing already cached is lost.");
  return lines.join("\n");
}

function afterForgettingPeer(peer, { sources, peers }, project) {
  const others = peers.filter((other) => other.name !== peer.name).map((other) => other.name);
  const team = sources.find((row) => row.project === project && row.server);
  const steps = [];
  if (others.length) {
    steps.push(`${others.length === 1 ? "the other machine" : "the other machines"} ` +
      `it borrows from (${others.join(", ")})`);
  }
  if (team) steps.push(`the team cache ${team.server}${team.inherited ? ", followed from global" : ""}`);
  if (!team || team.direct) steps.push("the public registries");
  return `Misses go to ${steps.join(", then ")} instead. Nothing already cached here is lost.`;
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
