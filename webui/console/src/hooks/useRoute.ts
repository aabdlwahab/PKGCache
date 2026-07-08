import { useEffect, useState } from "react";

// Real routes driven by the nav control. We use the History API directly (no
// router dependency — this is an air-gap-friendly static SPA): `/` is the
// overview, `/statistics` the stats tab, `/packages` the packages table. nginx +
// the Vite dev server both fall back to index.html, so deep-linking and refresh
// on any of these work.
export type View = "overview" | "statistics" | "packages" | "accounts";

const PATHS: Record<View, string> = {
  overview: "/",
  statistics: "/statistics",
  packages: "/packages",
  accounts: "/accounts",
};

function viewFromPath(path: string): View {
  const p = path.replace(/\/+$/, "");
  if (p === "/statistics") return "statistics";
  if (p === "/packages") return "packages";
  if (p === "/accounts") return "accounts";
  return "overview";
}

function initialView(): View {
  const fromPath = viewFromPath(window.location.pathname);
  if (fromPath !== "overview") return fromPath; // the URL wins on load
  try {
    const saved = localStorage.getItem("pcc_view");
    if (saved === "statistics" || saved === "packages" || saved === "accounts") return saved;
  } catch {
    /* ignore */
  }
  return "overview";
}

export function useRoute(): [View, (v: View) => void] {
  const [view, setView] = useState<View>(initialView);

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
