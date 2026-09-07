/* Choosing a place on the cache's machine.
 *
 * A pack is a file that goes onto a stick and comes back off one, and until now neither
 * end could be named from a window: export wrote into the cache's own outbox under a name
 * the server chose, and import read whatever single pack was sitting in the inbox. The
 * reasoning was that a browser cannot move a file off the machine — true, and beside the
 * point, because the machine it cannot move it off is the one the window is running on.
 *
 * So this browses the daemon's filesystem rather than the viewer's. It is deliberately
 * not a file input: the file the person wants is on the cache's disk, and a file input
 * would hand us bytes from theirs.
 *
 * Only offered where the daemon browses a machine at all — a pkgreg answers 404 and the
 * callers fall back to what they did before.
 */

import { el, region, button, loading } from "./dom.js";
import { openModal } from "./dialog.js";
import { api } from "./api.js";
import { bytes } from "./format.js";

/** Ask for a directory to write a pack into. Resolves to a path, or null if cancelled. */
export function pickDirectory({ title = "Where should the pack go?", confirm = "Export here" } = {}) {
  return open({ title, confirm, wantDir: true });
}

/** Ask for a pack to read. Resolves to a path, or null if cancelled. */
export function pickPack({ title = "Which pack?", confirm = "Import this" } = {}) {
  return open({ title, confirm, wantDir: false });
}

/** Whether the daemon browses a machine. Callers use it to decide whether to offer the
 *  picker at all, rather than opening one that can only fail. */
export async function canBrowse() {
  try {
    await api.browse("");
    return true;
  } catch {
    return false;
  }
}

function open({ title, confirm, wantDir }) {
  return new Promise((resolve) => {
    let current = null; // the directory being shown, null at the starting places
    let chosen = null; // the pack picked, for the import side
    const listRegion = region("div", { class: "pk-list" });
    const whereRegion = region("div", { class: "pk-where" });

    const confirmButton = button(confirm, () => finish(wantDir ? current : chosen), {
      kind: "primary",
    });
    // The same modal as every other question this window asks — one overlay, one escape
    // key, one focus trap — rather than a second implementation that drifts from it.
    let close = () => {};
    function finish(value) {
      close();
      resolve(value || null);
    }
    close = openModal({
      title,
      // A file browser needs the room a yes-or-no question does not.
      wide: true,
      body: el("div", { class: "pk-body" }, whereRegion.node, listRegion.node),
      onCancel: () => resolve(null),
      actions: [button("Cancel", () => finish(null)), confirmButton],
    });

    async function show(path) {
      listRegion.set(loading("Reading"));
      let listing;
      try {
        listing = await api.browse(path ?? "");
      } catch (cause) {
        listRegion.set(el("p", { class: "note", text: cause?.message || String(cause) }));
        return;
      }
      current = listing.path || null;
      chosen = null;
      render(listing);
    }

    function render(listing) {
      whereRegion.set(
        el(
          "div",
          {},
          el("code", { text: listing.path || "Places" }),
          // Said before the export rather than after it: a pack is gigabytes, and
          // finding out the stick is mounted read-only at the end is the worst moment.
          wantDir && listing.path && !listing.writable
            ? el("span", { class: "pk-warn", text: "  not writable" })
            : null,
        ),
      );

      const rows = [];
      if (listing.parent || listing.path) {
        rows.push(
          rowButton("..", () => show(listing.parent || ""), { dir: true, muted: true }),
        );
      }
      for (const entry of listing.entries) {
        rows.push(
          entry.dir
            ? rowButton(entry.label ? `${entry.label} — ${entry.name}` : entry.name,
                () => show(entry.path), { dir: true })
            : rowButton(`${entry.name}  ${bytes(entry.size || 0)}`, () => {
                chosen = entry.path;
                render(listing); // redraw so the selection shows
              }, { selected: chosen === entry.path }),
        );
      }
      if (!rows.length) {
        rows.push(el("p", { class: "note", text: "Nothing here." }));
      }
      listRegion.set(rows);

      // Exporting needs a directory, which is wherever we are; importing needs a pack,
      // which has to have been clicked.
      confirmButton.disabled = wantDir
        ? !listing.path || !listing.writable
        : !chosen;
    }

    function rowButton(label, onClick, { dir = false, muted = false, selected = false } = {}) {
      const node = el("button", {
        type: "button",
        class: "pk-row" + (dir ? " is-dir" : "") + (muted ? " is-muted" : "") +
          (selected ? " is-selected" : ""),
        text: label,
      });
      node.addEventListener("click", onClick);
      return node;
    }

    void show("");
  });
}
