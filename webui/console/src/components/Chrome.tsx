import { Segmented } from "./ui";
import type { Theme, Mode } from "../lib/uiState";

export function TopBar({
  theme,
  onToggleTheme,
  mode,
  onMode,
  proxyLabel,
  proxyColor,
  headShort,
  totalPkgs,
  totalSize,
  diskSize,
}: {
  theme: Theme;
  onToggleTheme: () => void;
  mode: Mode;
  onMode: (m: Mode) => void;
  proxyLabel: string;
  proxyColor: string;
  headShort: string;
  totalPkgs: number;
  totalSize: string;
  diskSize: string | null;
}) {
  return (
    <header className="topbar">
      <div className="brand">
        <span className="wordmark">
          <span className="br">[</span>
          <span className="f">pkg</span>
          <span className="a">cache</span>
          <span className="br">]</span>
        </span>
        <span className="sub">air-gap registry</span>
      </div>

      <div style={{ marginLeft: 4 }}>
        <Segmented<Mode>
          variant="mode"
          modeKind={(v) => (v === "online" ? "on" : "off")}
          value={mode}
          onChange={onMode}
          options={[
            { value: "online", label: "● online" },
            { value: "offline", label: "⦸ offline" },
          ]}
        />
      </div>

      <span className="pill" style={{ color: proxyColor }}>
        <span
          className="dot lg pulse"
          style={{ background: proxyColor, animationDuration: "1.7s" }}
        />
        {proxyLabel}
      </span>

      <span className="pill mono">
        HEAD&nbsp;<span className="head-sha">{headShort || "—"}</span>
      </span>

      <span className="spacer" />

      <span className="total" title="cached package payload (docker deduplicated) · total bytes on disk">
        {totalPkgs} pkgs · {totalSize}
        {diskSize && <> · {diskSize} on disk</>}
      </span>

      <button className="theme-btn" title="toggle theme" onClick={onToggleTheme}>
        {theme === "dark" ? "☀ light" : "☾ dark"}
      </button>
    </header>
  );
}

export function OfflineBanner({ totalPkgs }: { totalPkgs: number }) {
  return (
    <div className="offline-banner">
      <b>OFFLINE / AIR-GAPPED</b>
      <span>
        upstream fetch disabled — cache misses will fail. Serving {totalPkgs} cached
        artifacts only.
      </span>
    </div>
  );
}

export function Footer({ clock }: { clock: string }) {
  return (
    <footer className="footer">
      <span>pkgcache · pull-through cache for docker · npm · pip · apt · apk</span>
      <span className="spacer" />
      <span>{clock}</span>
    </footer>
  );
}
