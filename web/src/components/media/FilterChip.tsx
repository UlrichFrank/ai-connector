import type { ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

/** A filter of a media grid ("All", "★ Starred", "Images 12"). */
export function FilterChip({ on, onClick, count, children }: { on: boolean; onClick: () => void; count?: number; children: ReactNode }) {
  return (
    <Button
      variant={on ? "secondary" : "ghost"}
      size="sm"
      aria-pressed={on}
      className={cn("rounded-full", !on && "text-muted-foreground")}
      onClick={onClick}
    >
      {children}
      {count != null && count > 0 && <span className="text-muted-foreground tabular-nums">{count}</span>}
    </Button>
  );
}
