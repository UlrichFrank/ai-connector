import { Laptop } from "lucide-react";
import type { Job } from "@/api/types";
import { cn } from "@/lib/utils";
import { useManyMacs } from "@/store/app";

/**
 * "on Mac Studio": which Mac holds a job — only when more than one Mac is known (with one, nothing shows).
 * `plain` is text only (inside a line of "·"-separated facts); else a small tag.
 */
export function OnMac({ job, plain, prefix = "", className }: { job: Job; plain?: boolean; prefix?: string; className?: string }) {
  const many = useManyMacs();
  const name = job.worker?.name;
  if (!many || !name) return null;
  if (plain)
    return (
      <span className={cn("truncate", className)} data-on-mac={job.worker?.id}>
        {prefix}on {name}
      </span>
    );
  return (
    <span
      className={cn("inline-flex h-[18px] max-w-40 shrink-0 items-center gap-1 rounded-full bg-muted px-1.5 text-[11px] font-medium text-muted-foreground", className)}
      title={`Runs on ${name}`}
      data-on-mac={job.worker?.id}
    >
      <Laptop className="size-3 shrink-0" />
      <span className="truncate">on {name}</span>
    </span>
  );
}
