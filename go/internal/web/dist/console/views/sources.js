/* Sources — "where do misses go, and is that working?"
 *
 * Upstreams, peers and the offline switch share a page because they are one
 * decision: what happens when the cache does not already have the bytes. */

import { el, region, panel, fill, table, button, field, input, select, loading } from "../dom.js";
import { api } from "../api.js";
import { askConfirm } from "../dialog.js";
import * as store from "../store.js";
import * as charts from "../charts.js";
import { bytes, count, percent, duration, ecoColor } from "../format.js";

export default {
  mount(node) {
    const offline = region("div");
    const list = region("div");
    const health = region("div");
    // Its own slot rather than a panel in the grid, because whether this instance has
    // one at all is a question only the server can answer: a pkgreg has no local sources
    // and the endpoint 404s there. An empty "Team cache" panel on a server would be a
    // permanent piece of furniture explaining something that does not apply.
    const team = region("div");
    // Its own slot beside the team cache's, because they are the same question asked of
    // two different kinds of machine: where do misses go, and whose project on the far
    // side do they land in.
    const peers = region("div");

    fill(
      node,
      el("div", { class: "view-head" },
        el("h1", { text: "Sources" }),
        el("p", { class: "note", text: `Where misses in ${store.state.project} are resolved.` })),
      el("div", { class: "panel-grid" },
        panel("Offline", { note: "serve from cache only" }, offline.node),
        panel("Upstream health", { note: "hourly, last 24h — mean and max only", wide: true }, health.node)),
      team.node,
      peers.node,
      panel("Upstreams and peers", { note: "tried in priority order", wide: true }, list.node),
    );

    const draw = () => {
      offline.set(renderOffline());
      list.set(renderUpstreams());
    };
    const drawTeam = () => renderTeam(team);
    const drawPeers = () => renderPeers(peers);
    const unsubscribe = [
      store.on(["upstreams", "projects", "project", "ecosystems"], draw),
      // Redrawn on a project switch like everything else here: the team cache is
      // configured per project, and showing one project's while another is selected is
      // the same class of lie the switcher has caused everywhere else.
      store.on(["project"], drawTeam),
      store.on(["project"], drawPeers),
    ];
    draw();
    void drawTeam();
    void drawPeers();
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
            // Going offline takes effect immediately, and whom it reaches depends on the
            // binary: on a server, every machine using this project; on a local cache, this
            // machine, and whichever machines borrow from it. The question names which.
            // Coming back online restores the default, so only the outbound trip asks.
            if (!isOffline && !await askConfirm({
              title: "Go offline",
              body: store.isLocal()
                ? `Take ${store.state.project} offline in this cache?\n` +
                  "This machine stops reaching upstreams for it — misses fail instead of being " +
                  "fetched. Cached content still serves, and machines that borrow from this " +
                  "cache get only what it already holds.\n" +
                  "You can bring it back online from this page at any time."
                : `Take ${store.state.project} offline?\n` +
                  "Every machine using this project stops reaching upstreams — misses fail " +
                  "instead of being fetched. Cached content still serves.\n" +
                  "You can bring it back online from this page at any time.",
              confirmLabel: "Go offline", danger: true,
            })) return;
            await store.mutate(
              () => api.patchProject(store.state.project, { offline: !isOffline }),
              isOffline ? "Project is online" : "Project is offline");
          },
          { kind: isOffline ? "primary" : "danger" })
      : el("p", { class: "note", text: "Only the project owner or a superuser can change this." }),
  );
}

/* The team cache: which pkgreg this project's misses go through, and which project on
 * the far side they land in.
 *
 * The widget has had this form since local sources existed; the console never did, so the
 * whole operator UI could show you the chain a team cache had written and gave you no way
 * to write one — and no way at all to see, let alone change, which of the team's projects
 * you were pointed at. That last field is the one worth having here: it need not match
 * this project's name, and a laptop pointed at a team project that does not exist fetches
 * everything from upstream while looking configured.
 */
async function renderTeam(slot) {
  const project = store.state.project;
  let states;
  try {
    states = (await api.sources()).sources ?? [];
  } catch {
    // A pkgreg server, or a daemon older than this page. Neither has a team cache to
    // show, and neither is an error worth a panel.
    slot.set();
    return;
  }
  if (store.state.project !== project) return; // switched while we were asking
  const state = states.find((row) => row.project === project);
  slot.set(panel("Team cache", { note: "the pkgreg this project's misses go through" },
    teamBody(project, state, () => void renderTeam(slot))));
}

