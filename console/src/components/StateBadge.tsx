import type { JobState } from "@/api/types";
import { cn } from "@/lib/utils";

const styles: Record<JobState, string> = {
  running: "bg-primary/15 text-primary",
  queued: "bg-amber-500/15 text-amber-700 dark:text-amber-400",
  done: "bg-emerald-500/15 text-emerald-700 dark:text-emerald-400",
  failed: "bg-destructive/15 text-destructive",
  canceled: "bg-muted text-muted-foreground",
};

export function StateBadge({ state }: { state: JobState }) {
  return (
    <span className={cn("inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium", styles[state] ?? styles.canceled)}>
      {state === "running" && <span className="size-1.5 animate-pulse rounded-full bg-current" />}
      {state}
    </span>
  );
}
