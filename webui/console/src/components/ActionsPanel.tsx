import { useState } from "react";
import { Panel } from "./ui";
import type { JobAction, JobStatus } from "../lib/types";

export interface ActiveJob {
  action: string;
  status: JobStatus;
  log: string;
}

function statusColors(status: JobStatus): { color: string; bg: string } {
  if (status === "done") return { color: "var(--ok)", bg: "var(--ok-bg)" };
  if (status === "failed") return { color: "var(--bad)", bg: "var(--bad-bg)" };
  return { color: "var(--warn)", bg: "var(--warn-bg)" };
}

function JobConsole({ job, onClose }: { job: ActiveJob; onClose: () => void }) {
  const c = statusColors(job.status);
  const running = job.status === "running";
  return (
    <div className="console-card">
      <div className="console-head">
        <span
          className="dot lg"
          style={{ background: c.color, animation: running ? "pcc-pulse 1s ease-in-out infinite" : "none" }}
        />
        <span className="console-cmd">$ pkgcache {job.action}</span>
        <span className="console-status" style={{ color: c.color, background: c.bg }}>
          {job.status}
        </span>
        <span className="spacer" />
        <button className="console-close" onClick={onClose}>
          close
        </button>
      </div>
      <pre className="console-pre">
        {job.log}
        {running && <span className="cursor"> ▋</span>}
      </pre>
    </div>
  );
}

export function ActionsPanel({
  busy,
  job,
  onStart,
  onCloseJob,
}: {
  busy: boolean;
  job: ActiveJob | null;
  onStart: (action: JobAction, params: Record<string, string>) => void;
  onCloseJob: () => void;
}) {
  const [ckmsg, setCkmsg] = useState("");
  const [exdrive, setExdrive] = useState("/media/shuttle");
  const [imdrive, setImdrive] = useState("/media/shuttle");

  const checkpoint = () => {
    if (busy) return;
    onStart("checkpoint", { message: ckmsg.trim() });
    if (ckmsg.trim()) setCkmsg("");
  };

  return (
    <Panel
      className="actions"
      title="Maintenance actions"
      headRight={
        <>
          <span className="spacer" />
          <span className="note">{busy ? "job running…" : "one job at a time"}</span>
        </>
      }
    >
      <div className="actions-body">
        <div>
          <label className="field-label">Checkpoint — quiesce, hash, version &amp; commit the cache</label>
          <div className="row-inline">
            <input
              className="input"
              value={ckmsg}
              onChange={(e) => setCkmsg(e.target.value)}
              placeholder={'message, e.g. "added torch 2.3 + curl"'}
            />
            <button className="btn btn-primary" disabled={busy} onClick={checkpoint}>
              ⎘ Checkpoint
            </button>
          </div>
        </div>

        <div className="actions-grid">
          <div>
            <label className="field-label">Export delta → shuttle</label>
            <div className="row-inline">
              <input
                className="input"
                value={exdrive}
                onChange={(e) => setExdrive(e.target.value)}
                placeholder="/media/shuttle"
              />
              <button
                className="btn btn-ghost"
                disabled={busy}
                onClick={() => exdrive.trim() && !busy && onStart("export", { drive: exdrive.trim() })}
              >
                ↥ Export
              </button>
            </div>
          </div>
          <div>
            <label className="field-label">Import ← shuttle</label>
            <div className="row-inline">
              <input
                className="input"
                value={imdrive}
                onChange={(e) => setImdrive(e.target.value)}
                placeholder="/media/shuttle"
              />
              <button
                className="btn btn-ghost"
                disabled={busy}
                onClick={() => imdrive.trim() && !busy && onStart("import", { drive: imdrive.trim() })}
              >
                ↧ Import
              </button>
            </div>
          </div>
        </div>

        {job && <JobConsole job={job} onClose={onCloseJob} />}
      </div>
    </Panel>
  );
}
