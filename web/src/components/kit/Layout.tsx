import { ChevronRight } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Label } from "@/components/ui/label";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";

/**
 * A create page: the composer on the left (its own scroll, the Create button pinned at its bottom), the results on
 * the right. On a phone they stack.
 */
export function CreateLayout({ composer, footer, children }: { composer: ReactNode; footer?: ReactNode; children: ReactNode }) {
  return (
    <div className="flex h-full min-h-0 max-lg:flex-col max-lg:overflow-y-auto">
      <aside className="flex w-[400px] shrink-0 flex-col border-r bg-card/40 max-lg:w-full max-lg:border-r-0 max-lg:border-b">
        <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto p-5 max-lg:overflow-visible">{composer}</div>
        {footer && <div className="border-t bg-card/60 p-4">{footer}</div>}
      </aside>
      <section className="min-h-0 min-w-0 flex-1 overflow-y-auto p-5 max-lg:overflow-visible">{children}</section>
    </div>
  );
}

/** A labelled control. `actions` sit at the right of the label (quiet links: "Fit to lyrics", "Random"). */
export function Field({ label, actions, hint, children, className }: { label: ReactNode; actions?: ReactNode; hint?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <div className={cn("flex flex-col gap-1.5", className)}>
      <div className="flex min-h-6 items-center gap-1">
        <Label className="text-xs font-semibold tracking-wide text-muted-foreground">{label}</Label>
        <div className="flex-1" />
        {actions}
      </div>
      {children}
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
}

/** Settings most people leave alone, folded away (open or closed is remembered per `id`). */
export function Advanced({ id, title = "Advanced", summary, children }: { id: string; title?: string; summary?: ReactNode; children: ReactNode }) {
  const [open, setOpen] = useState(() => local.get(`advanced.${id}`, false));
  return (
    <div className="rounded-lg border">
      <button
        type="button"
        onClick={() => (setOpen(!open), local.set(`advanced.${id}`, !open))}
        className="flex w-full items-center gap-2 px-3 py-2.5 text-left text-sm font-medium"
      >
        <ChevronRight className={cn("size-4 transition-transform", open && "rotate-90")} />
        {title}
        {!open && summary && <span className="ml-auto truncate text-xs font-normal text-muted-foreground">{summary}</span>}
      </button>
      {open && <div className="flex flex-col gap-4 border-t px-3 pt-3 pb-4">{children}</div>}
    </div>
  );
}

/** A results heading with filters or actions at the right. */
export function SectionHeader({ title, children }: { title: ReactNode; children?: ReactNode }) {
  return (
    <div className="mb-3 flex flex-wrap items-center gap-2">
      <h2 className="text-sm font-semibold">{title}</h2>
      <div className="flex-1" />
      {children}
    </div>
  );
}

/** An empty list, said kindly. */
export function Empty({ icon, title, children }: { icon?: ReactNode; title: string; children?: ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-2 rounded-xl border border-dashed px-6 py-14 text-center">
      {icon && <div className="text-muted-foreground [&_svg]:size-8">{icon}</div>}
      <div className="text-sm font-medium">{title}</div>
      {children && <div className="max-w-sm text-sm text-muted-foreground">{children}</div>}
    </div>
  );
}
