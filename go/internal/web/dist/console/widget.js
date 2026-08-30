/* The widget: one window, four questions, no navigation.
 *
 * It is the console's own modules — api.js, store.js, dom.js, format.js — in a shell
 * built for a 420px window somebody leaves open beside their editor. Not a route inside
 * the console: the console's chrome is a nav rail, a project switcher, a shader and a
 * page title, and a widget that hid all of it would still be paying for it. This is a
 * second shell over the same state, which is what the region model is for.
 *
 * The four questions, in the order they are asked:
 *
 *   1. Is the cache working?          the state line, and the banner when it is not
 *   2. How much room is left?         the meter, against the limit somebody chose
 *   3. Is it actually helping?        the share served from here
 *   4. What is it doing right now?    live transfers, straight off the event bus
 *
 * Everything else a person might want is in the console, one click away in the footer.
 */

import { api } from "./api.js";
import * as store from "./store.js";
import { el, region, button, fill } from "./dom.js";
import { packagesPanel, transferPanel, sourcesPanel } from "./widget-panels.js";
import { bytes, percent, outcomeColor, OUTCOME_LABEL } from "./format.js";

const root = document.getElementById("root");

/** The wordmark, in this program's name.
 *
 * Not chrome.js's: that one reads [pkgreg], and this window is a cache on somebody's own
 * machine. The same bracket-and-accent construction, because they are the same product. */
function wordmark() {
  return el(
    "div",
    { class: "wordmark" },
    el("span", { class: "br", text: "[" }),
    el("span", { class: "a", text: "pkg" }),
    el("span", { class: "f", text: "cache" }),
    el("span", { class: "br", text: "]" }),
  );
}

/* ---- shell ---------------------------------------------------------------- */

/** Re-arm Wails' WML scan.
 *
 * WML binds its listeners once, when the document becomes ready, by querying the DOM for
 * its attributes — and this window builds its DOM in JS after that moment, so the console
 * link does not exist yet to be found and would never be bound. Rescanning after a render
 * is what makes the attribute do anything at all here.
 *
 * A no-op in a browser, where there is no wails global and the plain link already works. */
function rebindWML() {
  globalThis.wails?.WML?.Reload?.();
}

/** The console's address, absolute.
 *
 * The widget and the console are two paths on one origin — the daemon serves both — so
 * this is the same URL the app's own "Open the console" resolves to, arrived at from the
 * page rather than from Go. */
function consoleURL() {
  return new URL("/console", location.href).href;
}

function shell() {
  const projectRegion = region("div", { class: "wg-project" });
  const noticeRegion = region("div", {});
  const stateRegion = region("div", { class: "wg-section" });
  const diskRegion = region("div", { class: "wg-section" });
  const figuresRegion = region("div", { class: "wg-section" });
  const liveRegion = region("div", { class: "wg-section" });
  const footRegion = region("div", { class: "wg-foot" });
  const tabsRegion = region("nav", { class: "wg-seg", "aria-label": "View" });
  const panelRegion = region("div", { class: "wg-panel-host" });

  const live = el(
    "span",
    { class: "live" },
    el("span", { class: "dot", id: "wg-dot" }),
  );

  fill(
    root,
    el(
      "header",
      { class: "wg-head" },
      wordmark(),
      el("span", { class: "wg-head-spacer" }),
      projectRegion.node,
      // data-wml-openurl, and not only target=_blank, because this page has two very
      // different readers. In a browser the attribute means nothing and the link opens a
      // tab, as it always has. Inside the desktop app the link alone does nothing at all:
      // a _blank link asks WebKit to create a second web view, Wails' Linux backend
      // connects no handler for that request — load-changed, permission-request and
      // script-message-received are the only signals it takes — and so the click is
      // swallowed in silence. The attribute is Wails' own declarative hook for the case,
      // and it routes to the same system browser the tray's "Open the console" reaches.
      //
      // Absolute, because OpenURL is handed the string as written and a relative path
      // would not survive the trip out to another process.
      el("a", {
        class: "wg-head-link",
        href: "/console",
        target: "_blank",
        rel: "noopener",
        "data-wml-openurl": consoleURL(),
        text: "console",
        title: "Open the full console",
      }),
      live,
    ),
    tabsRegion.node,
    el(
      "main",
      { class: "wg-main" },
      noticeRegion.node,
      stateRegion.node,
      diskRegion.node,
      figuresRegion.node,
      liveRegion.node,
      panelRegion.node,
    ),
    footRegion.node,
  );
  root.classList.remove("booting");
  root.classList.add("wg-shell");
  rebindWML();
  return {
    projectRegion, noticeRegion, stateRegion, diskRegion, figuresRegion, liveRegion,
    footRegion, tabsRegion, panelRegion, live,
  };
}

