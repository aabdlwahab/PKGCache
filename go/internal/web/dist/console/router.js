/* Fragment routing: #/cache?eco=pypi&q=six
 *
 * The fragment never reaches the server, so the console needs no catch-all route and
 * a mistyped asset path still 404s honestly. Views stay bookmarkable and the back
 * button works, which a single scrolling page cannot offer.
 */

const routes = new Map();
let current = null;
let mountPoint = null;

export function define(name, view) {
  routes.set(name, view);
}

/* Which views this session may enter. Injected rather than imported so the router
   keeps knowing nothing about identity; boot supplies it once the store has `me`.
   The server refuses the data either way — this only avoids mounting a view that
   would render as a wall of permission errors. */
let reachable = () => true;
let fallback = () => "overview";

export function restrict(isReachable, fallbackName) {
  reachable = isReachable;
  fallback = fallbackName;
  render();
}

/** Parse the fragment into a view name and its query parameters. */
export function parse(hash = location.hash) {
  const raw = hash.replace(/^#\/?/, "");
  const [name, query] = raw.split("?");
  return {
    name: name || "overview",
    params: Object.fromEntries(new URLSearchParams(query || "")),
  };
}

export function href(name, params = {}) {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== "") query.set(key, String(value));
  }
  const text = query.toString();
  return `#/${name}${text ? `?${text}` : ""}`;
}

export function go(name, params = {}) {
  location.hash = href(name, params);
}

/** Update the current view's parameters without leaving it. */
export function setParams(params) {
  const route = parse();
  go(route.name, { ...route.params, ...params });
}

export function start(node) {
  mountPoint = node;
  addEventListener("hashchange", render);
  render();
}

/** Re-enter the current view from scratch. Used when the project changes, since a
 *  view's mounted shell is built around one project's shape. */
export function remount() {
  current = null;
  render();
}

function render() {
  // The store can wake a subscriber that remounts before start() has run — boot does
  // exactly that if its ordering is disturbed. Doing nothing is the correct answer:
  // start() renders once it has a mount point.
  if (!mountPoint) return;
  const route = parse();
  if (!reachable(route.name)) {
    // A hand-typed or bookmarked fragment for a view this session cannot enter.
    // Replace rather than push, so Back does not walk into the same wall again.
    location.replace(`#/${fallback()}`);
    return;
  }
  const view = routes.get(route.name) || routes.get("overview");
  if (!view) return;

  if (current?.name !== route.name) {
    current?.teardown?.();
    mountPoint.replaceChildren();
    // mount() builds the shell once and returns a teardown plus an update hook.
    const mounted = view.mount(mountPoint, route.params) || {};
    current = { name: route.name, ...mounted };
    document.querySelectorAll("[data-nav]").forEach((link) => {
      link.classList.toggle("active", link.dataset.nav === route.name);
      // Announce the current view for a screen reader, not just visually.
      if (link.dataset.nav === route.name) link.setAttribute("aria-current", "page");
      else link.removeAttribute("aria-current");
    });
    // Moving between views must move focus, or a keyboard user stays parked in the
    // nav while the whole page changes underneath them.
    mountPoint.focus({ preventScroll: true });
    return;
  }
  current.params?.(route.params);
}
