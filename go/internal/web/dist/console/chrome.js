/* Persistent shell: wordmark, project switcher, connection state, theme, the
 * activity rail, and the alert strip.
 *
 * The rail is the reason this is a routed app rather than one long page. You start a
 * checkpoint on Transfer and then go read the Cache view while it runs; jobs and live
 * transfers have to be visible from everywhere, and only a persistent element can do
 * that.
 */

import { el, region, button, fill } from "./dom.js";
import { api } from "./api.js";
import * as store from "./store.js";
import { href, remount } from "./router.js";
import { bytes, ago, percent } from "./format.js";
import { initTopbarShader } from "./shader.js";

/* The third field is whether a guest may reach the view. It mirrors the server's
   route allowlist in internal/control/api/guest.go — the server is the enforcement
   point, and this only decides what to draw. A view offered here but refused there
   would be a link to a 403; the reverse merely hides something harmless. */
const NAV = [
  ["overview", "Overview", true],
  ["cache", "Cache", true],
  ["connect", "Connect", true],
  ["sources", "Sources", false],
  ["transfer", "Transfer", false],
  ["admin", "Admin", false],
];

/** The views a session may enter, in nav order. */
export function visibleNav() {
  return store.isGuest() ? NAV.filter(([, , guest]) => guest) : NAV;
}

export function wordmark() {
  return el(
    "div",
    { class: "wordmark" },
    el("span", { class: "br", text: "[" }),
    el("span", { class: "a", text: "pkg" }),
    el("span", { class: "f", text: "reg" }),
    el("span", { class: "br", text: "]" }),
  );
}