/* ---- tabs ------------------------------------------------------------------
 *
 * Four views in one 420px column, and only one is built at a time. The glance view is
 * first and is what the window opens on: somebody who wanted to operate the cache came
 * looking, and somebody who wanted to watch it did not.
 *
 * The panels are constructed lazily and kept, so switching back does not refetch what is
 * already on screen — but each one refreshes on entry, because a window left open for an
 * afternoon showing an afternoon-old package list would be worse than a blank one. */
const TABS = [
  ["now", "now"],
  ["packages", "packages"],
  ["transfer", "transfer"],
  ["sources", "sources"],
];

let tab = "now";
const panels = new Map();

function panelFor(name) {
  if (panels.has(name)) return panels.get(name);
  const context = { notice, reload, settle };
  const built =
    name === "packages"
      ? packagesPanel(context)
      : name === "transfer"
        ? transferPanel(context)
        : sourcesPanel(context);
  panels.set(name, built);
  return built;
}

function renderTabs(regions) {
  regions.tabsRegion.set(
    TABS.map(([name, label]) =>
      el("button", {
        class: "wg-seg-item" + (tab === name ? " on" : ""),
        text: label,
        "aria-current": tab === name ? "page" : null,
        onclick: () => selectTab(regions, name),
      }),
    ),
  );
}

function selectTab(regions, name) {
  tab = name;
  renderTabs(regions);
  renderPanel(regions);
}

/** Show the current tab, hiding what belongs to the others. */
function renderPanel(regions) {
  const glance = tab === "now";
  for (const node of [regions.stateRegion.node, regions.diskRegion.node,
    regions.figuresRegion.node, regions.liveRegion.node]) {
    node.hidden = !glance;
  }
  regions.panelRegion.node.hidden = glance;
  if (glance) {
    regions.panelRegion.set();
    return;
  }
  const panel = panelFor(tab);
  regions.panelRegion.set(panel.node);
  // Failures land in the notice line rather than throwing into an event handler, where
  // they would reach nothing but the browser console nobody has open.
  void Promise.resolve(panel.refresh()).catch((cause) =>
    notice(cause?.message || String(cause), true),
  );
}

/* ---- the four questions ---------------------------------------------------
 *
 * Everything below updates nodes in place. It used to rebuild each section per frame
 * through region.set, which is a replaceChildren, and that had two costs on a machine
 * watching a real download:
 *
 *   - the window flashed. A recreated element replays its CSS entrance animation, so every
 *     live row re-faded on every progress flush — up to sixty times a second.
 *   - it was slow for no reason. A progress frame changes one row's width and one label;
 *     it was destroying and rebuilding the rows, the meter, the figures and the whole
 *     recent feed alongside.
 *
 * The console learned this the same way and says so in store.js. The store's coalescing to
 * one flush per animation frame bounds how often this runs; keeping the DOM bounds what it
 * costs each time.
 */

// The parts that are written to rather than replaced. Built once by buildSections.
let parts = null;

