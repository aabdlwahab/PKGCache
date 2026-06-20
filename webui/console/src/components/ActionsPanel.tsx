import { useEffect, useMemo, useState } from "react";
import { Panel } from "./ui";
import type { Commit, JobAction, JobStatus } from "../lib/types";

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

/** The checkpoint tree used to scope a shuttle export. Newest checkpoint is on
   top; click [base]/[target] on a row to mark the two ends of the diff. The rail
   between the selected base (older) and target (newer) lights up. */
function CheckpointTree({
  checkpoints,
  baseHash,
  targetHash,
  onPick,
  disabled,
}: {
  checkpoints: Commit[];
  baseHash: string;
  targetHash: string;
  onPick: (kind: "base" | "target", hash: string) => void;
  disabled: boolean;
}) {
  const baseIdx = checkpoints.findIndex((c) => c.hash === baseHash);
  const targetIdx = checkpoints.findIndex((c) => c.hash === targetHash);
  // Valid range: target is newer (smaller index) than base in a newest-first list.
  const rangeValid = baseIdx > -1 && targetIdx > -1 && baseIdx > targetIdx;
  const inRange = (i: number) => rangeValid && i >= targetIdx && i <= baseIdx;

  if (checkpoints.length === 0) {
    return <div className="empty">no checkpoints yet — create one above first.</div>;
  }

  return (
    <div className="ckpt-tree">
      {checkpoints.map((c, i) => {
        const isBase = c.hash === baseHash;
        const isTarget = c.hash === targetHash;
        const dotColor = c.is_head ? "var(--ok)" : "var(--accent)";
        return (
          <div className={`ckpt-node ${inRange(i) ? "in-range" : ""}`} key={c.hash}>
            <span className="ckpt-rail">
              <span
                className="ckpt-dot"
                style={{ background: inRange(i) ? "var(--accent)" : dotColor }}
              />
            </span>
            <span className="ckpt-sha">{c.short}</span>
            <span className="ckpt-subj" title={c.subject}>
              {c.subject}
            </span>
            {c.is_head && <span className="head-badge">HEAD</span>}
            <span className="ckpt-date">{c.date}</span>
            <span className="ckpt-picks">
              <button
                className={`ckpt-pick ${isBase ? "active base" : ""}`}
                disabled={disabled}
                title="set as base (diff from)"
                onClick={() => onPick("base", c.hash)}
              >
                base
              </button>
              <button
                className={`ckpt-pick ${isTarget ? "active target" : ""}`}
                disabled={disabled}
                title="set as target (diff to)"
                onClick={() => onPick("target", c.hash)}
              >
                target
              </button>
            </span>
          </div>
        );
      })}
    </div>
  );
}

export function ActionsPanel({
  busy,
  job,
  commits,
  onStart,
  onCloseJob,
}: {
  busy: boolean;
  job: ActiveJob | null;
  commits: Commit[];
  onStart: (action: JobAction, params: Record<string, string>) => void;
  onCloseJob: () => void;
}) {
  const [ckmsg, setCkmsg] = useState("");
  const [exdrive, setExdrive] = useState("/media/shuttle");
  const [imdrive, setImdrive] = useState("/media/shuttle");
  const [baseHash, setBaseHash] = useState("");
  const [targetHash, setTargetHash] = useState("");

  // The selectable checkpoints (every checkpoint commit + HEAD), newest first.
  const checkpoints = useMemo(
    () => commits.filter((c) => c.is_checkpoint || c.is_head),
    [commits],
  );

  // Default the range to "previous checkpoint → HEAD" once history loads, and
  // drop any selection that no longer exists (e.g. after a rollback).
  useEffect(() => {
    if (checkpoints.length === 0) return;
    const has = (h: string) => checkpoints.some((c) => c.hash === h);
    setTargetHash((t) => (t && has(t) ? t : checkpoints[0]?.hash ?? ""));
    setBaseHash((b) => (b && has(b) ? b : checkpoints[1]?.hash ?? ""));
  }, [checkpoints]);

  const baseIdx = checkpoints.findIndex((c) => c.hash === baseHash);
  const targetIdx = checkpoints.findIndex((c) => c.hash === targetHash);
  const rangeValid = baseIdx > -1 && targetIdx > -1 && baseIdx > targetIdx;

  const pick = (kind: "base" | "target", hash: string) => {
    if (busy) return;
    if (kind === "base") setBaseHash(hash);
    else setTargetHash(hash);
  };

  const checkpoint = () => {
    if (busy) return;
    onStart("checkpoint", { message: ckmsg.trim() });
    if (ckmsg.trim()) setCkmsg("");
  };

  const doExport = () => {
    if (busy || !exdrive.trim()) return;
    const params: Record<string, string> = { drive: exdrive.trim() };
    if (rangeValid) {
      params.base = baseHash;
      params.target = targetHash;
    }
    onStart("export", params);
  };

  const baseShort = checkpoints[baseIdx]?.short ?? "?";
  const targetShort = checkpoints[targetIdx]?.short ?? "?";

  return (
    <Panel
      className="actions"
      title="Checkpoint & Transfer"
      headRight={
        <>
          <span className="spacer" />
          <span className="note">{busy ? "job running…" : "one job at a time"}</span>
        </>
      }
    >
      <div className="actions-body">
        <div>
          <label className="field-label">Checkpoint — hash, version &amp; commit the cache (live, no downtime)</label>
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

        <div className="export-block">
          <label className="field-label">
            Export delta → shuttle
            <span className="export-range">
              {rangeValid ? (
                <>
                  diff <b>{baseShort}</b> → <b>{targetShort}</b>
                </>
              ) : (
                "pick a base + target below, or export the full delta"
              )}
            </span>
          </label>
          <div className="row-inline">
            <input
              className="input"
              value={exdrive}
              onChange={(e) => setExdrive(e.target.value)}
              placeholder="/media/shuttle"
            />
            <button className="btn btn-ghost" disabled={busy} onClick={doExport}>
              ↥ Export {rangeValid ? "range" : "all"}
            </button>
          </div>
          <CheckpointTree
            checkpoints={checkpoints}
            baseHash={baseHash}
            targetHash={targetHash}
            onPick={pick}
            disabled={busy}
          />
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

        {job && <JobConsole job={job} onClose={onCloseJob} />}
      </div>
    </Panel>
  );
}
