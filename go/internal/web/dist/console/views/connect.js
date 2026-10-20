/* Connect — "how do I point my tool at this?"
 *
 * A first-class view rather than a panel, because it is the first thing a new user
 * needs and the thing they come back to whenever they add a machine.
 *
 * Connect and /tutorial are deliberately not the same page, and an earlier version blurred
 * that line badly: both walked through download, rename, chmod, start, exit, and each
 * ended by linking to the other as "the source of truth". A reader who followed those
 * links went in a circle between two pages that said the same thing in different words.
 *
 * The split now is by what each one can know. Connect renders values only this running
 * instance has — the selected project, its exact setup command, whether it needs a token,
 * the live fingerprint, what is actually published — and states each step in one line.
 * The tutorial explains, and owns everything that is the same on every instance: what the
 * session changes, Docker, CI hosts, troubleshooting. So this file links there for
 * explanation and never restates it.
 *
 * Both binaries serve this page, and the reader stands somewhere different on each. On
 * pkgreg they are at another machine, which needs pkgcache downloaded, pointed at this
 * server once, and a shell opened. On pkgcache they are already on the machine the cache
 * serves: there is nothing to download and no CA to verify, and the only question is how
 * long the settings should last. Either way the shell they end up in is a pkgcache shell,
 * so the ecosystem cards below the first panel describe the same thing on both. */

import { el, region, panel, fill, table, button, field, input, select, loading } from "../dom.js";
import { api } from "../api.js";
import { askConfirm } from "../dialog.js";
import * as store from "../store.js";
import { when, ecoColor } from "../format.js";

export default {
  mount(node) {
    const local = store.isLocal();
    const project = store.state.project;
    const endpoints = region("div");
    const start = region("div");
    const tokens = region("div");

    fill(
      node,
      el("div", { class: "view-head" },
        el("h1", { text: "Connect" }),
        // The tutorial is the team server's getting-started page — downloads, fingerprints,
        // a server to point at — none of which a laptop's own cache has, so pkgcache does
        // not send its reader there.
        local
          ? el("p", { class: "note", text: `Commands for this machine, scoped to project ${project}.` })
          : el("p", { class: "note", text: `Exact values for this instance and project ${project}. The tutorial explains what each step does.` }),
        local ? null : el("a", { class: "btn ghost", href: `/tutorial?project=${encodeURIComponent(project)}`, text: "Open the tutorial" })),
      el("div", { class: "panel-grid" },
        local
          ? panel("Use it on this machine", { note: "each command starts the cache if it is not running", wide: true }, start.node)
          : panel("Start a session", { note: "one executable: no sudo, no system trust store, no insecure TLS flag", wide: true }, start.node),
        panel("Your tools in that shell", { note: "generated from this instance's ecosystems", wide: true }, endpoints.node),
        panel("Access tokens", { note: local ? "only for file uploads" : "only for protected projects and file uploads" }, tokens.node)),
    );

    const draw = () => {
      endpoints.set(renderEndpoints());
      start.set(local ? renderLocal() : renderServer());
      tokens.set(renderTokens());
    };
    // "projects" is in this list because both the setup command and the token notice
    // depend on whether the selected project requires one. It used to be missing, so a
    // reader who landed here before the project list arrived was shown the command
    // without -token-file and no warning, and nothing ever redrew it.
    const unsubscribe = [store.on(
      ["endpoints", "onboarding", "tokens", "ecosystems", "coordinates", "downloads", "projects"],
      draw,
    )];
    draw();

    return { teardown: () => unsubscribe.forEach((off) => off()) };
  },
};

