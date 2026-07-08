import { useState } from "react";
import { Panel } from "./ui";
import { ecoColors } from "../lib/format";
import { ECOS, type Endpoints, type Eco } from "../lib/types";
import type { Theme } from "../lib/uiState";

// The backend sends "<host>" as a placeholder; the console knows the real host.
const substHost = (s: string) => s.replace(/<host>/g, window.location.hostname);

export function EndpointsPanel({ endpoints, theme }: { endpoints: Endpoints; theme: Theme }) {
  const [copied, setCopied] = useState<string>("");
  const [open, setOpen] = useState<Eco | "">("");

  const copy = (key: string, text: string) => {
    try {
      navigator.clipboard?.writeText(text);
    } catch {
      /* clipboard may be unavailable on http origins */
    }
    setCopied(key);
    setTimeout(() => setCopied((c) => (c === key ? "" : c)), 1400);
  };

  const rows = ECOS.filter((eco) => endpoints[eco]);

  return (
    <Panel title="Pull endpoints">
      <div className="list-pad">
        {rows.map((eco) => {
          const ep = endpoints[eco]!; // rows is filtered to defined entries
          const c = ecoColors(eco, theme);
          const url = substHost(ep.url);
          const setup = (ep.setup ?? []).map(substHost);
          const isOpen = open === eco;
          return (
            <div key={eco}>
              <div className="ep-row">
                <span className="ep-eco" style={{ color: c.color }}>
                  <span className="eco-square" style={{ background: c.color }} />
                  {eco}
                </span>
                <code className="ep-cmd" title={ep.note ? `${url}   (${ep.note})` : url}>
                  {url}
                </code>
                <button
                  className="copy-btn"
                  style={{ color: copied === eco ? "var(--ok)" : "var(--muted)" }}
                  onClick={() => copy(eco, url)}
                >
                  {copied === eco ? "✓ copied" : "copy"}
                </button>
                {setup.length > 0 && (
                  <button
                    className="copy-btn"
                    style={{ color: isOpen ? "var(--fg2)" : "var(--muted)" }}
                    onClick={() => setOpen(isOpen ? "" : eco)}
                  >
                    {isOpen ? "hide" : "setup"}
                  </button>
                )}
              </div>
              {isOpen && (
                <div className="ep-setup">
                  {ep.note && <div className="ep-setup-note">{ep.note}</div>}
                  <pre className="ep-setup-cmds">{setup.join("\n")}</pre>
                  <button
                    className="copy-btn"
                    style={{ color: copied === `${eco}-setup` ? "var(--ok)" : "var(--muted)" }}
                    onClick={() =>
                      copy(`${eco}-setup`, setup.filter((l) => !l.trimStart().startsWith("#")).join("\n"))
                    }
                  >
                    {copied === `${eco}-setup` ? "✓ copied" : "copy commands"}
                  </button>
                </div>
              )}
            </div>
          );
        })}
      </div>
    </Panel>
  );
}
