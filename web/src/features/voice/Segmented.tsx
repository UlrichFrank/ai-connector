import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

/** A row of choices of which one is on (the look of the image page's precision switch). */
export function Segmented<T extends string>({
  value,
  onChange,
  options,
  className,
}: {
  value: T;
  onChange: (v: T) => void;
  options: { id: T; label: ReactNode; title?: string }[];
  className?: string;
}) {
  return (
    <div role="radiogroup" className={cn("grid gap-1 rounded-lg bg-muted/60 p-1", className)} style={{ gridTemplateColumns: `repeat(${options.length}, minmax(0, 1fr))` }}>
      {options.map((o) => (
        <button
          key={o.id}
          type="button"
          role="radio"
          aria-checked={value === o.id}
          title={o.title}
          onClick={() => onChange(o.id)}
          className={cn(
            "flex min-w-0 items-center justify-center gap-1.5 rounded-md px-2 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground [&_svg]:size-3.5 [&_svg]:shrink-0",
            value === o.id && "bg-background text-foreground shadow-sm ring-1 ring-foreground/10",
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
