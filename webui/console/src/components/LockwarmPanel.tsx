import { useMemo, useState } from "react";
import { Panel } from "./ui";
import { JobConsole, type ActiveJob } from "./ActionsPanel";
import { api } from "../lib/api";
import type { JobAction } from "../lib/types";

/**
 * Upload a uv.lock → warm the cache with every file it pins → download a lock
 * rewritten to pull through this cache. The heavy lifting is a background job
 * (action "lockwarm"); here we read the file client-side and stream its text in
 * the job params, then offer the rewritten lock once the job is done.
 */
export function LockwarmPanel({
  busy,
  job,
  project,
  online,
  onStart,
  onCloseJob,
}: {
  busy: boolean;
  job: ActiveJob | null;
  project: string;
  online: boolean;
  onStart: (action: JobAction, params: Record<string, string>) => void;
  onCloseJob: () => void;
}) {
  const [lock, setLock] = useState("");
  const [filename, setFilename] = useState("");
  const [host, setHost] = useState(window.location.hostname);

  const mine = job?.action === "lockwarm";
  const ready = lock.trim().length > 0 && host.trim().length > 0;
  const done = mine && job?.status === "done";

  // Progress is derived from the streamed job log (the only channel the job model
  // exposes): the "warming N files" banner gives the total, each cached/FAIL line
  // is one finished file.
  const progress = useMemo(() => {
    if (!mine || !job) return null;
    const m = job.log.match(/warming (\d+) files/);
    if (!m) return null;
    const total = parseInt(m[1] ?? "0", 10);
    const finished = (job.log.match(/^ {4}(cached|FAIL)\b/gm) || []).length;
    const failed = (job.log.match(/^ {4}FAIL\b/gm) || []).length;
    return { total, finished, failed, pct: total ? Math.round((finished / total) * 100) : 0 };
  }, [mine, job]);

  const read = (file?: File) => {
    if (!file) return;
    setFilename(file.name);
    file.text().then(setLock);
  };

  const warm = () => {
    if (busy || !ready || !online) return;
    onStart("lockwarm", { lock, host: host.trim() });
  };

  return (
    <Panel
      className="lockwarm"
      title="Warm from uv.lock"
      headRight={
        <>
          <span className="spacer" />
          <span className="note">{online ? "pulls pinned files, then rewrites the lock" : "online only"}</span>
        </>
      }
    >
      <div className="actions-body">
        <div className="xfer-card">
          <div className="xfer-card-head">
            <span className="xfer-card-title accent">⇪ uv.lock</span>
            <span className="xfer-card-cap">cache every pinned file · emit a cache-pointing lock</span>
          </div>

          <label className="field-label">Lock file</label>
          <div className="row-inline">
            <input
              type="file"
              className="input"
              accept=".lock,text/plain"
              disabled={busy}
              onChange={(e) => read(e.target.files?.[0])}
            />
          </div>
          {filename && (
            <p className="xfer-hint">
              loaded <code>{filename}</code> ({lock.length.toLocaleString()} bytes)
            </p>
          )}

          <label className="field-label">Cache host</label>
          <div className="row-inline">
            <input
              className="input"
              value={host}
              onChange={(e) => setHost(e.target.value)}
              placeholder="cache.local"
              disabled={busy}
            />
            <button className="btn btn-primary" disabled={busy || !ready || !online} onClick={warm}>
              ⇪ Warm & rewrite
            </button>
          </div>
          <p className="xfer-note">
            Pulls every file the lock pins into <b>{project}</b>'s cache, then writes a lock whose URLs
            point at <code>{host || "<host>"}</code>. Hashes are kept, so uv still verifies bytes.
          </p>
          {!online && (
            <div className="empty">cache is offline — switch to online mode to warm.</div>
          )}
        </div>

        {progress && (
          <div className="warm-progress">
            <div className="warm-progress-head">
              <span className="note">
                {progress.finished}/{progress.total} files cached
                {progress.failed > 0 && <> · {progress.failed} failed</>}
              </span>
              <span className="spacer" />
              <span className="note">{progress.pct}%</span>
            </div>
            <div className="dl-bar">
              <div
                className="dl-fill"
                style={{
                  width: `${progress.pct}%`,
                  background: progress.failed > 0 ? "var(--warn)" : "var(--accent)",
                }}
              />
            </div>
          </div>
        )}

        {mine && job && <JobConsole job={job} onClose={onCloseJob} />}

        {/* Persistent download row — the button activates once the job is done. */}
        <div className="row-inline">
          <span className="note">{done ? "rewritten lock ready" : "available when the job finishes"}</span>
          <span className="spacer" />
          {done ? (
            <a className="btn btn-ghost" href={api.lockfileUrl(project)} download="uv.lock">
              ↧ Download uv.lock
            </a>
          ) : (
            <button className="btn btn-ghost" disabled>
              ↧ Download uv.lock
            </button>
          )}
        </div>
      </div>
    </Panel>
  );
}