/** Build the glance view's nodes once. Called from the shell, before any state arrives. */
function buildSections(regions) {
	const stateLine = el("span", {});
	const stateNote = el("span", { class: "wg-state-note" });
	const stateRow = el("div", { class: "wg-state" }, stateLine, stateNote);
	const banner = region("div", {});
	regions.stateRegion.set(stateRow, banner.node);

	const diskLabel = el("div", { class: "wg-label", text: "Disk" });
	const meterFill = el("span", { class: "meter-fill" });
	const meter = el(
		"div",
		{
			class: "meter wg-meter", role: "meter", "aria-valuemin": "0",
			"aria-label": "Cache size against its limit",
		},
		meterFill,
	);
	const used = el("span", {});
	const ceilingText = el("span", { class: "wg-of" });
	regions.diskRegion.set(
		diskLabel, meter, el("div", { class: "wg-numbers" }, used, ceilingText));

	const served = el("span", { class: "wg-figure-value", text: "—" });
	const held = el("span", { class: "wg-figure-value", text: "0" });
	regions.figuresRegion.set(
		el(
			"div",
			{ class: "wg-pair" },
			el("div", { class: "wg-figure" }, served,
				el("span", { class: "wg-figure-label", text: "served from here" })),
			el("div", { class: "wg-figure" }, held,
				el("span", { class: "wg-figure-label", text: "objects held" })),
		),
	);

	const liveLabel = el("div", { class: "wg-label", text: "Just now" });
	const liveList = el("div", { class: "wg-live" });
	const recentList = el("div", { class: "wg-recent" });
	const quiet = el("div", { class: "wg-quiet", text: "Nothing has been asked for yet." });
	regions.liveRegion.set(liveLabel, liveList, recentList, quiet);

	parts = {
		stateLine, stateNote, stateRow, banner,
		diskLabel, meter, meterFill, used, ceilingText,
		served, held,
		liveLabel, liveList, recentList, quiet,
		// Keyed by the event id, so a row that is already on screen is updated rather than
		// recreated — which is what stops the animation replaying.
		rows: new Map(),
	};
}

/* ---- the four questions --------------------------------------------------- */

/** The state line, and the banner when the cache has stopped storing.
 *
 * "Caching" is not a fact about the process being up — it is up, or this window would
 * be showing a disconnected dot. It is a fact about whether anything is being kept,
 * which is the thing that silently stops being true. */
function renderState(regions) {
	const budget = store.state.storage?.budget ?? null;
	const project = currentProject();
	const offline = project?.offline === true;

	let text = "Caching";
	let note = "and serving";
	let modifier = "";
	if (budget?.full) {
		text = "Not caching";
		note = "still serving what it has";
		modifier = " is-full";
	} else if (offline) {
		text = "Offline";
		note = "serving only what it already holds";
		modifier = " is-offline";
	}
	parts.stateLine.textContent = text;
	parts.stateNote.textContent = note;
	parts.stateRow.className = "wg-state" + modifier;

	// The banner is the one part worth replacing, and it changes when the cache fills up
	// rather than when a byte arrives.
	const reason = budget?.full ? budget.reason || "" : "";
	if (parts.bannerReason === reason) {
		return;
	}
	parts.bannerReason = reason;
	parts.banner.set(
		budget?.full
			? el(
					"div",
					{ class: "wg-banner", role: "status" },
					el("div", { class: "wg-banner-title", text: "The cache is full" }),
					el("div", {
						class: "wg-banner-reason",
						// The daemon's own sentence. It already names the numbers and the two
						// ways out, and rewriting it here would mean two places to keep true.
						text: reason || "It is serving what it has and storing nothing new.",
					}),
					el(
						"div",
						{ class: "wg-banner-actions" },
						button("Reclaim space", () => run(() => api.gc(false), "Reclaiming space…"), {
							kind: "danger",
						}),
					),
				)
			: null,
	);
}

/** Disk: what is held against what was allowed, with the free-disk floor marked.
 *
 * A cache with no limit still has a floor, so the meter falls back to the filesystem —
 * "no limit" is a choice about this cache, not a promise about the disk. */
