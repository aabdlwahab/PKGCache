import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "../lib/api";
import type { ActiveJob } from "../components/ActionsPanel";
import type { JobAction } from "../lib/types";

/**
 * Drives a single maintenance job: POST /api/jobs, then poll /api/jobs/<id>
 * until it leaves "running". Validation errors (e.g. empty checkpoint message)
 * come back as a 400 and surface as a failed job in the console.
 * `onSettled` fires once when a job finishes so callers can refresh data.
 */
export function useJob(onSettled?: () => void) {
  const [job, setJob] = useState<ActiveJob | null>(null);
  const idRef = useRef<number | null>(null);
  const runningRef = useRef(false);
  const settledRef = useRef(onSettled);
  settledRef.current = onSettled;
  const busy = job?.status === "running";

  const start = useCallback(async (action: JobAction, params: Record<string, string>) => {
    if (runningRef.current) return;
    runningRef.current = true;
    idRef.current = null;
    setJob({ action, status: "running", log: "" });
    try {
      idRef.current = await api.startJob(action, params);
    } catch (e) {
      runningRef.current = false;
      setJob({ action, status: "failed", log: `✗ ${(e as Error).message}` });
    }
  }, []);

  useEffect(() => {
    if (!busy) return;
    let stop = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      if (idRef.current == null) {
        timer = setTimeout(poll, 300); // POST in flight; wait for the id
        return;
      }
      try {
        const r = await api.job(idRef.current);
        if (stop) return;
        setJob({ action: r.action, status: r.status, log: r.log });
        if (r.status === "running") {
          timer = setTimeout(poll, 600);
        } else {
          runningRef.current = false;
          settledRef.current?.();
        }
      } catch {
        if (!stop) timer = setTimeout(poll, 1000);
      }
    };
    timer = setTimeout(poll, 400);
    return () => {
      stop = true;
      clearTimeout(timer);
    };
  }, [busy]);

  const close = useCallback(() => {
    setJob(null);
    idRef.current = null;
    runningRef.current = false;
  }, []);

  return { job, busy, start, close };
}
