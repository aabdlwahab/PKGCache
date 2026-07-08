import { Segmented } from "./ui";
import type { Theme, Mode } from "../lib/uiState";
import { GLOBAL_PROJECT, type Commit, type ProjectInfo, type User } from "../lib/types";
import type { View } from "../hooks/useRoute";

// Pick + manage which project the console is viewing. The global project is always
// present and can't be deleted; creating and deleting are gated by role/ownership
// (the backend enforces too — this just hides what the caller can't do).
function ProjectSwitcher({
  projects,
  project,
  canCreate,
  canDelete,
  onSelect,
  onCreate,
  onDelete,
}: {
  projects: ProjectInfo[];
  project: string;
  canCreate: boolean;
  canDelete: boolean;
  onSelect: (p: string) => void;
  onCreate: (name: string) => void;
  onDelete: (name: string) => void;
}) {
  const names = projects.length ? projects : [{ name: GLOBAL_PROJECT } as ProjectInfo];
  return (
    <span className="project-switcher" title="project (its own URLs, cache & version control)">
      <span className="ps-label">proj</span>
      <select value={project} onChange={(e) => onSelect(e.target.value)}>
        {names.map((p) => (
          <option key={p.name} value={p.name}>
            {p.name}
            {p.offline ? " ⦸" : ""}
          </option>
        ))}
      </select>
      {canCreate && (
        <button
          className="ps-btn"
          title="new project"
          onClick={() => {
            const n = window.prompt("New project name (lowercase letters, digits, dashes):");
            if (n && n.trim()) onCreate(n.trim());
          }}
        >
          + new
        </button>
      )}
      {project !== GLOBAL_PROJECT && canDelete && (
        <button
          className="ps-btn ps-del"
          title="delete this project (cached files stay on disk)"
          onClick={() => {
            if (window.confirm(`Remove project '${project}'? Its cached files stay on disk.`))
              onDelete(project);
          }}
        >
          ✕
        </button>
      )}
    </span>
  );
}

export function TopBar({
  theme,
  onToggleTheme,
  view,
  onView,
  mode,
  onMode,
  modeLocked,
  modeLockReason,
  proxyLabel,
  proxyColor,
  headShort,
  projects,
  project,
  canCreateProject,
  canDeleteProject,
  canManageAccounts,
  user,
  onLogout,
  onSelectProject,
  onCreateProject,
  onDeleteProject,
}: {
  theme: Theme;
  onToggleTheme: () => void;
  view: View;
  onView: (v: View) => void;
  mode: Mode;
  onMode: (m: Mode) => void;
  // The per-project mode toggle is locked either because the instance is hard-offline
  // (OFFLINE=1) or the caller doesn't own the project; modeLockReason explains which.
  modeLocked?: boolean;
  modeLockReason?: string;
  proxyLabel: string;
  proxyColor: string;
  headShort: string;
  projects: ProjectInfo[];
  project: string;
  canCreateProject: boolean;
  canDeleteProject: boolean;
  canManageAccounts: boolean;
  // The signed-in account (null in open mode, where auth is off) + logout.
  user: User | null;
  onLogout: () => void;
  onSelectProject: (p: string) => void;
  onCreateProject: (name: string) => void;
  onDeleteProject: (name: string) => void;
}) {
  const viewOptions: { value: View; label: string }[] = [
    { value: "overview", label: "overview" },
    { value: "statistics", label: "statistics" },
    { value: "packages", label: "packages" },
    ...(canManageAccounts ? [{ value: "accounts" as View, label: "accounts" }] : []),
  ];
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

      <ProjectSwitcher
        projects={projects}
        project={project}
        canCreate={canCreateProject}
        canDelete={canDeleteProject}
        onSelect={onSelectProject}
        onCreate={onCreateProject}
        onDelete={onDeleteProject}
      />

      <Segmented<View> value={view} onChange={onView} options={viewOptions} />

      <Segmented<Mode>
        variant="mode"
        modeKind={(v) => (v === "online" ? "on" : "off")}
        value={mode}
        onChange={onMode}
        disabled={modeLocked}
        title={
          modeLocked
            ? modeLockReason ??
              "instance is hard-offline (OFFLINE=1) — the per-project toggle has no effect"
            : `this project's mode (soft — applies within ~5s, other projects untouched)`
        }
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

      {user && (
        <span className="user-pill" title={`signed in as ${user.username}`}>
          {user.username}
          <span className="role">{user.role}</span>
          <button className="logout-btn" onClick={onLogout}>
            sign out
          </button>
        </span>
      )}

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
