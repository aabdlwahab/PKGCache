"""Mutating actions (checkpoint/export/import/rollback/mode) run as background
jobs whose streamed log the UI polls. Only one runs at a time — concurrent ones
would race on git/dvc/the proxies.

The actual work lives on the Operations service (ops.py) as generators of log
lines — the same code the operator CLI scripts/pkgops.py runs by hand on the
air-gapped host. This store enforces one-at-a-time, runs the generator on a
worker thread, and accumulates its output into a log the HTTP layer polls."""
import itertools
import threading


class Jobs:
    """Owns the in-memory job table and the single-runner lock. Given an Operations
    service, `start` validates + launches a background job; `get`/`snapshot` read
    the accumulating logs the HTTP layer polls."""

    def __init__(self, operations) -> None:
        self._operations = operations
        self._jobs = {}
        self._ids = itertools.count(1)
        self._lock = threading.Lock()

    def start(self, action, params):
        if self._busy():
            raise RuntimeError("another operation is already running")
        # Eager validation: build raises OpError on bad input here, on the request
        # thread, so the POST returns 400 instead of failing asynchronously.
        gen = self._operations.build(action, params)
        jid = next(self._ids)
        job = {"id": jid, "action": action, "status": "running", "log": ""}
        self._jobs[jid] = job
        threading.Thread(target=self._run, args=(job, gen), daemon=True).start()
        return jid

    def get(self, jid):
        with self._lock:
            job = self._jobs.get(jid)
            if not job:
                return None
            return {"id": job["id"], "action": job["action"], "status": job["status"], "log": job["log"]}

    def snapshot(self):
        with self._lock:
            return {
                "busy": self._busy(),
                "jobs": [
                    {"id": j["id"], "action": j["action"], "status": j["status"]}
                    for j in self._jobs.values()
                ],
            }

    def _busy(self):
        return any(j["status"] == "running" for j in self._jobs.values())

    def _run(self, job, gen):
        """Drain the op's log-line generator into the job log; mark done/failed."""
        try:
            for line in gen:
                with self._lock:
                    job["log"] += line
        except Exception as exc:  # noqa: BLE001 - surface anything (OpError + the unexpected) to the UI
            with self._lock:
                job["log"] += f"\n[error] {exc}\n"
                job["status"] = "failed"
            return
        with self._lock:
            job["status"] = "done"