export function buildChrome(root) {
  const projectSelect = el("select", { class: "project-select", "aria-label": "Project" });
  projectSelect.addEventListener("change", () => {
    store.setProject(projectSelect.value);
  });

  const live = region("span", { class: "live" });
  const railBody = region("div", { class: "rail-body" });
  const alerts = region("div", { class: "alerts", role: "status", "aria-live": "polite" });

  // The heading takes focus when the rail opens. Without it the drawer appears, focus
  // stays behind in the topbar, and because the rail is the last child of #root its
  // Close button sits behind the entire mounted view in tab order — so a keyboard user
  // could open the rail and have no reachable way out of it.
  const railHeading = el("h2", { text: "Activity", tabindex: "-1" });

  const rail = el(
    "aside",
    { class: "rail", id: "activity-rail", hidden: true, "aria-label": "Activity" },
    el(
      "header",
      { class: "rail-head" },
      railHeading,
      button("Close", () => toggleRail(false), { kind: "ghost" }),
    ),
    railBody.node,
  );

  const railToggle = el("button", {
    class: "btn ghost rail-toggle",
    text: "Activity",
    "aria-controls": "activity-rail",
    "aria-expanded": "false",
    onclick: () => toggleRail(rail.hidden),
  });

  function toggleRail(open) {
    rail.hidden = !open;
    railToggle.setAttribute("aria-expanded", String(open));
    document.body.classList.toggle("rail-open", open);
    if (open) railHeading.focus({ preventScroll: true });
    // Returning focus to the toggle is what makes closing reversible: the reader lands
    // back where they were instead of at the top of the document.
    else if (rail.contains(document.activeElement)) railToggle.focus({ preventScroll: true });
  }

  // Escape closes the rail from anywhere. Below 800px it covers 92% of the viewport
  // with no backdrop, so this is the only dismissal that does not require finding a
  // specific button first.
  rail.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      event.stopPropagation();
      toggleRail(false);
    }
  });

  const themeButton = el("button", {
    class: "btn ghost",
    text: store.state.theme === "dark" ? "Light" : "Dark",
    onclick: () => {
      const next = store.state.theme === "dark" ? "light" : "dark";
      store.setTheme(next);
      themeButton.textContent = next === "dark" ? "Light" : "Dark";
    },
  });

  const identity = region("div", { class: "identity" });
  /* Reactive, because which sections exist depends on `me`, and boot sets `me` after
     this function returns — that ordering is deliberate and documented there. Building
     the links eagerly here would show a guest the three sections they cannot enter,
     for as long as it took the store to notify. */
  const nav = region("nav", { class: "nav", "aria-label": "Sections" });

  // Animated GLSL background behind the top bar. Absolutely positioned, so it
  // sits behind the controls without affecting the flex layout.
  const shaderCanvas = el("canvas", { class: "topbar-shader", "aria-hidden": "true" });

  const header = el(
    "header",
    { class: "topbar" },
    shaderCanvas,
    el("a", { class: "brand", href: href("overview") }, wordmark()),
    nav.node,
    el("div", { class: "spacer" }),
    el("label", { class: "project-switcher" },
      el("span", { class: "ps-label", text: "project" }), projectSelect),
    live.node,
    themeButton,
    identity.node,
    railToggle,
  );

  const main = el("main", { class: "view", id: "view", tabindex: "-1" });

  // The topbar is eleven controls deep, so every keyboard visit to a new view started
  // with eleven tab stops before the content.
  const skip = el("a", { class: "skip-link", href: "#view", text: "Skip to content" });
  skip.addEventListener("click", (event) => {
    // href="#view" alone would only move the scroll position; focus has to be moved
    // explicitly for the next Tab to continue from the content.
    event.preventDefault();
    main.focus();
  });

  fill(root, skip, header, alerts.node, main, rail);
  root.classList.remove("booting");

  // Start the top-bar shader once the canvas is in the document (it needs layout
  // to size itself). Guarded internally against missing WebGL. The wave count follows
  // store.trafficLevel(), so the bar reads as a load gauge: one slow swell on a quiet
  // instance, a busy chop during a sync.
  initTopbarShader(shaderCanvas, { level: store.trafficLevel });

  // ---- reactive pieces ------------------------------------------------------

  store.on(["projects", "project"], () => {
    fill(
      projectSelect,
      store.state.projects.map((project) =>
        el("option", {
          value: project.name,
          selected: project.name === store.state.project,
          text: project.name,
        }),
      ),
    );
  });

  store.on("project", () => {
    // A view's shell is built around one project's shape, so a switch re-enters it
    // rather than trying to patch it in place.
    void store.loadProject().catch(store.fail);
    remount();
  });

  store.on(["connected", "health"], () => {
    const { connected, health } = store.state;
    live.set(
      el("span", { class: `dot ${connected ? "ok" : "bad"}` }),
      el("span", { text: connected ? `live · ${health}` : "reconnecting" }),
    );
  });

  store.on("me", () => {
    nav.set(
      ...visibleNav().map(([name, label]) =>
        el("a", { class: "nav-link", href: href(name), dataset: { nav: name }, text: label }),
      ),
    );
  });

  store.on("me", () => {
    const me = store.state.me;
    if (store.isGuest()) {
      /* "Sign in" rather than "sign out": ending a guest session and starting a real
         one are the same gesture from here, and offering to sign out of something the
         visitor never signed in to reads as a mistake. */
      identity.set(
        el("span", { class: "pill", title: "Read-only view of the global project", text: "guest · read-only" }),
        button("Sign in", async () => {
          await api.logout();
          location.reload();
        }, { kind: "ghost" }),
      );
    } else if (me?.authenticated) {
      identity.set(
        button(`${me.username} · sign out`, async () => {
          await api.logout();
          location.reload();
        }, { kind: "ghost" }),
      );
    } else {
      identity.set(el("span", { class: "note", text: "open mode" }));
    }
  });

  store.on(["error", "notice"], () => {
    const { error, notice } = store.state;
    if (!error && !notice) return alerts.set();
    alerts.set(
      el(
        "div",
        { class: `alert ${error ? "error" : "notice"}` },
        el("span", { text: error || notice }),
        el("button", {
          class: "alert-close",
          text: "×",
          "aria-label": "Dismiss",
          onclick: () => store.set({ error: "", notice: "" }),
        }),
      ),
    );
  });

  store.on(["live", "jobs", "recent"], () => {
    railBody.set(renderRail());
    const running = store.state.jobs.filter((job) => job.status === "running").length;
    const busy = store.state.live.size + running;
    railToggle.textContent = busy ? `Activity · ${busy}` : "Activity";
    railToggle.classList.toggle("busy-hint", busy > 0);
  });

  return { main };
}