function teamBody(project, state, again) {
  // A project always has a state; having a source of its own is a different question.
  const configured = Boolean(state?.server);
  if (!store.canOperate()) {
    return el("div", { class: "stack" },
      teamSummary(state),
      el("p", { class: "note", text: "Only the project owner or a superuser can change this." }));
  }

  const server = input("server", {
    placeholder: "https://cache.internal:8443", value: state?.server ?? "",
    autocomplete: "off", spellcheck: "false",
  });
  const fingerprint = input("ca_sha256", {
    // Never prefilled. It is the whole of the trust decision, and a value already in the
    // box invites Update to re-verify against what this page last read rather than
    // against what the person was told out of band.
    placeholder: "the CA fingerprint you were given", autocomplete: "off", spellcheck: "false",
  });
  const teamProject = input("team_project", {
    placeholder: "global", value: state?.team_project ?? "", autocomplete: "off",
  });
  const direct = el("input", { type: "checkbox", checked: state ? state.direct : true });
  // Write-only, like the fingerprint and for a stronger reason: the daemon never sends a
  // stored token back, so there is nothing to prefill it with. Empty keeps the stored one
  // while the cache and project are unchanged; the box below is how to drop it.
  const hasToken = Boolean(state?.token_configured) && !state.inherited;
  const token = input("token", {
    type: "password", autocomplete: "new-password", spellcheck: "false",
    placeholder: hasToken ? "stored — leave empty to keep it" : "only if their project requires one",
  });
  const clearToken = el("input", { type: "checkbox" });

  const form = el(
    "form",
    { class: "form" },
    teamSummary(state),
    field("Team cache", server, "the pkgreg address"),
    field("CA fingerprint", fingerprint, "from your colleague, not from this network"),
    field("Project on their side", teamProject, "empty means their global project"),
    field("Read token", token, "for a project that serves downloads only to a token"),
    hasToken
      ? el("label", { class: "field-check" }, clearToken,
          el("span", { text: "stop sending the stored token" }))
      : null,
    // field() is for a labelled control above its input; a checkbox reads the other way
    // round, so it is built here rather than bent into that shape.
    el("label", { class: "field-check" }, direct,
      el("span", { text: "fall back to the public registry when the team cache is unreachable" })),
    el("div", { class: "field-actions" },
      el("button", { class: "btn primary", type: "submit",
        text: configured ? "Update" : "Use this cache" }),
      // Only where there is something of this project's own to remove. An inherited
      // source belongs to another project, and forgetting it from here would either do
      // nothing or take it from everyone.
      configured && !state.inherited
        ? button("Forget", async () => {
            if (!await askConfirm({
              title: "Forget the team cache",
              body: `Stop sending ${project} through ${state.server}?\n` +
                afterForgettingTeam(project, await sourcesNow(project)),
              confirmLabel: "Forget", danger: true,
            })) return;
            await store.mutate(() => api.deleteSource(project), `${project} no longer uses a team cache`);
            await store.loadProject();
            again();
          }, { kind: "danger" })
        : null,
    ),
  );
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const applied = await store.mutate(
      () => api.putSource(project, {
        server: server.value.trim(),
        ca_sha256: fingerprint.value.trim(),
        team_project: teamProject.value.trim(),
        direct: direct.checked,
        ...(token.value.trim() ? { token: token.value.trim() } : {}),
        ...(clearToken.checked ? { clear_token: true } : {}),
      }),
      `${project} now goes through the team cache`);
    if (applied) {
      // The chain rows the daemon just rewrote are what the panel below draws.
      await store.loadProject();
      again();
    }
  });
  return form;
}

// What this project resolves through today, in the two words that matter: whose cache,
// and whose project on it.
function teamSummary(state) {
  if (!state?.server) {
    return el("p", { class: "note", text: "No team cache: misses go to the upstreams below." });
  }
  const far = state.team_project || "global";
  return el("div", { class: "status-line" },
    el("span", { class: `pill ${state.reachable === false ? "warning" : "good"}`,
      text: state.reachable === false ? "unreachable" : "reachable" }),
    el("span", { class: "note", text: `${state.server} → ${far}` }),
    state.token_configured ? el("span", { class: "note", text: "· token configured" }) : null,
    state.inherited
      ? el("span", { class: "note", text: "(inherited from another project)" })
      : null,
  );
}