function renderEndpoints() {
  const entries = Object.entries(store.state.endpoints || {});
  if (!entries.length) return el("p", { class: "empty", text: "No ecosystems registered." });

  return el(
    "div",
    { class: "eco-cards" },
    entries.map(([id, endpoint]) => {
      const descriptor = store.state.ecosystems.find((d) => d.id === id);
      const steps = endpoint.setup || [];
      return el(
        "article",
        { class: "eco-card" },
        el("header", {},
          el("span", { class: "swatch", style: `background:${ecoColor(id)}` }),
          el("strong", { text: descriptor?.display || id }),
          el("span", { class: "note", text: endpoint.listener || "" })),
        descriptor?.summary ? el("p", { class: "note", text: descriptor.summary }) : null,
        steps.length
          ? el("div", { class: "steps" }, steps.map(renderStep))
          : el("p", { class: "empty", text: "No setup needed." }),
      );
    }),
  );
}

// A descriptor step is either a comment or a command — never both, and never a
// numbered instruction. Rendering them as an ordered list produced empty bullets for
// the comments, so each kind gets the shape it actually is.
function renderStep(step) {
  if (typeof step === "string") return el("p", { class: "step-note", text: step });
  if (step.comment) return el("p", { class: "step-note", text: step.comment });
  if (step.command) return commandBlock(step.command);
  return null;
}

