try {
  document.documentElement.dataset.theme = localStorage.getItem("pcc_theme") || "dark";
} catch {
  // Private mode or blocked storage: keep the markup default.
}