/* Where a project's misses go once a team cache or a machine is forgotten.
 *
 * Asked of the daemon when the question is put rather than taken from what this page last
 * drew: the answer depends on the global project's team cache and on every sibling, and
 * either may have changed from another window or a terminal since. A failure to ask
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
    // A project with no team cache of its own follows global's — TeamSet.For — so for
    // every other project, forgetting its own is switching to that one, not to the
    // internet.
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

function afterForgettingPeer(project, peer, { sources, peers }) {
  const others = peers.filter((other) => other.name !== peer.name).map((other) => other.name);
  const team = sources.find((row) => row.project === project && row.server);
  // The order a miss walks: siblings, the team cache, and the public registry only where
  // the team cache falls back to it — or where there is no team cache at all.
  const steps = [];
  if (others.length) {
    steps.push(`${others.length === 1 ? "the other machine" : "the other machines"} ` +
      `it borrows from (${others.join(", ")})`);
  }
  if (team) steps.push(`the team cache ${team.server}${team.inherited ? ", followed from global" : ""}`);
  if (!team || team.direct) steps.push("the public registries");
  return `Misses go to ${steps.join(", then ")} instead. Nothing already cached here is lost.`;
}

/* Other machines' caches.
 *
 * The same question the team-cache panel above asks — where do misses go, and whose
 * project on the far side — of a laptop rather than a server. A sibling is written as
 * several upstream rows, one per ecosystem it fronts plus the digest rows, so this talks
 * to the surface that knows that arithmetic rather than writing rows itself.
 */
async function renderPeers(slot) {
  const project = store.state.project;
  let peers;
  try {
    peers = (await api.peers(project)).peers ?? [];
  } catch {
    // A pkgreg, which does not borrow from laptops, or a daemon older than this page.
    slot.set();
    return;
  }
  if (store.state.project !== project) return; // switched while we were asking
  slot.set(panel("Other machines", { note: "caches on the same desk, asked before the internet" },
    peersBody(project, peers, () => void renderPeers(slot))));
}

