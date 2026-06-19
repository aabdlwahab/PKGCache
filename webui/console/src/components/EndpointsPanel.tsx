import { useState } from "react";
import { Panel } from "./ui";
import { ecoColors } from "../lib/format";
import { ECOS, type Endpoints, type Eco } from "../lib/types";
import type { Theme } from "../lib/uiState";

export function EndpointsPanel({ endpoints, theme }: { endpoints: Endpoints; theme: Theme }) {
  const [copied, setCopied] = useState<Eco | "">("");

  const copy = (eco: Eco, cmd: string) => {
    try {
      navigator.clipboard?.writeText(cmd);
    } catch {
      /* clipboard may be unavailable on http origins */
    }
    setCopied(eco);
    setTimeout(() => setCopied((c) => (c === eco ? "" : c)), 1400);
  };

  const rows = ECOS.filter((eco) => endpoints[eco]);

  return (
    <Panel title="Pull endpoints">
      <div className="list-pad">
        {rows.map((eco) => {
          const cmd = endpoints[eco] as string;
          const c = ecoColors(eco, theme);
          const isCopied = copied === eco;
          return (
            <div className="ep-row" key={eco}>
              <span className="ep-eco" style={{ color: c.color }}>
                <span className="eco-square" style={{ background: c.color }} />
                {eco}
              </span>
              <code className="ep-cmd" title={cmd}>
                {cmd}
              </code>
              <button
                className="copy-btn"
                style={{ color: isCopied ? "var(--ok)" : "var(--muted)" }}
                onClick={() => copy(eco, cmd)}
              >
                {isCopied ? "✓ copied" : "copy"}
              </button>
            </div>
          );
        })}
      </div>
    </Panel>
  );
}