function renderDisk(regions) {
	const storage = store.state.storage;
	if (!storage) {
		return;
	}
	const budget = storage.budget ?? null;
	const used = budget ? budget.used_bytes : storage.blob_bytes;
	const limit = budget && budget.limit_bytes > 0 ? budget.limit_bytes : 0;
	const ceiling = limit || storage.fs_total || 0;
	const share = ceiling > 0 ? Math.min(1, used / ceiling) : 0;

	parts.diskLabel.textContent = limit ? "Of the limit you set" : "On this disk";
	// Setting the width on the element that is already there is also what lets its CSS
	// transition run. Replacing the element meant every value change jumped.
	parts.meterFill.style.setProperty("width", `${(share * 100).toFixed(1)}%`);
	parts.meter.className = "meter wg-meter" + (budget?.full ? " status-critical" : "");
	parts.meter.setAttribute("aria-valuemax", String(ceiling));
	parts.meter.setAttribute("aria-valuenow", String(used));
	parts.used.textContent = bytes(used);
	parts.ceilingText.textContent = limit ? `of ${bytes(limit)}` : `${bytes(storage.fs_free)} free`;
}

/** Two figures: is this helping, and how much is in it.
 *
 * The share served from here is the one number that answers "why do I run this". It is
 * deliberately not called a hit rate: dedup and a peer fetch are both "did not leave
 * this machine", and folding them into one word is what the catalog already does. */
function renderFigures(regions) {
	const stats = store.state.stats;
	const rows = stats?.by_eco ?? [];
	let hits = 0;
	let misses = 0;
	for (const row of rows) {
		hits += row.hit_count ?? 0;
		misses += row.miss_count ?? 0;
	}
	const asked = hits + misses;
	// Nothing asked for yet is not a rate of zero, and printing 0% would be a claim about a
	// cache that has not been used.
	parts.served.textContent = asked ? percent(hits, asked) : "—";
	parts.held.textContent = String(stats?.total_blobs ?? 0);
}

/** What is moving right now, straight off the event bus.
 *
 * Keyed: a transfer already on screen is written to, and only a new one creates an element —
 * which is also the only time its entrance animation should play. Replacing this section per
 * frame is what made the window flash, because a recreated element replays that animation on
 * every one of ~60 flushes a second. */
function renderLive() {
	const events = [...store.state.live.entries()].reverse().slice(0, 5);
	const seen = new Set();
	for (const [id, event] of events) {
		seen.add(id);
		let row = parts.rows.get(id);
		if (!row) {
			row = liveRow(event);
			parts.rows.set(id, row);
			// Newest at the top, and rows already there never move. A list that reorders
			// while somebody is reading it is harder to follow than one that only grows.
			parts.liveList.insertBefore(row.node, parts.liveList.firstChild);
		}
		row.update(event);
	}
	for (const [id, row] of parts.rows) {
		if (!seen.has(id)) {
			row.node.remove();
			parts.rows.delete(id);
		}
	}
	const label = events.length ? `Downloading (${events.length})` : "Just now";
	if (parts.liveLabel.textContent !== label) {
		parts.liveLabel.textContent = label;
	}
	parts.quiet.hidden = events.length > 0 || store.state.recent.length > 0;
}

/** The feed of what just finished.
 *
 * Its own subscriber, because it changes when a request completes and not when bytes move.
 * Coupled to the live rows, every progress frame rebuilt nine rows that had not changed. */
function renderRecent() {
	fill(parts.recentList, store.state.recent.slice(0, 6).map(recentRow));
	parts.quiet.hidden = store.state.recent.length > 0 || parts.rows.size > 0;
}

/* One finished request. The dot carries the outcome — the console's own colours, so hit,
   dedup, peer, miss and fail mean the same thing in both windows — and the word beside it
   is what tells two rows for the same package apart: an afternoon has both a fetch and a
   hit for the same tarball, and without the word that reads as a rendering bug. */
function recentRow(event) {
	const outcome = event.kind === "cache.hit" ? "hit" : event.outcome || outcomeOf(event.kind);
	return el(
		"div",
		{ class: "wg-recent-row", title: `${event.eco ?? ""} ${event.id ?? ""} — ${outcome}` },
		el("span", { class: "wg-recent-mark", style: `background:${outcomeColor(outcome)}` }),
		el("span", { class: "wg-recent-name", text: shortName(event.id ?? "") }),
		el("span", { class: "wg-recent-outcome", text: OUTCOME_LABEL[outcome] ?? outcome }),
		el("span", { class: "wg-recent-size", text: event.size ? bytes(event.size) : "" }),
	);
}

