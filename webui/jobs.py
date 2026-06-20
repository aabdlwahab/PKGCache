"""Mutating actions (checkpoint/export/import/rollback/mode) run as background
jobs whose streamed log the UI polls. Only one runs at a time — concurrent ones
would race on git/dvc/the proxies.

The actual work lives in ops.py (this package) as generators of log lines — the
same code the operator CLI scripts/pkgops.py runs by hand on the air-gapped host.
This module just enforces one-at-a-time, runs the generator on a worker thread,
and accumulates its output into a log the HTTP layer polls."""
import itertools
import threading

import ops

_jobs = {}
_job_ids = itertools.count(1)
_lock = threading.Lock()


def _busy():
    return any(j["status"] == "running" for j in _jobs.values())


def _run_job(job, gen):
    """Drain the op's log-line generator into the job log; mark done/failed."""
    try:
        for line in gen:
            with _lock:
                job["log"] += line
    except Exception as exc:  # noqa: BLE001 - surface anything (OpError + the unexpected) to the UI
        with _lock:
            job["log"] += f"\n[error] {exc}\n"
            job["status"] = "failed"
        return
    with _lock:
        job["status"] = "done"


def start_job(action, params):
    if _busy():
        raise RuntimeError("another operation is already running")
    # Eager validation: ops.build raises OpError on bad input here, on the
    # request thread, so the POST returns 400 instead of failing asynchronously.
    gen = ops.build(action, params)
    jid = next(_job_ids)
    job = {"id": jid, "action": action, "status": "running", "log": ""}
    _jobs[jid] = job
    threading.Thread(target=_run_job, args=(job, gen), daemon=True).start()
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
