/* Dialogs that do not depend on the webview implementing them.
 *
 * WKWebView does not implement alert(), confirm() or prompt() itself — it asks its
 * WKUIDelegate, and Wails' macOS backend implements exactly one delegate method, the
 * open panel for file inputs. The three JavaScript panels are absent, and WKWebView's
 * documented behaviour without them is not an error: alert() does nothing, confirm()
 * returns false, prompt() returns null.
 *
 * So on a Mac "New project" silently did nothing — prompt() answered null and the caller
 * read that as cancelled — and every confirm() in this UI silently answered no, which
 * quietly disabled rollback, deleting a project, removing packages and forgetting a
 * source. It worked everywhere else because WebKitGTK and WebView2 both provide their
 * own panels when the host does not.
 *
 * Nothing here asks the webview for anything. It is the same modal the file picker uses,
 * which is also why it looks like the rest of the window rather than like the operating
 * system.
 */

import { el, region, button } from "./dom.js";

/** Open a modal over the page. Returns a close function.
 *
 * The caller supplies the body and the actions; this owns the overlay, the escape key,
 * the backdrop click and the focus. */
export function openModal({ ariaLabel, title, body, actions, onCancel, wide = false }) {
  const box = el(
    "div",
    { class: "dlg-box" + (wide ? " is-wide" : ""),
      role: "dialog", "aria-modal": "true", "aria-label": ariaLabel || title },
    title ? el("div", { class: "dlg-head" }, el("strong", { text: title })) : null,
    body,
    el("div", { class: "dlg-actions" }, ...actions),
  );
  const overlay = el("div", { class: "dlg-overlay" }, box);

  function close() {
    document.removeEventListener("keydown", onKey, true);
    overlay.remove();
  }
  function onKey(event) {
    if (event.key === "Escape") {
      event.stopPropagation();
      close();
      onCancel?.();
      return;
    }
    // Focus stays inside while the dialog is up. Without this, tab walks into the page
    // behind it — which is still there, still clickable by keyboard, and no longer the
    // thing the reader is answering.
    if (event.key !== "Tab") return;
    const focusable = box.querySelectorAll(
      "button, input, select, textarea, a[href], [tabindex]:not([tabindex='-1'])");
    if (!focusable.length) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }
  document.addEventListener("keydown", onKey, true);
  overlay.addEventListener("click", (event) => {
    // Only a click on the backdrop itself, never one that started inside the box.
    if (event.target !== overlay) return;
    close();
    onCancel?.();
  });

  document.body.appendChild(overlay);
  return close;
}

/** Ask for a line of text. Resolves to the trimmed value, or null if cancelled.
 *
 * The replacement for window.prompt, which on a Mac answers null without asking. */
export function askText({
  title, label, placeholder = "", value = "", confirmLabel = "OK", validate,
} = {}) {
  return new Promise((resolve) => {
    const field = el("input", {
      class: "dlg-input", value, placeholder, autocomplete: "off", spellcheck: "false",
    });
    const errorRegion = region("div", { class: "dlg-error" });
    let close = () => {};

    function submit() {
      const answer = field.value.trim();
      // Refused in place rather than by closing and reopening: a dialog that vanishes
      // on a bad answer takes the answer with it.
      const complaint = validate ? validate(answer) : (answer ? "" : "A name is required.");
      if (complaint) {
        errorRegion.set(el("span", { text: complaint }));
        field.focus();
        return;
      }
      close();
      resolve(answer);
    }

    const form = el("form", { class: "dlg-body" },
      label ? el("label", { class: "dlg-label", text: label }) : null,
      field, errorRegion.node);
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      submit();
    });

    close = openModal({
      title,
      body: form,
      onCancel: () => resolve(null),
      actions: [
        button("Cancel", () => { close(); resolve(null); }),
        button(confirmLabel, submit, { kind: "primary" }),
      ],
    });
    field.focus();
  });
}

/** Ask a yes-or-no question. Resolves true only if the reader said yes.
 *
 * The replacement for window.confirm, which on a Mac answers false without asking —
 * so every guarded action behind one quietly did nothing. */
export function askConfirm({
  title, body, confirmLabel = "Continue", danger = false,
} = {}) {
  return new Promise((resolve) => {
    let close = () => {};
    // Each paragraph on its own line: these questions name what is about to be lost,
    // and a wall of text is the shape people stop reading.
    const lines = String(body || "").split("\n").filter((line) => line.trim() !== "");
    const confirmButton = button(confirmLabel, () => { close(); resolve(true); },
      { kind: danger ? "danger" : "primary" });
    close = openModal({
      title,
      body: el("div", { class: "dlg-body" },
        ...lines.map((line) => el("p", { class: "dlg-line", text: line }))),
      onCancel: () => resolve(false),
      actions: [button("Cancel", () => { close(); resolve(false); }), confirmButton],
    });
    // The safe answer holds focus: Enter on an unread dialog must not delete a project.
    confirmButton.parentElement?.firstElementChild?.focus();
  });
}
