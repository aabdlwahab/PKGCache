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
 * instance has — the selected project, its exact start command, whether it needs a token,
 * the live fingerprint, what is actually published — and states each step in one line.
 * The tutorial explains, and owns everything that is the same on every instance: what the
 * session changes, Docker, CI hosts, troubleshooting. So this file links there for
 * explanation and never restates it. */

import { el, region, panel, fill, table, button, field, input, select } from "../dom.js";
import { api } from "../api.js";
import * as store from "../store.js";
import { when, ecoColor } from "../format.js";

export default {
  mount(node) {
    const endpoints = region("div");
    const trust = region("div");
    const tokens = region("div");

    fill(
      node,
      el("div", { class: "view-head" },
        el("h1", { text: "Connect" }),
        el("p", { class: "note", text: `Exact values for this instance and project ${store.state.project}. The tutorial explains what each step does.` }),
        el("a", { class: "btn ghost", href: `/tutorial?project=${encodeURIComponent(store.state.project)}`, text: "Open the tutorial" })),
      el("div", { class: "panel-grid" },
        panel("Start a session", { note: "no installation, sudo, cache IP, or insecure TLS flag", wide: true }, trust.node),
        panel("Your tools in that shell", { note: "generated from this instance's ecosystems", wide: true }, endpoints.node),
        panel("Access tokens", { note: "only for protected projects and file uploads" }, tokens.node)),
    );

    const draw = () => {
      endpoints.set(renderEndpoints());
      trust.set(renderTrust());
      tokens.set(renderTokens());
    };
    // "projects" is in this list because both the start command and the token notice
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

function renderTrust() {
  const onboarding = store.state.onboarding;
  if (!onboarding) {
    return el("p", {
      class: "empty",
      text: "Client setup is unavailable because this instance does not expose a TLS certificate authority.",
    });
  }
  const coordinates = store.state.coordinates;
  const origin = coordinates
    ? `${coordinates.scheme}://${coordinates.unified}`
    : "https://<cache-host>:8443";
  const clients = store.state.downloads.filter((item) => item.tool === "client");
  const fingerprintFact = el("dl", { class: "facts" },
    el("div", { class: "fact" },
      el("dt", { text: "CA fingerprint" }),
      el("dd", {}, el("code", { text: onboarding.ca_sha256 }))));

  // A scheme other than https means this instance has no certificate pair configured.
  // The client refuses an http:// server by design — there is nothing for it to pin — so
  // rendering the start command anyway hands out a command that cannot work, which is
  // what this page used to do: the reader hit "server must use https" with no hint that
  // the blocker was on the server.
  if (coordinates && coordinates.scheme !== "https") {
    return el(
      "div",
      { class: "stack" },
      el("div", { class: "token-needed" },
        el("strong", { text: "This instance is serving plain HTTP, so no client can connect." }),
        el("p", { class: "note", text: `pkgreg-client requires an https:// server and stops with "server must use https" against ${origin}. It fetches the CA over one unverified request and pins it to the fingerprint below; without TLS there is nothing for it to verify.` }),
        el("p", { class: "note", text: "On the cache host, mint a certificate pair into the data directory and restart the server; a server started against a data directory that holds one now uses it. pkgreg doctor reports the missing pair as a failure." }),
        commandBlock("pkgreg init -data-dir <data-dir>")),
      fingerprintFact,
      el("p", { class: "note", text: "Downloads and this fingerprint stay available meanwhile, so developers can fetch and verify the client while the server is fixed." }),
      clientDownloads(clients),
    );
  }

  const selectedProject = store.state.projects.find((item) => item.name === store.state.project);
  const needsToken = selectedProject?.data_plane_auth === "token";
  const tokenFlag = needsToken ? " -token-file ./pkgreg.token" : "";
  const tutorial = `/tutorial?project=${encodeURIComponent(store.state.project)}`;

  return el(
    "div",
    { class: "stack" },
    el("h3", { class: "sub-heading", text: "1. Download the client" }),
    el("p", { class: "note", text: "One executable, not an installer. Most Intel and AMD machines are amd64; Apple silicon is darwin arm64. Rename it to pkgreg-client (pkgreg-client.exe on Windows) and, on Linux or macOS, chmod +x it. Each button's tooltip carries the SHA-256 to check against." }),
    clientDownloads(clients),
    el("h3", { class: "sub-heading", text: "2. Start the session" }),
    // The fingerprint is the value people come back to this page for, so it is stated
    // here rather than folded into a details element as it once was — and stated next
    // to the command it appears in, since the point is to compare the two.
    fingerprintFact,
    el("p", { class: "note", text: "Compare it with a value the operator gave you through a separate channel before running anything. The client refuses to continue if the cache presents a different CA." }),
    needsToken ? el("div", { class: "token-needed" },
      el("strong", { text: "This project requires an access token." }),
      el("p", { class: "note", text: "Create a read token below, save the one-time secret in ./pkgreg.token, chmod 600 that file, and keep the -token-file option in the command." })) : null,
    el("p", { class: "platform-label", text: "Linux and macOS" }),
    commandBlock(
      `./pkgreg-client -server ${origin} -project ${store.state.project} ` +
      `-ca-sha256 ${onboarding.ca_sha256}${tokenFlag}`,
    ),
    el("p", { class: "platform-label", text: "Windows PowerShell" }),
    commandBlock(
      `.\\pkgreg-client.exe -server ${origin} -project ${store.state.project} ` +
      `-ca-sha256 ${onboarding.ca_sha256}${tokenFlag}`,
    ),
    el("div", { class: "session-result" },
      el("strong", { text: "What happens next" }),
      el("p", { class: "note", text: "The client verifies the fingerprint, starts a localhost bridge, and opens a child shell in this terminal. Run the commands in the next panel there. Type exit to stop the bridge and return to your unchanged terminal." })),
    el("h3", { class: "sub-heading", text: "Other machines and other cases" }),
    el("p", { class: "note", text: "Docker's daemon does not read that shell, and CI runners and shared build hosts need settings that outlive it. Both are explained in the tutorial; setup.sh and setup.ps1 below are exactly what the persistent path runs, if you want to read them first." }),
    el("div", { class: "link-row" },
      el("a", { class: "btn ghost", href: `${tutorial}#docker`, text: "Docker" }),
      el("a", { class: "btn ghost", href: `${tutorial}#persist`, text: "CI and shared hosts" }),
      el("a", { class: "btn ghost", href: onboarding.ca_url, download: "pkgreg-ca.crt", text: "Download CA only" }),
      el("a", { class: "btn ghost", href: onboarding.setup_sh_url, text: "Read setup.sh" }),
      el("a", { class: "btn ghost", href: onboarding.setup_ps1_url, text: "Read setup.ps1" })),
  );
}

// clientDownloads renders one button per published platform, or — for the state every
// fresh instance is in — the command that fixes it. Saying "the operator must publish the
// release" left even the operator guessing, and this is the wall every developer hits
// first.
function clientDownloads(clients) {
  if (!clients.length) {
    return el("div", { class: "token-needed" },
      el("strong", { text: "No client is published on this instance." }),
      el("p", { class: "note", text: "Nobody can start a session until one is. On the cache host, run:" }),
      commandBlock("pkgreg publish-client"),
      el("p", { class: "note", text: "The pkgreg-client-* release files must be on that host first; pkgreg doctor reports whether it worked." }));
  }
  return el("div", { class: "link-row" }, clients.map((item) =>
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
      ? el("p", { class: "note", text: "This project requires a token for package downloads. Create a read token, save the one-time secret in a chmod 600 file, and pass that path to pkgreg-client with -token-file." })
      : el("p", { class: "note", text: "Package downloads are public for this project. You need a token only for protected operations such as file uploads." }),
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
                    if (!confirm(
                      `Revoke the token ${row.label || row.id}?\n\n` +
                      "Anything still using it — CI jobs, shared hosts, running builds — " +
                      "starts failing authentication immediately. This cannot be undone; " +
                      "you would have to issue a new token and redistribute it.",
                    )) return;
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
