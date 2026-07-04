import { useCallback, useEffect, useRef, useState } from "react";
import { Panel } from "./ui";
import { api } from "../lib/api";
import { fmtBytes } from "../lib/format";

// The write side of the `files` ecosystem: manage the per-project write token and
// upload artifacts through the webui proxy (which injects the token — the browser
// never holds it). Rendered above the Packages list; uploads/deletes bump the
// parent's refresh key so the list picks them up on its next poll.
export function ArtifactsPanel({
  project,
  online,
  filesEndpoint,
  onChanged,
}: {
  project: string;
  online: boolean;
  filesEndpoint: string; // the endpoints.files hint: "https://<host>:3144/<path>  (…)"
  onChanged: () => void;
}) {
  const [tokenSet, setTokenSet] = useState<boolean | null>(null);
  const [newToken, setNewToken] = useState<string | null>(null);
  const [file, setFile] = useState<File | null>(null);
  const [path, setPath] = useState("");
  const [overwrite, setOverwrite] = useState(false);
  const [progress, setProgress] = useState<number | null>(null);
  const [result, setResult] = useState<{ path: string; sha256: string; size: number } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [drag, setDrag] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  // Refresh token status whenever the selected project changes.
  useEffect(() => {
    let stop = false;
    setNewToken(null);
    setTokenSet(null);
    api.tokenStatus(project).then(
      (r) => !stop && setTokenSet(r.set),
      () => !stop && setTokenSet(false),
    );
    return () => {
      stop = true;
    };
  }, [project]);

  const pickFile = useCallback((f: File | null) => {
    setFile(f);
    setResult(null);
    setError(null);
    if (f) setPath((p) => (p && !p.endsWith("/") ? p : (p + f.name)));
  }, []);

  const onDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault();
      setDrag(false);
      const f = e.dataTransfer.files?.[0];
      if (f) pickFile(f);
    },
    [pickFile],
  );

  const rotate = async () => {
    if (tokenSet && !window.confirm("Rotate the write token? The current one stops working immediately.")) return;
    setError(null);
    try {
      const t = await api.rotateToken(project);
      setNewToken(t);
      setTokenSet(true);
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const upload = async () => {
    if (!file || !path.trim()) return;
    setError(null);
    setResult(null);
    setProgress(0);
    try {
      const r = await api.uploadArtifact(project, path.trim(), file, overwrite, setProgress);
      setResult({ path: r.path, sha256: r.sha256, size: r.size });
      setFile(null);
      setPath("");
      if (inputRef.current) inputRef.current.value = "";
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setProgress(null);
    }
  };

  const wgetCmd = (p: string) => {
    const tmpl = (filesEndpoint || "https://<host>:3144/<path>").split(/\s+/)[0] ?? "";
    const url = tmpl.replace("<host>", window.location.hostname).replace("<path>", p);
    return `wget --ca-certificate=ca.crt ${url}`;
  };

  return (
    <Panel
      title="Artifacts — upload"
      headRight={
        <span className="note">generic files · wget-download · token-gated write</span>
      }
    >
      <div className="artifacts-body">
        {!online && (
          <div className="artifacts-offline note">
            Offline (air-gapped) — uploads are disabled; files serve read-only.
          </div>
        )}

        {/* write token */}
        <div className="af-row">
          <span className="af-label">Write token</span>
          <span className={`badge ${tokenSet ? "ok" : "muted"}`}>
            {tokenSet === null ? "…" : tokenSet ? "set" : "not set"}
          </span>
          <button className="btn" onClick={rotate} disabled={!online}>
            {tokenSet ? "Rotate" : "Generate"}
          </button>
          <span className="note">CI sends <code>Authorization: Bearer &lt;token&gt;</code></span>
        </div>
        {newToken && (
          <div className="af-token">
            <input readOnly value={newToken} onFocus={(e) => e.currentTarget.select()} />
            <button className="copy-btn" onClick={() => navigator.clipboard?.writeText(newToken)}>
              copy
            </button>
            <span className="note af-token-warn">shown once — copy it now</span>
          </div>
        )}

        {/* upload */}
        <div
          className={`af-drop ${drag ? "over" : ""} ${online ? "" : "disabled"}`}
          onDragOver={(e) => {
            e.preventDefault();
            if (online) setDrag(true);
          }}
          onDragLeave={() => setDrag(false)}
          onDrop={online ? onDrop : (e) => e.preventDefault()}
          onClick={() => online && inputRef.current?.click()}
        >
          {file ? (
            <span>
              <b>{file.name}</b> · {fmtBytes(file.size)}
            </span>
          ) : (
            <span className="note">drop a file here, or click to choose</span>
          )}
          <input
            ref={inputRef}
            type="file"
            hidden
            onChange={(e) => pickFile(e.target.files?.[0] ?? null)}
          />
        </div>

        <div className="af-row">
          <span className="af-label">Path</span>
          <input
            className="af-path"
            placeholder="builds/v1.2/app.tar.gz"
            value={path}
            onChange={(e) => setPath(e.target.value)}
            disabled={!online}
          />
          <label className="note af-ow">
            <input type="checkbox" checked={overwrite} onChange={(e) => setOverwrite(e.target.checked)} />
            overwrite
          </label>
          <button className="btn" onClick={upload} disabled={!online || !file || !path.trim() || progress !== null}>
            {progress !== null ? "Uploading…" : "Upload"}
          </button>
        </div>

        {progress !== null && (
          <div className="af-prog">
            <div className="af-prog-fill" style={{ width: `${Math.round(progress * 100)}%` }} />
          </div>
        )}
        {error && <div className="af-error">{error}</div>}
        {result && (
          <div className="af-result">
            <div className="note">
              uploaded <b>{result.path}</b> · {fmtBytes(result.size)} · sha256 {result.sha256.slice(0, 16)}…
            </div>
            <div className="af-token">
              <input readOnly value={wgetCmd(result.path)} onFocus={(e) => e.currentTarget.select()} />
              <button className="copy-btn" onClick={() => navigator.clipboard?.writeText(wgetCmd(result.path))}>
                copy
              </button>
            </div>
          </div>
        )}
      </div>
    </Panel>
  );
}