function peersBody(project, peers, again) {
  const rows = peers.length
    ? peers.map((peer) => el("div", { class: "stack" },
        el("div", { class: "status-line" },
          el("span", { class: "pill good", text: peer.name }),
          el("span", { class: "note", text: `${peer.url} → ${peer.their_project}` }),
          store.canOperate()
            ? button("Forget", async () => {
                if (!await askConfirm({
                  title: "Forget this machine",
                  body: `Stop borrowing from ${peer.name}?\n` +
                    afterForgettingPeer(project, peer, await sourcesNow(project)),
                  confirmLabel: "Forget", danger: true,
                })) return;
                await store.mutate(() => api.forgetPeer(project, peer.name),
                  `${project} no longer borrows from ${peer.name}`);
                await store.loadProject();
                again();
              }, { kind: "danger" })
            : null),
        // Two lists because they are two different promises, and a reader who is told
        // only the first will be surprised the day they go offline.
        el("p", { class: "note", text: `through  ${peer.through.join(", ") || "nothing"}` }),
        el("p", { class: "note",
          text: peer.offline.length
            ? `offline  ${peer.offline.join(", ")} — answered by digest with this project offline`
            : "offline  nothing: no token, so it cannot be asked by digest" })))
    : [el("p", { class: "note", text: "This project borrows from no other machine." })];

  if (!store.canOperate()) {
    return el("div", { class: "stack" }, ...rows);
  }

  const address = input("address", { placeholder: "sams-laptop or 192.168.1.4:41780", autocomplete: "off" });
  const token = input("token", { placeholder: "only if that cache has accounts", autocomplete: "off" });
  // A text box until that machine has been asked, a menu afterwards. Typing a project
  // name blind is what writes a chain resolving to nothing, and the first sign of it is
  // a build failing later with nothing pointing back here.
  const projectRegion = region("div", {});
  let theirProjects = null;
  function drawTheirProject() {
    projectRegion.set(
      theirProjects && theirProjects.length
        ? field("Project on their side",
            select("their_project", theirProjects, theirProjects[0]),
            "the projects that machine says it has")
        : field("Project on their side",
            input("their_project", { placeholder: "global", autocomplete: "off" }),
            theirProjects
              ? "that machine did not list its projects; name one yourself"
              : "check the address to list what it has, or leave empty for their global"),
    );
  }
  drawTheirProject();

  const check = button("Check", async () => {
    theirProjects = null;
    drawTheirProject();
    const reached = await store.mutate(
      () => api.reach(project, { address: address.value.trim() }),
      "");
    if (!reached) return;
    theirProjects = reached.projects || [];
    drawTheirProject();
    store.notify(theirProjects.length
      ? `${reached.url} has ${theirProjects.length} project(s)`
      : reached.reason || `${reached.url} listed no projects`);
  });

  const form = el("form", { class: "form" },
    field("Their address", address, "a cache listens on loopback unless it was told otherwise"),
    el("div", { class: "field-actions" }, check),
    projectRegion.node,
    field("Peer token", token, "leave empty and that cache is asked for one"),
    el("button", { class: "btn primary", type: "submit", text: "Borrow from it" }));
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const added = await store.mutate(
      () => api.addPeer(project, {
        address: address.value.trim(),
        // Whichever control is showing — the menu once the machine has been asked, the
        // text box until then.
        their_project: (form.elements.their_project?.value || "").trim(),
        token: token.value.trim(),
      }),
      `${project} now fetches through that machine`);
    if (added) {
      await store.loadProject();
      again();
    }
  });
  return el("div", { class: "stack" }, ...rows, form);
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
  // Read before it is drawn: the empty table and the defaults beneath it are a statement
  // about where this project's misses go, and making it before the list has arrived
  // would describe a project nobody has looked at.
  if (!store.hasLoaded("upstreams")) return loading("Reading upstreams");
  const rows = (store.state.upstreams || []).slice().sort((a, b) => a.priority - b.priority);
  const canOperate = store.canOperate();
  const kinds = configurable();
  const defaults = defaultsInEffect(rows);

  const ecoSelect = select("eco", kinds.map((d) => ({ value: d.id, label: d.display })), kinds[0]?.id);
  const nameInput = input("name", { required: true, autocomplete: "off", spellcheck: "false" });
  const urlInput = input("url", { required: true, type: "url" });
  const nameField = field("Name", nameInput, " ");
  // The name is the part people get wrong: it is the alias a row answers for, not a
  // label, so the hint says what that ecosystem's names are and what reusing one does.
  const describe = () => {
    const descriptor = kinds.find((d) => d.id === ecoSelect.value);
    const name = descriptor ? primaryDefault(descriptor) : "";
    nameInput.placeholder = name || "an alias";
    urlInput.placeholder = descriptor?.default_upstreams?.[name] || "https://";
    nameField.querySelector(".field-hint").textContent = descriptor ? nameHint(descriptor) : "";
  };
  ecoSelect.addEventListener("change", describe);
  describe();

  const form = el(
    "form",
    { class: "form row-form" },
    field("Ecosystem", ecoSelect),
    nameField,
    field("URL", urlInput),
    // Origin or peer: the only two kinds the server accepts.
    field("Kind", select("kind", [
      { value: "origin", label: "origin" },
      { value: "peer", label: "peer" },
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
    describe();
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
        { label: "Name", cell: (row) => {
          const unused = unusedBecause(row, rows);
          return unused
            ? el("span", { class: "inline" }, row.name, el("span", { class: "note", text: unused }))
            : row.name;
        } },
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
                    if (!await askConfirm({
                      title: "Remove upstream",
                      body: `Remove the upstream ${row.name}?\n` +
                        `${afterRemoving(row, rows)} Cached content is untouched.`,
                      confirmLabel: "Remove", danger: true,
                    })) return;
                    await store.mutate(
                      () => api.deleteUpstream(store.state.project, row.id), `Removed ${row.name}`);
                  },
                  { kind: "ghost small danger" })
              : "—",
        },
      ],
      rows,
      // Not "nowhere to go", which this said for years: a name no row replaces is answered
      // by the ecosystem's own default, listed beneath.
      { empty: "No upstreams of this project's own. Misses go to the built-in origins below." },
    ),
    defaults.length
      ? [
          el("h3", { class: "sub-heading", text: "Built-in origins in effect" }),
          el("p", { class: "note",
            text: "Each ecosystem's own defaults, answering every name no row above replaces. " +
              "A row with the same name replaces one outright." }),
          table(
            [
              { label: "Eco", cell: (row) =>
                el("span", { class: "badge" },
                  el("span", { class: "swatch", style: `background:${ecoColor(row.eco)}` }),
                  el("span", { text: row.eco })) },
              { label: "Name", cell: (row) => row.name },
              { label: "URL", cell: (row) => el("code", { text: row.url, title: row.url }) },
            ],
            defaults,
          ),
        ]
      : null,
    canOperate ? form : null,
  );
}

