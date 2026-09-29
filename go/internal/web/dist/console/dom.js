/* Minimal DOM construction. This is the whole "framework".
 *
 * Two rules make a hand-written console maintainable at this size:
 *
 *   1. Text goes through textContent, never innerHTML. The CSP forbids inline
 *      script, but a template-string renderer would still let an artifact name
 *      out of the catalog write markup into the page. Nothing here can.
 *
 *   2. Views mount their shell once and re-render named regions. There is no
 *      diffing, so re-rendering a whole view would blow away focus and selection
 *      mid-typing; regions keep the churn away from anything a person is using.
 */

/** Build an element. Children may be nodes, strings, numbers, or nested arrays;
 *  null and undefined are skipped so `cond && el(...)` reads naturally. */
export function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [name, value] of Object.entries(attrs || {})) {
    if (value === null || value === undefined || value === false) continue;
    if (name === "class") node.className = value;
    else if (name === "text") node.textContent = String(value);
    else if (name === "dataset") Object.assign(node.dataset, value);
    else if (name === "style") applyStyle(node, value);
    else if (name.startsWith("on")) node.addEventListener(name.slice(2), value);
    else if (value === true) node.setAttribute(name, "");
    else node.setAttribute(name, String(value));
  }
  append(node, children);
  return node;
}

/* The CSP is `style-src 'self'` with no unsafe-inline, which blocks the style content
 * attribute — setAttribute("style", …) is silently dropped and every computed width
 * in the console disappears. CSSOM is not covered by that directive, so geometry is
 * applied property by property instead. This is the reason charts render at all. */
function applyStyle(node, value) {
  for (const rule of String(value).split(";")) {
    const colon = rule.indexOf(":");
    if (colon === -1) continue;
    node.style.setProperty(rule.slice(0, colon).trim(), rule.slice(colon + 1).trim());
  }
}

/** Same as el() but in the SVG namespace, which createElement cannot produce. */
export function svg(tag, attrs = {}, ...children) {
  const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
  for (const [name, value] of Object.entries(attrs || {})) {
    if (value === null || value === undefined || value === false) continue;
    if (name === "style") applyStyle(node, value);
    else if (name.startsWith("on")) node.addEventListener(name.slice(2), value);
    else node.setAttribute(name, String(value));
  }
  append(node, children);
  return node;
}

function append(node, children) {
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    node.appendChild(child instanceof Node ? child : document.createTextNode(String(child)));
  }
}

/** Replace a node's children in one pass. */
export function fill(node, ...children) {
  node.replaceChildren();
  append(node, children);
  return node;
}

/** A named region: a container plus a render function that refreshes only it. */
export function region(tag = "div", attrs = {}) {
  const node = el(tag, attrs);
  return {
    node,
    set(...children) {
      fill(node, ...children);
      return node;
    },
  };
}

export const text = (value) => document.createTextNode(String(value));

/** Common shells, so every view looks like the same product. */
export function panel(title, { note, actions, wide } = {}, ...body) {
  return el(
    "section",
    { class: "panel" + (wide ? " panel-wide" : "") },
    el(
      "header",
      { class: "panel-head" },
      el("h2", { class: "panel-title", text: title }),
      note ? el("span", { class: "note", text: note }) : null,
      actions ? el("div", { class: "panel-actions" }, actions) : null,
    ),
    el("div", { class: "panel-body" }, ...body),
  );
}

/* The placeholder for a region whose data has not arrived yet.
 *
 * Distinct from the empty state on purpose: "Nothing cached yet." is a claim about the
 * instance, and making that claim before the request has returned is simply false. This
 * says only that the console is still reading, and says it to assistive tech too — the
 * text is announced, while the bars are decoration. */
export function loading(label = "Loading") {
  return el(
    "div",
    { class: "loading", role: "status" },
    el("span", { class: "sr-only", text: `${label}…` }),
    el("span", { class: "loading-bar", "aria-hidden": "true" }),
    el("span", { class: "loading-bar", "aria-hidden": "true" }),
    el("span", { class: "loading-bar", "aria-hidden": "true" }),
  );
}

/** A table built from a column spec. Cells are text unless a column returns a node,
 *  which keeps the common case safe by default. */
export function table(columns, rows, { empty = "Nothing yet." } = {}) {
  if (!rows.length) return el("p", { class: "empty", text: empty });
  return el(
    "div",
    { class: "table-wrap" },
    el(
      "table",
      {},
      el(
        "thead",
        {},
        el(
          "tr",
          {},
          columns.map((column) =>
            el("th", { class: column.numeric ? "numeric" : null, text: column.label }),
          ),
        ),
      ),
      el(
        "tbody",
        {},
        rows.map((row) =>
          el(
            "tr",
            {},
            columns.map((column) => {
              const value = column.cell(row);
              return el(
                "td",
                { class: column.numeric ? "numeric" : null, title: column.title?.(row) },
                value instanceof Node ? value : text(value ?? "—"),
              );
            }),
          ),
        ),
      ),
    ),
  );
}

/** Buttons carry their own busy state so no view has to track it. */
export function button(label, onClick, { kind = "", disabled = false, title } = {}) {
  const node = el("button", {
    class: ["btn", kind].filter(Boolean).join(" "),
    text: label,
    disabled,
    title,
  });
  node.addEventListener("click", async () => {
    if (node.disabled) return;
    node.disabled = true;
    node.classList.add("busy");
    try {
      await onClick();
    } finally {
      node.disabled = false;
      node.classList.remove("busy");
    }
  });
  return node;
}

/** A cache key, whole, with its last segment set apart.
 *
 *  The last segment is the part a person recognises — the wheel, the tarball, the tag.
 *  What comes before it is a host and hash directories that every key from one index
 *  shares, so it is muted and the name is what the eye lands on. The key wraps rather
 *  than being shortened: cut from the right, every pypi key read the same; cut from the
 *  middle, a long wheel name still lost its end. */
export function cacheKey(key) {
  const value = String(key || "—");
  // The slash before the last segment, allowing for a key that ends in one.
  const cut = value.lastIndexOf("/", value.length - 2) + 1;
  return el(
    "code",
    { class: "key" },
    cut ? el("span", { class: "key-head" }, breakAfterSlashes(value.slice(0, cut))) : null,
    el("span", { class: "key-tail", text: value.slice(cut) }),
  );
}

/* A key has no spaces, so without these it wraps wherever the line happens to end —
   usually mid-segment. With them it breaks between segments, and only a segment longer
   than the line (a hash) is split. */
function breakAfterSlashes(text) {
  return text.split("/").flatMap((part, i, all) =>
    i < all.length - 1 ? [`${part}/`, el("wbr")] : part ? [part] : []);
}

export function field(label, control, hint) {
  return el(
    "label",
    { class: "field" },
    el("span", { class: "field-label", text: label }),
    control,
    hint ? el("span", { class: "field-hint", text: hint }) : null,
  );
}

export function input(name, attrs = {}) {
  return el("input", { name, ...attrs });
}

export function select(name, options, current) {
  return el(
    "select",
    { name },
    options.map((option) => {
      const value = typeof option === "string" ? option : option.value;
      const label = typeof option === "string" ? option : option.label;
      return el("option", { value, selected: value === current, text: label });
    }),
  );
}
