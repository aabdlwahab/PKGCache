"""Mutating actions (checkpoint/export/import/rollback) run as background jobs
whose combined output streams into an in-memory log the UI polls. Only one job
runs at a time — concurrent ones would race on git/dvc/the proxies."""
import itertools
import re
import subprocess
import threading

from config import ROOT

_jobs = {}
_job_ids = itertools.count(1)
_lock = threading.Lock()


def _busy():
    return any(j["status"] == "running" for j in _jobs.values())


def build_commands(action, params):
    """Translate a UI action into a list of argv lists (run sequentially).

    Every value that reaches a command is a separate argv element — never
    interpolated into a shell string — and commit hashes are format-checked."""
    if action == "checkpoint":
        msg = (params.get("message") or "").strip()
        if not msg:
            raise ValueError("a checkpoint message is required")
        return [["bash", "scripts/checkpoint.sh", msg]]
    if action == "export":
        drive = (params.get("drive") or "").strip()
        if not drive:
            raise ValueError("a shuttle drive path is required")
        return [["bash", "scripts/export-shuttle.sh", drive]]
    if action == "import":
        drive = (params.get("drive") or "").strip()
        if not drive:
            raise ValueError("a shuttle drive path is required")
        return [["bash", "scripts/import-airgap.sh", drive]]
    if action == "rollback":
        commit = (params.get("commit") or "").strip()
        if not re.fullmatch(r"[0-9a-f]{7,40}", commit):
            raise ValueError("invalid commit hash")
        return [["git", "checkout", commit], ["dvc", "checkout"]]
    if action == "mode":
        target = _mode_target(params)
        # Recreate just the cache container under the target profile; the matching
        # OFFLINE env is injected via _job_env so behavior is air-gap or online.
        return [["docker", "compose", "--profile", target, "up", "-d", "--no-deps", "pkgcache"]]
    raise ValueError(f"unknown action: {action}")


def _mode_target(params):
    target = (params.get("target") or "").strip()
    if target not in ("online", "offline"):
        raise ValueError("mode target must be 'online' or 'offline'")
    return target


def _job_env(action, params):
    """Extra environment for a job's commands. Mode-switch sets OFFLINE so the
    recreated pkgcache container serves cache-only (offline) or fetches (online)."""
    if action == "mode":
        return {"OFFLINE": "1" if _mode_target(params) == "offline" else "0"}
    return {}


def _run_job(job):
    import os

    env = dict(os.environ)
    if job.get("profile"):
        env["COMPOSE_PROFILE"] = job["profile"]
    env.update(job.get("env") or {})
    for cmd in job["commands"]:
        with _lock:
            job["log"] += f"$ {' '.join(cmd)}\n"
        try:
            proc = subprocess.Popen(
                cmd, cwd=str(ROOT), env=env, text=True,
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            )
            for line in proc.stdout:
                with _lock:
                    job["log"] += line
            proc.wait()
        except Exception as exc:  # noqa: BLE001 - surface anything to the UI
            with _lock:
                job["log"] += f"\n[error] {exc}\n"
                job["status"] = "failed"
            return
        if proc.returncode != 0:
            with _lock:
                job["log"] += f"\n[exited {proc.returncode}]\n"
                job["status"] = "failed"
            return
    with _lock:
        job["status"] = "done"


def start_job(action, params):
    if _busy():
        raise RuntimeError("another operation is already running")
    commands = build_commands(action, params)
    jid = next(_job_ids)
    job = {
        "id": jid, "action": action, "status": "running", "log": "",
        "commands": commands, "profile": (params.get("profile") or "").strip(),
        "env": _job_env(action, params),
    }
    _jobs[jid] = job
    threading.Thread(target=_run_job, args=(job,), daemon=True).start()
    return jid


def get_job(jid):
    with _lock:
        job = _jobs.get(jid)
        if not job:
            return None
        return {"id": job["id"], "action": job["action"], "status": job["status"], "log": job["log"]}


def jobs_snapshot():
    with _lock:
        return {
            "busy": _busy(),
            "jobs": [
                {"id": j["id"], "action": j["action"], "status": j["status"]}
                for j in _jobs.values()
            ],
        }
