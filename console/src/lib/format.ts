// Times and sizes as words. Pure: tested with `pnpm test` (src/lib/format.test.ts).

/** A span in seconds, short: "12 s", "5 min", "3 h 20 min", "2 d". */
export function span(seconds: number): string {
  if (!isFinite(seconds)) return "";
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} min`;
  const h = Math.floor(m / 60);
  if (h < 24) return m % 60 ? `${h} h ${m % 60} min` : `${h} h`;
  return `${Math.floor(h / 24)} d`;
}

/** How long ago `iso` was, as a span ("5 min"), or "" without a time. */
export function age(iso: string | undefined, now = Date.now()): string {
  if (!iso) return "";
  const t = Date.parse(iso);
  return !(t > 0) ? "" : span((now - t) / 1000);
}

/** "just now", "5 min ago", "yesterday", or the date; "never" without a time. */
export function ago(iso: string | undefined, now = Date.now()): string {
  if (!iso) return "never";
  const t = Date.parse(iso);
  // Go writes its zero time (0001-01-01) for "never"
  if (!(t > 0)) return "never";
  const s = (now - t) / 1000;
  if (s < 45) return "just now";
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  if (s < 2 * 86400) return "yesterday";
  return new Date(t).toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });
}

/** 1.5e9 → "1.5 GB". */
export function bytes(n: number | undefined): string {
  if (!n) return "";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1000 && i < u.length - 1) {
    n /= 1000;
    i++;
  }
  return `${n < 10 && i ? n.toFixed(1) : Math.round(n)} ${u[i]}`;
}
