import { useEffect, useRef, useState } from "react";

export interface Poll<T> {
  data: T | null;
  error: string | null;
  /** true until the first response (success or failure) arrives. */
  loading: boolean;
}

/**
 * Poll an async fetcher on a fixed interval. Re-fetches immediately when the
 * interval changes, aborts in-flight requests on unmount, and keeps the last
 * good data on a transient error (so a blip doesn't blank a panel).
 */
export function usePolling<T>(
  fetcher: (signal: AbortSignal) => Promise<T>,
  intervalMs: number,
  deps: unknown[] = [],
): Poll<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;

  useEffect(() => {
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    let controller: AbortController;

    const run = async () => {
      controller = new AbortController();
      try {
        const next = await fetcherRef.current(controller.signal);
        if (!stopped) {
          setData(next);
          setError(null);
        }
      } catch (e) {
        if (!stopped && (e as Error).name !== "AbortError") {
          setError((e as Error).message);
        }
      } finally {
        if (!stopped) {
          setLoading(false);
          timer = setTimeout(run, intervalMs);
        }
      }
    };
    run();

    return () => {
      stopped = true;
      clearTimeout(timer);
      controller?.abort();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [intervalMs, ...deps]);

  return { data, error, loading };
}