function commandBlock(command) {
  const code = el("code", { text: command });
  return el(
    "div",
    { class: "command" },
    el("pre", {}, code),
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

// renderLocal is Connect on pkgcache. There is nothing to download or verify here, so
// the four ways of using the cache are the whole panel, ordered from the one that
// changes least to the one that changes most. Each takes -project, and each is given the
// selected one: left to its default, a command uses whatever `pkgcache project use`
// last chose, which need not be the project this page is showing.
function renderLocal() {
  const scope = `-project ${store.state.project}`;
  const port = localPort();
  return el(
    "div",
    { class: "stack" },
    el("h3", { class: "sub-heading", text: "A shell" }),
    commandBlock(`pkgcache shell ${scope}`),
    el("p", { class: "note", text: "pip, uv, npm, pnpm, go and git in it use this cache, and the next panel is what to type there. Type exit to return to your terminal as it was." }),
    el("h3", { class: "sub-heading", text: "One command" }),
    commandBlock(`pkgcache run ${scope} -- npm ci`),
    el("h3", { class: "sub-heading", text: "Settings that outlive the shell" }),
    commandBlock(`pkgcache persist ${scope}`),
    el("p", { class: "note", text: "Writes user-level settings for npm, pip, uv and git, so a Makefile, an IDE or a terminal opened earlier use the cache too. No root and nothing under /etc; -dry-run shows every change first, and -uninstall reverses them." }),
    el("h3", { class: "sub-heading", text: "This terminal only" }),
    commandBlock(`eval "$(pkgcache env ${scope})"`),
    el("p", { class: "note", text: "The shell's settings, printed as exports for sh, bash or zsh to keep." }),
    // Other machines get one paragraph, because this cache is not something they can be
    // pointed at: its packages answer on loopback only, and sharing opens the console
    // alone. What another machine can do is borrow from it, which is a pkgcache command
    // run over there.
    el("h3", { class: "sub-heading", text: "Other machines" }),
    el("p", { class: "note", text: `Packages are served to this machine only; pkgcache share opens this console to other machines, not the packages. Another machine can reuse what this one has cached by running its own pkgcache and borrowing from this one as a peer, for pypi and oci. That needs this cache listening beyond loopback (PKGCACHE_ADDR=0.0.0.0:${port}); then, on the other machine:` }),
    commandBlock(`pkgcache peer add <this-machine>:${port}`),
  );
}

// localPort is the port this cache answers on, for the one command that has to name it
// from another machine. 41780 is every pkgcache's default.
function localPort() {
  return store.state.coordinates?.unified?.match(/:(\d+)$/)?.[1] || "41780";
}

// renderServer is Connect on pkgreg: download pkgcache, point it at this server once,
// open a shell. The commands are the ones `pkgcache setup -h` documents, and -project is
// deliberately absent from them: there it names a project on the developer's machine,
// while the project this page is showing is the server's, which is -team-project.
function renderServer() {
  // Until the endpoints answer, a missing onboarding block means "not read yet", and
  // saying the server has no CA on that basis would be a false claim about it.
  if (!store.hasLoaded("endpoints")) return loading("Reading this server's certificate");

  const onboarding = store.state.onboarding;
  const coordinates = store.state.coordinates;
  const origin = coordinates
    ? `${coordinates.scheme}://${coordinates.unified}`
    : "https://<cache-host>:8443";
  const binaries = store.state.downloads.filter((item) => item.tool === "pkgcache");

  if (!onboarding) {
    return el(
      "div",
      { class: "stack" },
      el("div", { class: "token-needed" },
        el("strong", { text: "This server has no certificate authority, so no machine can verify it." }),
        el("p", { class: "note", text: "pkgcache setup trusts a server only once its CA matches a fingerprint the developer was given, and there is no CA to match. On the server host, mint one — with a certificate pair, into the data directory — and restart pkgreg serve. pkgreg doctor reports what is missing." }),
        commandBlock("pkgreg init -data-dir <data-dir>")),
      el("p", { class: "note", text: "Downloads stay available meanwhile." }),
      pkgcacheDownloads(binaries),
    );
  }

  const fingerprintFact = el("dl", { class: "facts" },
    el("div", { class: "fact" },
      el("dt", { text: "CA fingerprint" }),
      el("dd", {}, el("code", { text: onboarding.ca_sha256 }))));

  // A scheme other than https means this server is answering without its certificate.
  // setup refuses an http:// server by design — there is nothing for the pin to check —
  // so rendering the command anyway hands out one that cannot work, which is what this
  // page used to do: the reader hit "server must use https" with no hint that the
  // blocker was on the server.
  if (coordinates && coordinates.scheme !== "https") {
    return el(
      "div",
      { class: "stack" },
      el("div", { class: "token-needed" },
        el("strong", { text: "This server is answering in plain HTTP, so no machine can be pointed at it." }),
        el("p", { class: "note", text: `pkgcache setup stops with "server must use https" against ${origin}. It fetches the CA over one unverified request and pins it to the fingerprint below; without TLS there is nothing to check it against.` }),
        el("p", { class: "note", text: "The server has a CA but is not serving a certificate. On the server host, issue a pair into the data directory and restart pkgreg serve; init keeps the existing CA, so the fingerprint below stays the one to give out. To use a pair you already have, set server.tls.cert_file and key_file instead. pkgreg doctor reports a missing pair as a failure." }),
        commandBlock("pkgreg init -data-dir <data-dir>")),
      fingerprintFact,
      el("p", { class: "note", text: "Downloads and this fingerprint stay available meanwhile, so developers can fetch pkgcache and check the fingerprint while the server is fixed." }),
      pkgcacheDownloads(binaries),
    );
  }

  const project = store.state.project;
  const selectedProject = store.state.projects.find((item) => item.name === project);
  const needsToken = selectedProject?.data_plane_auth === "token";
  const tokenFlag = needsToken ? " -token-file ./pkgreg.token" : "";
  const target = `-server ${origin} -ca-sha256 ${onboarding.ca_sha256} -team-project ${project}`;
  const tutorial = `/tutorial?project=${encodeURIComponent(project)}`;

  return el(
    "div",
    { class: "stack" },
    el("h3", { class: "sub-heading", text: "1. Download pkgcache" }),
    el("p", { class: "note", text: "One executable, not an installer. Most Intel and AMD machines are amd64; Apple silicon is darwin arm64. Rename it to pkgcache (pkgcache.exe on Windows) and, on Linux or macOS, chmod +x it. Each button's tooltip carries the SHA-256 to check against." }),
    pkgcacheDownloads(binaries),
    el("h3", { class: "sub-heading", text: "2. Point it at this server, once" }),
    // The fingerprint is the value people come back to this page for, so it is stated
    // here rather than folded into a details element as it once was — and stated next
    // to the command it appears in, since the point is to compare the two.
    fingerprintFact,
    el("p", { class: "note", text: "Compare it with a value the operator gave you through a separate channel before running anything. setup refuses to continue if this server presents a different CA." }),
    needsToken ? el("div", { class: "token-needed" },
      el("strong", { text: "This project requires an access token." }),
      el("p", { class: "note", text: "Create a read token below, save the one-time secret in ./pkgreg.token, chmod 600 that file, and keep the -token-file option in the setup command." })) : null,
    el("p", { class: "platform-label", text: "Linux and macOS" }),
    commandBlock(`./pkgcache setup ${target} -limit 25G${tokenFlag}`),
    el("p", { class: "platform-label", text: "Windows PowerShell" }),
    commandBlock(`.\\pkgcache.exe setup ${target} -limit 25G${tokenFlag}`),
    el("p", { class: "note", text: "That keeps a cache of up to 25G on this machine: a lookup goes there first, then to this server, and to the public registry only while this server cannot be reached (-no-direct forbids even that). To cache nothing here and reach this server through a verified loopback bridge instead — what pkgreg-client used to do:" }),
    commandBlock(`./pkgcache setup -no-cache ${target}${tokenFlag}`),
    el("h3", { class: "sub-heading", text: "3. Open a shell" }),
    commandBlock("./pkgcache shell"),
    el("p", { class: "note", text: "Or run one command with its tools pointed at the cache: ./pkgcache run -- npm ci. On Windows, every command is the same with .\\pkgcache.exe." }),
    el("div", { class: "session-result" },
      el("strong", { text: "What happens next" }),
      el("p", { class: "note", text: "setup runs once per machine. It checks the fingerprint, keeps the CA and this server's address inside pkgcache's own directory, and changes nothing outside it. pkgcache shell then opens a child shell in this terminal whose pip, uv, npm, go and git use the cache; the next panel is what to type there. Type exit to return to your unchanged terminal, and open another shell whenever you need one." })),
    el("h3", { class: "sub-heading", text: "Other machines and other cases" }),
    el("p", { class: "note", text: "Docker's daemon never sees the shell: run pkgcache docker-setup once, then pkgcache pull or pkgcache build. For settings that outlive the shell, pkgcache persist writes user-level npm, pip, uv and git settings, with no root. A CI runner or shared build host that needs this server trusted machine-wide runs setup.sh or setup.ps1 below instead; they install its CA into the system and Docker trust stores, and need administrator rights to do it." }),
    el("div", { class: "link-row" },
      el("a", { class: "btn ghost", href: `${tutorial}#docker`, text: "Docker" }),
      el("a", { class: "btn ghost", href: `${tutorial}#persist`, text: "CI and shared hosts" }),
      el("a", { class: "btn ghost", href: onboarding.ca_url, download: "pkgreg-ca.crt", text: "Download CA only" }),
      el("a", { class: "btn ghost", href: onboarding.setup_sh_url, text: "Read setup.sh" }),
      el("a", { class: "btn ghost", href: onboarding.setup_ps1_url, text: "Read setup.ps1" })),
  );
}

// pkgcacheDownloads renders one button per published platform, or — for the state every
// fresh instance is in — the command that fixes it. Saying "the operator must publish the
// release" left even the operator guessing, and this is the wall every developer hits
// first.
//
// Only tool "pkgcache" is offered. pkgreg-client is still published, but as a shim that
// runs pkgcache for instructions written before the two merged; handing it out as the
// thing to download is how this page came to print a command that could not run.
function pkgcacheDownloads(binaries) {
  if (!binaries.length) {
    return el("div", { class: "token-needed" },
      el("strong", { text: "pkgcache is not published on this server." }),
      el("p", { class: "note", text: "Nobody can start a session until it is. On the server host, run:" }),
      commandBlock("pkgreg publish-client"),
      el("p", { class: "note", text: "The pkgcache-* release files must be on that host first, beside pkgreg or at a path given to the command; pkgreg doctor reports whether it worked." }));
  }
  return el("div", { class: "link-row" }, binaries.map((item) =>
    el("a", {
      class: "btn",
      href: `/api/v1/downloads/${encodeURIComponent(item.name)}`,
      download: item.name,
      text: `${platformLabel(item)} · ${fileSize(item.bytes)}`,
      title: item.sha256 ? `SHA-256 ${item.sha256}` : "",
    })));
}

function platformLabel(item) {
  const os = { linux: "Linux", darwin: "macOS", windows: "Windows" }[item.os] || item.os;
  return `${os} ${item.arch}`;
}

function fileSize(value) {
  const bytes = Number(value || 0);
  return bytes >= 1_048_576
    ? `${(bytes / 1_048_576).toFixed(1)} MB`
    : `${Math.max(1, Math.round(bytes / 1024))} KB`;
}

function renderTokens() {
  const rows = store.state.tokens || [];
  const canOperate = store.canOperate();
  const project = store.state.projects.find((item) => item.name === store.state.project);
  const needsToken = project?.data_plane_auth === "token";

  const ecoOptions = [{ value: "", label: "All ecosystems" },
    ...store.state.ecosystems.map((d) => ({ value: d.id, label: d.display }))];

  const form = el(
    "form",
    { class: "stack form" },
    field("Label", input("label", { required: true, placeholder: "ci-runner" })),
    field("Ecosystem", select("eco", ecoOptions, "")),
    field("Scope", select("scope", [{ value: "read", label: "read" }, { value: "write", label: "write" }], "read")),
    field("Expires in (hours)", input("ttl_hours", { type: "number", value: "720", min: "1" })),
    el("button", { class: "btn primary", type: "submit", text: "Create token", disabled: !canOperate }),
  );

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const data = new FormData(form);
    const result = await store.mutate(
      () => api.createToken({
        project: store.state.project,
        eco: String(data.get("eco") || ""),
        scope: String(data.get("scope")),
        label: String(data.get("label")),
        ttl_seconds: Number(data.get("ttl_hours")) * 3600,
        rate_limit: 0,
        rate_burst: 0,
      }),
      "Token created",
    );
    if (result?.secret) {
      // Shown once, never stored. Say so plainly rather than letting someone assume
      // they can come back for it.
      form.prepend(el("div", { class: "secret" },
        el("strong", { text: "Copy this now — it is not stored:" }),
        el("code", { text: result.secret })));
      form.reset();
    }
  });

  return el(
    "div",
    { class: "stack" },
    needsToken
      ? el("p", { class: "note", text: "This project requires a token for package downloads. Create a read token, save the one-time secret in a chmod 600 file, and pass that path to pkgcache setup with -token-file." })
      : el("p", { class: "note", text: store.isLocal()
          ? "Package downloads need no token. You need one only to upload files."
          : "Package downloads are public for this project. You need a token only for protected operations such as file uploads." }),
    table(
      [
        { label: "Label", cell: (row) => row.label || row.id },
        { label: "Eco", cell: (row) => row.eco || "all" },
        { label: "Scope", cell: (row) => row.scope },
        { label: "Expires", cell: (row) => when(row.expires_at) },
        {
          label: "",
          cell: (row) =>
            canOperate
              ? button("Revoke",
                  async () => {
                    // A token is only ever shown once, so a mistaken revoke cannot be
                    // undone by pasting it back — the credential is gone for good.
                    if (!await askConfirm({
                      title: "Revoke token",
                      body: `Revoke the token ${row.label || row.id}?\n` +
                        "Anything still using it — CI jobs, shared hosts, running builds — " +
                        "starts failing authentication immediately. This cannot be undone; " +
                        "you would have to issue a new token and redistribute it.",
                      confirmLabel: "Revoke", danger: true,
                    })) return;
                    await store.mutate(() => api.deleteToken(row.id), "Token revoked");
                  },
                  { kind: "ghost small danger" })
              : "—",
        },
      ],
      rows,
      { empty: "No tokens. The project is reachable without one." },
    ),
    canOperate ? form : el("p", { class: "note", text: "You cannot create tokens in this project." }),
  );
}
