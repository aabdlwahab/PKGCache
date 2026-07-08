import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "../lib/api";
import { Panel } from "./ui";
import type { ProjectInfo, Role, User } from "../lib/types";

const ROLES: Role[] = ["user", "admin", "superuser"];

// Account management. A superuser manages everyone (create any role, promote/demote,
// reassign who a user reports to and which admin owns a project, delete). An admin
// manages only the users that report to them (create, reset password, delete). The
// backend is the source of truth for every rule here; this panel just avoids
// offering actions it knows will be refused, and surfaces the error when one is.
export function AccountsPanel({ me, onChanged }: { me: User; onChanged?: () => void }) {
  const isSuper = me.role === "superuser";
  const [users, setUsers] = useState<User[]>([]);
  const [projects, setProjects] = useState<ProjectInfo[]>([]);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const [u, p] = await Promise.all([api.users(), isSuper ? api.projects() : null]);
      setUsers(u.users);
      if (p) setProjects(p.projects);
    } catch (e) {
      setError((e as Error).message);
    }
  }, [isSuper]);

  useEffect(() => {
    load();
  }, [load]);

  // Guard every mutation with the same error surface + reload, and bubble project
  // owner changes up so the switcher's visibility refreshes.
  const run = useCallback(
    async (fn: () => Promise<unknown>) => {
      setError(null);
      try {
        await fn();
        await load();
        onChanged?.();
      } catch (e) {
        setError((e as Error).message);
      }
    },
    [load, onChanged],
  );

  // Accounts a user may be assigned to report to / a project may be owned by.
  const managers = useMemo(
    () => users.filter((u) => u.role === "admin" || u.role === "superuser").map((u) => u.username),
    [users],
  );

  return (
    <div className="page-accounts">
      {error && (
        <div className="acct-error" role="alert">
          <span style={{ flex: 1 }}>{error}</span>
          <button className="copy-btn" onClick={() => setError(null)}>
            dismiss
          </button>
        </div>
      )}

      <CreateAccount me={me} managers={managers} onCreate={run} />

      <Panel title={isSuper ? "Accounts" : "My users"} className="acct-panel">
        <div className="acct-table">
          <div className="acct-row acct-head">
            <span>username</span>
            <span>role</span>
            <span>reports to</span>
            <span className="acct-actions">actions</span>
          </div>
          {users.map((u) => (
            <AccountRow
              key={u.username}
              row={u}
              me={me}
              managers={managers}
              onRun={run}
            />
          ))}
          {users.length === 0 && <div className="empty">no accounts yet</div>}
        </div>
      </Panel>

      {isSuper && (
        <Panel title="Project ownership" className="acct-panel">
          <div className="acct-table">
            <div className="acct-row acct-head">
              <span>project</span>
              <span>owner</span>
              <span className="acct-actions" />
            </div>
            {projects.map((p) => (
              <div className="acct-row" key={p.name}>
                <span className="mono">{p.name}</span>
                <span>
                  <select
                    className="input acct-select"
                    value={p.owner ?? ""}
                    onChange={(e) =>
                      run(() => api.setProjectOwner(p.name, e.target.value))
                    }
                  >
                    <option value="" disabled>
                      {p.owner ?? "— superuser-owned —"}
                    </option>
                    {managers.map((m) => (
                      <option key={m} value={m}>
                        {m}
                      </option>
                    ))}
                  </select>
                </span>
                <span className="acct-actions" />
              </div>
            ))}
            {projects.length === 0 && <div className="empty">no projects</div>}
          </div>
        </Panel>
      )}
    </div>
  );
}

function CreateAccount({
  me,
  managers,
  onCreate,
}: {
  me: User;
  managers: string[];
  onCreate: (fn: () => Promise<unknown>) => Promise<void>;
}) {
  const isSuper = me.role === "superuser";
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<Role>("user");
  // An admin only ever creates users reporting to themselves; a superuser picks.
  const [reportsTo, setReportsTo] = useState<string>(me.username);

  const needsManager = role === "user";
  const canSubmit = username.trim() && password && (!needsManager || reportsTo);

  const submit = () => {
    if (!canSubmit) return;
    const body = {
      username: username.trim(),
      password,
      role: isSuper ? role : ("user" as Role),
      reports_to: (isSuper ? role : "user") === "user"
        ? isSuper
          ? reportsTo
          : me.username
        : null,
    };
    onCreate(() => api.createUser(body)).then(() => {
      setUsername("");
      setPassword("");
    });
  };

  return (
    <Panel title="Create account" className="acct-panel">
      <div className="acct-create">
        <input
          className="input"
          placeholder="username"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
        />
        <input
          className="input"
          type="password"
          placeholder="password (≥ 8 chars)"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        {isSuper && (
          <select
            className="input acct-select"
            value={role}
            onChange={(e) => setRole(e.target.value as Role)}
          >
            {ROLES.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        )}
        {isSuper && needsManager && (
          <select
            className="input acct-select"
            value={reportsTo}
            onChange={(e) => setReportsTo(e.target.value)}
          >
            <option value="" disabled>
              reports to…
            </option>
            {managers.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
        )}
        <button className="btn btn-primary" disabled={!canSubmit} onClick={submit}>
          + create
        </button>
      </div>
      {!isSuper && (
        <p className="note" style={{ marginTop: "0.5rem" }}>
          new users report to you and can view your projects
        </p>
      )}
    </Panel>
  );
}

function AccountRow({
  row,
  me,
  managers,
  onRun,
}: {
  row: User;
  me: User;
  managers: string[];
  onRun: (fn: () => Promise<unknown>) => Promise<void>;
}) {
  const isSuper = me.role === "superuser";
  const isSelf = row.username === me.username;
  const locked = !!row.builtin; // the env-superuser: not editable
  const canDelete = !locked && !isSelf;

  const changeRole = (next: Role) => {
    // Demoting to user needs a manager; default to the acting superuser (reassignable).
    if (next === "user") onRun(() => api.updateUser(row.username, { role: "user", reports_to: me.username }));
    else onRun(() => api.updateUser(row.username, { role: next }));
  };

  const resetPassword = () => {
    const pw = window.prompt(`New password for ${row.username} (≥ 8 chars):`);
    if (pw) onRun(() => api.updateUser(row.username, { password: pw }));
  };

  const remove = () => {
    if (window.confirm(`Delete account '${row.username}'?`)) {
      onRun(() => api.deleteUser(row.username));
    }
  };

  return (
    <div className="acct-row">
      <span className="mono">
        {row.username}
        {locked && <span className="acct-badge">root</span>}
        {isSelf && <span className="acct-badge you">you</span>}
      </span>
      <span>
        {isSuper && !locked && !isSelf ? (
          <select
            className="input acct-select"
            value={row.role}
            onChange={(e) => changeRole(e.target.value as Role)}
          >
            {ROLES.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        ) : (
          <span className="acct-role">{row.role}</span>
        )}
      </span>
      <span>
        {row.role === "user" && isSuper && !locked ? (
          <select
            className="input acct-select"
            value={row.reports_to ?? ""}
            onChange={(e) => onRun(() => api.updateUser(row.username, { reports_to: e.target.value }))}
          >
            {managers.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
        ) : (
          <span className="mono muted">{row.reports_to ?? "—"}</span>
        )}
      </span>
      <span className="acct-actions">
        {!locked && (
          <button className="btn btn-ghost sm" onClick={resetPassword}>
            reset pw
          </button>
        )}
        {canDelete && (
          <button className="btn btn-ghost sm acct-del" onClick={remove}>
            delete
          </button>
        )}
      </span>
    </div>
  );
}
