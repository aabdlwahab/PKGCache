// Scripting is on: the landing page may gate reveal-on-scroll (which starts at
// opacity 0) behind this class, so with JS off every section stays visible.
document.documentElement.classList.add("js");

try {
  document.documentElement.dataset.theme = localStorage.getItem("pcc_theme") || "dark";
} catch {
  // Private mode or blocked storage: keep the markup default.
}
