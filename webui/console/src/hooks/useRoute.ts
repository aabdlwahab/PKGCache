import { useEffect, useState } from "react";

// Two real routes driven by the nav control. We use the History API directly
// (no router dependency — this is an air-gap-friendly static SPA): `/` is the
// overview, `/packages` the packages table. nginx + the Vite dev server both
// fall back to index.html, so deep-linking and refresh on /packages work.
export type View = "overview" | "packages";

const PATHS: Record<View, string> = { overview: "/", packages: "/packages" };

function viewFromPath(path: string): View {
  return path.replace(/\/+$/, "") === "/packages" ? "packages" : "overview";
}

export function useRoute(): [View, (v: View) => void] {
  const [view, setView] = useState<View>(() => {
    // The URL wins on load; otherwise restore the last view from localStorage.
    if (viewFromPath(window.location.pathname) === "packages") return "packages";
    try {
      return localStorage.getItem("pcc_view") === "packages" ? "packages" : "overview";
    } catch {
      return "overview";
    }
  });

  useEffect(() => {
    // Sync the URL to the initial view (e.g. one restored from localStorage on `/`)
    // and follow browser back/forward.
    if (viewFromPath(window.location.pathname) !== view) {
      window.history.replaceState(null, "", PATHS[view]);
    }
    const onPop = () => setView(viewFromPath(window.location.pathname));
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
    // Mount-only: this seeds the URL and wires popstate once.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const navigate = (v: View) => {
    setView(v);
    try {
      localStorage.setItem("pcc_view", v);
    } catch {
      /* ignore */
    }
    if (viewFromPath(window.location.pathname) !== v) {
      window.history.pushState(null, "", PATHS[v]);
    }
  };

  return [view, navigate];
}
