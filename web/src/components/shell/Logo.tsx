import { cn } from "@/lib/utils";

/** The mark: a violet tile with an "m". */
export function Logo({ className }: { className?: string }) {
  return (
    <span
      aria-hidden
      className={cn("grid size-7 shrink-0 place-items-center rounded-lg bg-primary text-sm font-bold text-primary-foreground", className)}
    >
      m
    </span>
  );
}