/* The ecosystems a row can be written for. apt, files and git take their origin from the
 * request itself (upstream_shape "none") and the server refuses a row for them; listed,
 * they were also the default selection — alphabetically first — so an Add left at its
 * defaults failed every time. */
function configurable() {
  return store.state.ecosystems.filter((d) => d.upstream_shape && d.upstream_shape !== "none");
}

/* Which default an override most likely means, for the placeholder: the one named after
 * the ecosystem (root/pypi among pypi's eleven), else the first. Only ever a suggestion. */
function primaryDefault(descriptor) {
  const names = Object.keys(descriptor.default_upstreams || {}).sort();
  return names.find((name) => name.includes(descriptor.id)) ?? names[0] ?? "";
}

/* Rows that are part of a name's chain. A peer row is asked by digest and answers for no
 * name, and a disabled row is not published at all. */
function chained(rows, eco) {
  return rows.filter((row) => row.eco === eco && row.enabled && row.kind !== "peer");
}

/* The one name a single-origin ecosystem uses. The daemon merges the defaults with this
 * project's rows by name and takes the first name in sort order, so a row named anything
 * that sorts after the default's is never asked. */
function singleName(descriptor, rows) {
  const names = new Set(Object.keys(descriptor.default_upstreams || {}));
  for (const row of chained(rows, descriptor.id)) names.add(row.name);
  return [...names].sort()[0] ?? "";
}

function nameHint(descriptor) {
  const name = primaryDefault(descriptor);
  const origin = descriptor.default_upstreams?.[name];
  if (descriptor.upstream_shape === "single") {
    return name
      ? `${descriptor.display} uses one origin, the first name in sort order: name it ${name} ` +
        `to replace ${origin}`
      : `${descriptor.display} uses one origin, the first name in sort order`;
  }
  return name
    ? `the alias clients address: ${name} replaces ${origin}; ` +
      "a new name adds one beside the defaults"
    : "the alias clients address";
}

/* What this project's misses are answered by when no row of the same name replaces it. */
function defaultsInEffect(rows) {
  const out = [];
  for (const descriptor of configurable()) {
    const named = new Set(chained(rows, descriptor.id).map((row) => row.name));
    const only = descriptor.upstream_shape === "single" ? singleName(descriptor, rows) : null;
    for (const name of Object.keys(descriptor.default_upstreams || {}).sort()) {
      if (named.has(name) || (only !== null && only !== name)) continue;
      out.push({ eco: descriptor.id, name, url: descriptor.default_upstreams[name] });
    }
  }
  return out;
}

// Why an enabled row is never asked, or "" when it is.
function unusedBecause(row, rows) {
  const descriptor = store.state.ecosystems.find((d) => d.id === row.eco);
  if (descriptor?.upstream_shape !== "single" || !row.enabled || row.kind === "peer") return "";
  const used = singleName(descriptor, rows);
  return used === row.name ? "" : `not used: ${row.eco} takes ${used}, first in sort order`;
}

// What answers for a row's name once it is gone.
function afterRemoving(row, rows) {
  if (row.kind === "peer") return "Lookups by digest stop asking it.";
  const descriptor = store.state.ecosystems.find((d) => d.id === row.eco);
  const left = rows.filter((other) => other.id !== row.id);
  if (descriptor?.upstream_shape === "single") {
    const name = singleName(descriptor, left);
    const next = chained(left, row.eco).find((other) => other.name === name)?.url ??
      descriptor.default_upstreams?.[name];
    return next
      ? `${row.eco} misses then go to ${name}, ${next}.`
      : `${row.eco} has nothing else to fetch from, so its misses fail.`;
  }
  const rest = chained(left, row.eco).filter((other) => other.name === row.name);
  if (rest.length) return `The rest of the ${row.name} chain still answers: ${rest.map((r) => r.url).join(", ")}.`;
  const fallback = descriptor?.default_upstreams?.[row.name];
  return fallback
    ? `${row.name} goes back to its built-in origin, ${fallback}.`
    : `Nothing else answers for ${row.name}, so misses addressed to it fail.`;
}
