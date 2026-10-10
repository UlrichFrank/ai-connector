import { useEffect, useState } from "react";

/** The time, renewed every `every` ms, so that "5 min ago" stays true while the page is open. */
export function useNow(every = 30000): number {
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), every);
    return () => clearInterval(t);
  }, [every]);
  return now;
}
