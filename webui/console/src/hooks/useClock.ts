import { useEffect, useState } from "react";

/** Returns Date.now() (ms), updated every `everyMs`. One timer for the whole
 *  app drives all relative-time labels and the footer clock. */
export function useClock(everyMs = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), everyMs);
    return () => clearInterval(t);
  }, [everyMs]);
  return now;
}
