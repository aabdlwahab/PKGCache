import { useCallback, useState } from "react";

/** Persisted string state (theme/skin/mode). Falls back gracefully if
 *  localStorage is unavailable (private mode, sandboxed iframe). */
export function useLocalStorage<T extends string>(
  key: string,
  fallback: T,
): [T, (v: T) => void] {
  const [value, setValue] = useState<T>(() => {
    try {
      return (localStorage.getItem(key) as T) || fallback;
    } catch {
      return fallback;
    }
  });

  const set = useCallback(
    (v: T) => {
      setValue(v);
      try {
        localStorage.setItem(key, v);
      } catch {
        /* ignore */
      }
    },
    [key],
  );

  return [value, set];
}

/** A 1s clock tick — drives relative-time labels in the recent feed. */
export { useClock } from "./useClock";