function renderRail() {
  const transfers = [...store.state.live.values()];
  const jobs = store.state.jobs.filter((job) => job.status === "running" || job.status === "queued");

  return [
    el("h3", { class: "rail-heading", text: `Transfers (${transfers.length})` }),
    transfers.length
      ? el("ul", { class: "rail-list" }, transfers.map(transferRow))
      : el("p", { class: "empty", text: "Nothing in flight." }),

    el("h3", { class: "rail-heading", text: `Jobs (${jobs.length})` }),
    jobs.length
      ? el("ul", { class: "rail-list" }, jobs.map(jobRow))
      : el("p", { class: "empty", text: "No jobs running." }),

    el("h3", { class: "rail-heading", text: "Recent" }),
    store.state.recent.length
      ? el("ul", { class: "rail-list compact" }, store.state.recent.slice(0, 12).map(recentRow))
      : el("p", { class: "empty", text: "Quiet." }),
  ];
}

function transferRow(event) {
  const received = Number(event.received || 0);
  const total = Number(event.total || 0);
  return el(
    "li",
    { class: "rail-item" },
    el("div", { class: "rail-line" },
      el("code", { text: event.id || "—" }),
      el("span", { class: "note", text: event.eco || "" })),
    total
      ? el("div", { class: "meter" },
          el("div", { class: "meter-fill", style: `width:${Math.min(100, (received / total) * 100)}%` }))
      : null,
    el("span", { class: "note", text: total ? `${bytes(received)} of ${bytes(total)} · ${percent(received, total)}` : bytes(received) }),
  );
}

function jobRow(job) {
  return el(
    "li",
    { class: "rail-item" },
    el("div", { class: "rail-line" },
      el("strong", { text: job.action }),
      el("span", { class: `pill ${job.status}`, text: job.status })),
    el("span", { class: "note", text: `${job.project || "instance"} · ${ago(job.started_at)}` }),
    job.status === "running" || job.status === "queued"
      ? button("Cancel", () => store.mutate(() => api.cancelJob(job.id), `Job ${job.id} cancelled`), { kind: "ghost small" })
      : null,
  );
}

function recentRow(event) {
  const failed = event.kind === "fetch.error";
  return el(
    "li",
    { class: "rail-item compact" },
    el("span", { class: `dot ${failed ? "bad" : "ok"}` }),
    el("code", { text: event.id || event.kind }),
    el("span", { class: "note", text: event.size ? bytes(event.size) : event.kind }),
  );
}

/** The sign-in screen. Rendered instead of the console, never alongside it. */
export function renderLogin(root, message, { guestAvailable = false } = {}) {
  // autocomplete is what lets a password manager fill this form, and what tells the
  // browser these two fields are credentials rather than arbitrary text.
  const username = el("input", {
    name: "username", required: true, autofocus: true,
    autocomplete: "username", autocapitalize: "none", spellcheck: "false",
  });
  const password = el("input", {
    name: "password", type: "password", required: true, autocomplete: "current-password",
  });
  // role="alert" so a failed sign-in is announced. Without it the message appears
  // silently and a screen-reader user is left with a form that simply did nothing.
  const error = el("div", {
    class: "form-error", role: "alert", hidden: !message, text: message || "",
  });

  /* The guest path is offered below the credential fields, not beside them: it is the
     lesser action, and a visitor who has an account should not have to read past it.
     It is a button rather than a link because it performs something — it mints a
     session — and type="button" keeps it from submitting the form it sits inside. */
  const guestNote = el("p", {
    class: "note login-guest-note",
    text: "No account needed. Read-only view of the global project: what is cached, and how to point your tools at it.",
  });
  const guestButton = el("button", {
    class: "btn ghost login-guest", type: "button", text: "Browse as guest",
  });
  guestButton.addEventListener("click", async () => {
    error.hidden = true;
    guestButton.disabled = true;
    try {
      await api.loginGuest();
      location.reload();
    } catch (cause) {
      guestButton.disabled = false;
      error.textContent = cause.message;
      error.hidden = false;
    }
  });

  const form = el(
    "form",
    { class: "login-card" },
    wordmark(),
    el("h1", { class: "login-title", text: "Sign in to the control plane" }),
    el("label", {}, el("span", { text: "Username" }), username),
    el("label", {}, el("span", { text: "Password" }), password),
    error,
    el("button", { class: "btn primary", type: "submit", text: "Sign in" }),
    guestAvailable ? el("div", { class: "login-alt" }, guestButton, guestNote) : null,
  );

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    error.hidden = true;
    try {
      await api.login(username.value, password.value);
      location.reload();
    } catch (cause) {
      error.textContent = cause.message;
      error.hidden = false;
    }
  });

  fill(root, el("div", { class: "login-screen" }, form));
  root.classList.remove("booting");
}
