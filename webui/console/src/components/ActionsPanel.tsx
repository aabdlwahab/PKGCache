import { useEffect, useMemo, useState } from "react";
import { Panel, Segmented } from "./ui";
import type { Commit, JobAction, JobStatus, ShuttleResp } from "../lib/types";

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
  shuttle,
  pendingNew,
  headShort,
  headDate,
  onStart,
  onCloseJob,
}: {
  busy: boolean;
  job: ActiveJob | null;
  commits: Commit[];
  shuttle?: ShuttleResp | null;
  pendingNew: number;
  headShort: string;
  headDate: string;
  onStart: (action: JobAction, params: Record<string, string>) => void;
  onCloseJob: () => void;
}) {
  const [ckmsg, setCkmsg] = useState("");
  const [exportMode, setExportMode] = useState<"full" | "delta">("full");
  const [baseHash, setBaseHash] = useState("");
  const [targetHash, setTargetHash] = useState("");

  // The selectable checkpoints (every checkpoint commit + HEAD), newest first.
  const checkpoints = useMemo(
    () => commits.filter((c) => c.is_checkpoint || c.is_head),
    [commits],
  );

  // Default the delta range to "previous checkpoint → HEAD" once history loads,
  // and drop any selection that no longer exists (e.g. after a rollback).
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
    if (busy) return;
    // Full = no base (everything, for a fresh machine). Delta = base→target diff.
    if (exportMode === "delta" && rangeValid) onStart("export", { base: baseHash, target: targetHash });
    else onStart("export", {});
  };

  // Checkpoint state pill: warn while artifacts are uncommitted, ok once clean.
  const committed = pendingNew <= 0;
  const pillLabel = committed ? "all committed" : `+${pendingNew} uncommitted`;
  const pillStyle = committed
    ? { color: "var(--ok)", background: "var(--ok-bg)" }
    : { color: "var(--warn)", background: "var(--warn-bg)" };

  // Air-gap diagram arrow reflects the live transfer direction.
  const xfering = busy && (job?.action === "export" || job?.action === "import");
  const xferArrow = xfering ? (job?.action === "export" ? "↦" : "↤") : "⇄";
  const xferArrowColor = xfering ? "var(--accent)" : "var(--muted)";

  const baseShort = checkpoints[baseIdx]?.short ?? "?";
  const targetShort = checkpoints[targetIdx]?.short ?? "?";
  const exportDir = shuttle?.export_dir ?? "shuttle/out";
  const importDir = shuttle?.import_dir ?? "shuttle/in";
  const importReady = !!shuttle?.import_ready;
  const importCkpts = shuttle?.import_checkpoints ?? [];

  return (
    <Panel
      className="actions"
      title="Checkpoint & transfer"
      headRight={
        <>
          <span className="spacer" />
          <span className="note">{busy ? "job running…" : "one job at a time"}</span>
        </>
      }
    >
      <div className="actions-body">
        {/* ---- Checkpoint card ---- */}
        <div className="xfer-card">
          <div className="xfer-card-head">
            <span className="xfer-card-title accent">⎘ Checkpoint</span>
            <span className="xfer-card-cap">quiesce · hash · commit the cache tree</span>
            <span className="spacer" />
            <span className="state-pill" style={pillStyle}>
              {pillLabel}
            </span>
          </div>
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
          <div className="xfer-foot">
            <span className="dot" style={{ background: "var(--ok)" }} />
            last: <span className="head-sha">HEAD {headShort || "—"}</span>
            {headDate && <> · {headDate}</>}
          </div>
        </div>

        {/* ---- Shuttle transfer card ---- */}
        <div className="xfer-card">
          <div className="xfer-card-head">
            <span className="xfer-card-title">⇄ Shuttle transfer</span>
            <span className="xfer-card-cap">move artifacts across the air gap</span>
          </div>

          {/* Air-gap diagram — arrow shows the live transfer direction. */}
          <div className="airgap">
            <div className="airgap-node">
              <div className="airgap-node-t">▣ pkgcache</div>
              <div className="airgap-node-s">this cache</div>
            </div>
            <div className="airgap-conn">
              <span className="airgap-label">air gap</span>
              <div className="airgap-rule" />
              <span className="airgap-arrow" style={{ color: xferArrowColor }}>
                {xferArrow}
              </span>
            </div>
            <div className="airgap-node">
              <div className="airgap-node-t">▤ shuttle</div>
              <div className="airgap-node-s">removable drive</div>
            </div>
          </div>

        {/* ---- Export: writes to the fixed shuttle/out dir ---- */}
        <div className="export-block">
          <label className="field-label">Export → shuttle</label>
          <p className="xfer-note">
            Writes to <code>{exportDir}</code> — then <b>copy everything in that folder onto your
            shuttle media</b> and carry it to the other machine.
          </p>
          <div className="row-inline">
            <Segmented<"full" | "delta">
              value={exportMode}
              onChange={setExportMode}
              options={[
                { value: "full", label: "full" },
                { value: "delta", label: "delta" },
              ]}
            />
            <span className="spacer" />
            <button
              className="btn btn-ghost"
              disabled={busy || (exportMode === "delta" && !rangeValid)}
              onClick={doExport}
            >
              {exportMode === "full" ? "↥ Export everything" : `↥ Export ${baseShort} → ${targetShort}`}
            </button>
          </div>
          {exportMode === "full" ? (
            <p className="xfer-hint">
              Full — sends every checkpoint, for a machine that has nothing yet (import there
              initialises everything).
            </p>
          ) : (
            <>
              <p className="xfer-hint">
                Incremental — only the diff between two checkpoints; the other machine must already
                have the base.
              </p>
              <CheckpointTree
                checkpoints={checkpoints}
                baseHash={baseHash}
                targetHash={targetHash}
                onPick={pick}
                disabled={busy}
              />
            </>
          )}
        </div>

        {/* ---- Import: reads the fixed shuttle/in dir ---- */}
        <div className="export-block">
          <label className="field-label">Import ← shuttle</label>
          <p className="xfer-note">
            <b>Copy your shuttle media's contents into <code>{importDir}</code></b>, then import.
          </p>
          {importReady ? (
            <>
              <div className="ckpt-tree">
                {importCkpts.map((c) => (
                  <div className="ckpt-node" key={c.hash}>
                    <span className="ckpt-rail">
                      <span className="ckpt-dot" style={{ background: "var(--accent)" }} />
                    </span>
                    <span className="ckpt-sha">{c.short}</span>
                    <span className="ckpt-subj" title={c.subject}>
                      {c.subject}
                    </span>
                    <span className="ckpt-date">{c.date}</span>
                  </div>
                ))}
              </div>
              <div className="row-inline">
                <span className="note">
                  {importCkpts.length} checkpoint{importCkpts.length === 1 ? "" : "s"} staged
                </span>
                <span className="spacer" />
                <button className="btn btn-primary" disabled={busy} onClick={() => !busy && onStart("import", {})}>
                  ↧ Import
                </button>
              </div>
            </>
          ) : (
            <div className="empty">nothing staged in {importDir} — copy your media there first.</div>
          )}
        </div>
        </div>

        {job && <JobConsole job={job} onClose={onCloseJob} />}
      </div>
    </Panel>
  );
}