function outcomeOf(kind) {
	if (kind === "fetch.error") return "fail";
	if (kind === "fetch.done") return "miss";
	return "hit";
}

function liveRow(event) {
	const eco = el("span", { class: "wg-live-eco" });
	const name = el("span", {});
	const size = el("span", { class: "wg-live-size" });
	const bar = el("span", { class: "wg-live-fill" });
	const node = el(
		"div",
		{ class: "wg-live-row" },
		el("div", { class: "wg-live-name" }, eco, name, size),
		el("div", { class: "wg-live-track" }, bar),
	);
	// What a transfer is does not change while it runs, so it is written once. Only the
	// numbers and the bar move, which is the least a progress row can cost per frame.
	eco.textContent = event.eco ?? "";
	name.textContent = shortName(event.id ?? "");

	let lastSize = "";
	const handle = {
		node,
		update(current) {
			const total = current.total ?? 0;
			const received = current.received ?? 0;
			const known = total > 0;
			const text = known ? `${bytes(received)} / ${bytes(total)}` : bytes(received);
			if (text !== lastSize) {
				lastSize = text;
				size.textContent = text;
			}
			// A transfer with no declared length cannot claim a fraction it does not know,
			// so the bar sweeps instead of filling.
			bar.className = "wg-live-fill" + (known ? "" : " indeterminate");
			if (known) {
				bar.style.setProperty("width", `${(Math.min(1, received / total) * 100).toFixed(1)}%`);
			} else {
				bar.style.removeProperty("width");
			}
		},
	};
	handle.update(event);
	return handle;
}

/** The tail of a key, which is the part that identifies the artifact. A wheel's full
 *  catalog key is longer than this window is wide. */
function shortName(id) {
  const parts = String(id).split("/").filter(Boolean);
  return parts.length ? parts[parts.length - 1] : id;
}

/* ---- controls -------------------------------------------------------------- */

function renderProject(regions) {
  const names = store.state.projects.map((project) => project.name);
  const select = el(
    "select",
    {
      "aria-label": "Project",
      onchange: (event) => {
        store.setProject(event.target.value);
        void reload();
      },
    },
    names.map((name) =>
      el("option", { value: name, selected: name === store.state.project, text: name }),
    ),
  );
  // Beside the switcher rather than inside it. A "New project…" option in the list would
  // put an action among a set of destinations, so choosing it with the keyboard — which
  // is how a select is often used — would fire a dialog on the way past.
  const add = button("+", () => addProject(names), {
    title: "New project on this cache",
  });
  add.classList.add("wg-project-add");
  regions.projectRegion.set(el("div", { class: "wg-project-row" }, [select, add]));
}

/** Create a project on this cache and switch to it.
 *
 * A project here is a separate catalog over shared bytes: two projects needing the same
 * wheel store it once. There is no owner and no member list — this cache has one user.
 *
 * What it is pointed at is not asked for. The daemon gives a new project the team cache
 * the global one uses as it creates it, so it caches through the same remote from its
 * first request; pointing it somewhere of its own is a deliberate second step, in the
 * sources panel. */
async function addProject(existing) {
  const answer = window.prompt("Name for the new project");
  if (answer === null) return;
  const name = answer.trim();
  if (!name) return;
  if (existing.includes(name)) {
    // Already here: switching to it is what was meant, and is what creating it would
    // have failed to do.
    store.setProject(name);
    await reload();
    notice(`Switched to ${name}.`);
    return;
  }
  await run(async () => {
    await api.createProject(name);
    store.setProject(name);
  }, `Created ${name}.`);
}

/** The footer: the two things worth doing from here, and the way out to the console.
 *
 * Offline is a toggle rather than a switch with a label, because it is the one control
 * whose current state is already stated at the top of the window. */
function renderFoot(regions) {
  const project = currentProject();
  const offline = project?.offline === true;
  // One control. Export and import moved into the transfer panel, next to the checkpoints
  // they are made from — two buttons in two places was two places to keep true.
  regions.footRegion.set(
    button(offline ? "Go online" : "Go offline", () =>
      run(
        () => api.patchProject(store.state.project, { offline: !offline }),
        offline ? "Fetching again." : "Serving only what is already here.",
      ),
    ),
  );
}

