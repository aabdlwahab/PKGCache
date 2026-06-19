import { Panel } from "./ui";
import type { Commit } from "../lib/types";

export function HistoryPanel({
  commits,
  busy,
  onRollback,
}: {
  commits: Commit[];
  busy: boolean;
  onRollback: (commit: Commit) => void;
}) {
  return (
    <Panel
      title="History"
      headRight={
        <>
          <span className="spacer" />
          <span className="note">click a checkpoint to roll back</span>
        </>
      }
    >
      <div className="history-scroll">
        {commits.map((c) => {
          const canRoll = c.is_checkpoint && !c.is_head;
          const dot = c.is_head
            ? "var(--ok)"
            : c.is_checkpoint
              ? "var(--accent)"
              : "var(--panel)";
          const border = c.is_head
            ? "var(--ok)"
            : c.is_checkpoint
              ? "var(--accent)"
              : "var(--line2)";
          return (
            <div className="commit-row" key={c.hash}>
              <span className="commit-dot" style={{ background: dot, border: `2px solid ${border}` }} />
              <span className="commit-short">{c.short}</span>
              <span
                className="commit-subject"
                style={{ color: c.is_head ? "var(--fg)" : "var(--fg2)" }}
                title={c.subject}
              >
                {c.subject}
              </span>
              <span className="commit-date">{c.date}</span>
              {c.is_head && <span className="head-badge">HEAD</span>}
              {canRoll && (
                <button className="roll-btn" disabled={busy} onClick={() => onRollback(c)}>
                  roll back
                </button>
              )}
              {c.is_checkpoint && !c.is_head && (
                <span className="ckpt-tag" title="checkpoint">
                  ⎘ ckpt
                </span>
              )}
            </div>
          );
        })}
      </div>
    </Panel>
  );
}
