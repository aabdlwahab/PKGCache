import { Segmented } from "./ui";
import type { Theme, Mode } from "../lib/uiState";
import type { Commit } from "../lib/types";
import type { View } from "../hooks/useRoute";

export function TopBar({
  theme,
  onToggleTheme,
  view,
  onView,
  mode,
  onMode,
  proxyLabel,
  proxyColor,
  headShort,
}: {
  theme: Theme;
  onToggleTheme: () => void;
  view: View;
  onView: (v: View) => void;
  mode: Mode;
  onMode: (m: Mode) => void;
  proxyLabel: string;
  proxyColor: string;
  headShort: string;
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

      <Segmented<View>
        value={view}
        onChange={onView}
        options={[
          { value: "overview", label: "overview" },
          { value: "packages", label: "packages" },
        ]}
      />

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

      <button className="theme-btn" title="toggle theme" onClick={onToggleTheme}>
        {theme === "dark" ? "☀ light" : "☾ dark"}
      </button>
    </header>
  );
}

export function OfflineBanner({
  totalPkgs,
  head,
}: {
  totalPkgs: number;
  head: Commit | null;
}) {
  return (
    <div className="offline-banner">
      <b>OFFLINE / AIR-GAPPED</b>
      <span>
        upstream fetch disabled — cache misses will fail. Serving {totalPkgs} cached
        artifacts only.
      </span>
      <span className="spacer" />
      <span className="offline-ckpt" title={head ? head.subject : undefined}>
        <span className="offline-ckpt-label">serving checkpoint</span>
        <span className="offline-ckpt-sha">{head ? head.short : "—"}</span>
        {head && <span className="offline-ckpt-subj">{head.subject}</span>}
        {head?.date && <span className="offline-ckpt-date">{head.date}</span>}
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