/** Wait for a job to finish, and fail loudly if it did.
 *
 * Polled rather than watched: job.update frames arrive on the bus, but a widget that
 * only learned the outcome from a stream it might have missed would leave somebody
 * looking at "Writing the pack…" forever. */
async function settle(submitted) {
  const deadline = Date.now() + 10 * 60 * 1000;
  for (;;) {
    const job = await api.job(submitted.id);
    if (job.status === "done") return job;
    if (job.status === "failed" || job.status === "cancelled") {
      throw new Error(job.error || `${job.action} ${job.status}`);
    }
    if (Date.now() > deadline) throw new Error(`${job.action} is still running; see the console`);
    await new Promise((resolve) => setTimeout(resolve, 400));
  }
}

/* ---- plumbing ------------------------------------------------------------- */

let regions = null;
let noticeTimer = 0;

function notice(content, isError = false) {
  clearTimeout(noticeTimer);
  const children = Array.isArray(content) ? content : [content];
  regions.noticeRegion.set(
    el("div", { class: "wg-notice" + (isError ? " is-error" : ""), role: "status" }, ...children),
  );
  // Errors stay until something replaces them; a progress line has served its purpose
  // once the thing it described has happened.
  if (!isError) noticeTimer = setTimeout(() => regions.noticeRegion.set(), 12000);
}

async function run(operation, success) {
  try {
    await operation();
    await reload();
    notice(success);
  } catch (cause) {
    notice(cause?.message || String(cause), true);
  }
}

async function reload() {
  await store.loadInstance();
  await store.loadProject();
}

function currentProject() {
  return store.state.projects.find((project) => project.name === store.state.project) ?? null;
}

function renderConnection() {
  const dot = document.getElementById("wg-dot");
  if (!dot) return;
  dot.className = "dot " + (store.state.connected ? "ok" : "bad");
  dot.title = store.state.connected ? "Live" : "Not receiving events";
}

async function boot() {
  document.documentElement.dataset.theme = store.state.theme;
  try {
    store.set({ me: await api.me() });
  } catch {
    // An instance with accounts is not what this window is for: it is pkgcache's, and
    // pkgcache has none. Say so rather than rendering a login form at 420px.
    fill(
      root,
      el("main", { class: "wg-main" },
        el("div", { class: "wg-state", text: "Sign in required" }),
        el("div", { class: "wg-quiet", text: "This cache asks for an account. Open the console instead." }),
        // Same reason as the header link, and one more: without this the click would
        // navigate this window to the console rather than doing nothing, which is worse.
        // The console is an operator UI with a nav rail and tabs, and this window is
        // 420 points wide.
        el("a", {
          href: "/console",
          target: "_blank",
          rel: "noopener",
          "data-wml-openurl": consoleURL(),
          text: "console",
        }),
      ),
    );
    rebindWML();
    root.classList.remove("booting");
    return;
  }

  regions = shell();
  buildSections(regions);

  store.on(["storage", "projects", "project"], () => {
    renderState(regions);
    renderDisk(regions);
    renderFoot(regions);
  });
  store.on(["stats"], () => renderFigures(regions));
  store.on(["live"], () => renderLive());
  store.on(["recent"], () => renderRecent());
  store.on(["projects", "project"], () => renderProject(regions));
  store.on(["project"], () => {
    // A panel showing another project's packages, checkpoints or sources is worse than a
    // blank one: every row would look like it belonged to the project now selected.
    if (tab !== "now") renderPanel(regions);
  });
  store.on(["connected"], () => renderConnection());

  renderState(regions);
  renderDisk(regions);
  renderFigures(regions);
  renderLive();
  renderRecent();
  renderProject(regions);
  renderFoot(regions);
  renderConnection();
  renderTabs(regions);
  renderPanel(regions);

  const disconnect = store.connectEvents();
  addEventListener("beforeunload", disconnect);

  try {
    await reload();
  } catch (cause) {
    notice(cause?.message || String(cause), true);
  }
}

void boot();
