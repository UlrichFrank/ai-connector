import { ago } from "@/lib/format";
import { useNow } from "@/lib/now";

/** "5 min ago", kept up to date, with the exact time on hover. */
export function Ago({ iso }: { iso: string | undefined }) {
  useNow();
  if (!iso) return null;
  return (
    <time dateTime={iso} title={new Date(iso).toLocaleString()}>
      {ago(iso)}
    </time>
  );
}
