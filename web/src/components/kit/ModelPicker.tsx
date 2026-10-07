import { Check, ChevronDown, Download, Laptop, Lock, Search } from "lucide-react";
import { useEffect, useMemo } from "react";
import type { Kind, Model, ModelWorker } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { bytes } from "@/lib/format";
import { fitsNowhere, noFitReason, rankWorkers, whereText } from "@/lib/macs";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";
import { useState } from "react";

/**
 * The models of a kind to make something with. One is the usual choice; ticking more makes a version with each
 * ("Create 2 versions"). The choice is remembered per kind (and per `scope`, e.g. a studio task).
 */
export function useModelChoice(kind: Kind, scope = "", filter?: (m: Model) => boolean) {
  const all = useApp((s) => s.models);
  const models = useMemo(() => all.filter((m) => m.kind === kind && (!filter || filter(m))), [all, kind, filter]);
  const key = `models.${kind}${scope ? "." + scope : ""}`;
  const [chosen, setChosen] = useState<string[]>(() => local.get<string[]>(key, []));
  // keep only models that exist and that a connected Mac has the memory for; without a choice, the first one on disk
  const usable = models.filter((m) => !fitsNowhere(m));
  const valid = chosen.filter((id) => usable.some((m) => m.id === id));
  const ids = valid.length ? valid : usable.length ? [(usable.find((m) => m.here) ?? usable[0]).id] : [];
  useEffect(() => local.set(key, chosen), [key, chosen]);
  return { models, ids, set: setChosen, primary: models.find((m) => m.id === ids[0]) };
}

/**
 * "Non-commercial": a model whose licence does not allow commercial use (e.g. CC BY-NC 4.0); the licence in the
 * tooltip. `compact` (the picker's trigger) is a little smaller.
 */
export function NonCommercialBadge({ licence, compact, className }: { licence?: string; compact?: boolean; className?: string }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          aria-label={`Non-commercial${licence ? ` (${licence})` : ""}`}
          className={cn(
            "inline-flex h-[18px] shrink-0 items-center rounded-full border border-amber-500/35 bg-amber-500/12 px-1.5 font-medium whitespace-nowrap text-amber-700 dark:text-amber-300",
            compact ? "text-[10px]" : "text-[11px]",
            className,
          )}
        >
          Non-commercial
        </span>
      </TooltipTrigger>
      <TooltipContent side="top" className="max-w-64 flex-col items-start gap-0.5">
        <span className="font-medium">Not for commercial use</span>
        <span className="opacity-80">Licence: {licence || "non-commercial"}. What you make with it may not be used commercially.</span>
      </TooltipContent>
    </Tooltip>
  );
}

export function ModelPicker({
  choice,
  multiple = true,
  className,
}: {
  choice: ReturnType<typeof useModelChoice>;
  multiple?: boolean;
  className?: string;
}) {
  const { models, ids, set } = choice;
  const label = ids.length === 0 ? "No model" : ids.length === 1 ? (models.find((m) => m.id === ids[0])?.name ?? ids[0]) : `${ids.length} models`;
  // the badge in the trigger when any chosen model is not for commercial use
  const nc = models.filter((m) => ids.includes(m.id) && m.nonCommercial);
  const [query, setQuery] = useState("");
  const q = query.trim().toLowerCase();
  const shown = q ? models.filter((m) => `${m.name} ${m.id} ${m.about}`.toLowerCase().includes(q)) : models;
  const toggle = (id: string) => {
    if (!multiple) return set([id]);
    set(ids.includes(id) ? (ids.length > 1 ? ids.filter((x) => x !== id) : ids) : [...ids, id]);
  };
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button variant="outline" className={cn("h-9 max-w-full justify-between gap-2 font-medium", className)} data-model-picker="">
          <span className={cn("size-2 shrink-0 rounded-full", choice.primary?.here ? "bg-emerald-500" : "bg-amber-500")} />
          <span className="truncate">{label}</span>
          {nc.length > 0 && <NonCommercialBadge licence={nc.map((m) => (nc.length > 1 ? `${m.name}: ${m.licence ?? "non-commercial"}` : m.licence)).join(" · ")} compact />}
          <ChevronDown className="ml-auto size-4 opacity-60" />
        </Button>
      </PopoverTrigger>
      {/* never taller than the room the popover has: long lists scroll inside it (they ran off the window) */}
      <PopoverContent
        align="start"
        collisionPadding={12}
        className="flex max-h-[min(34rem,var(--radix-popover-content-available-height))] w-80 flex-col p-1"
        onOpenAutoFocus={(e) => models.length <= 7 && e.preventDefault()}
      >
        {models.length > 7 && (
          <div className="relative shrink-0 px-1 pt-1 pb-1.5">
            <Search className="pointer-events-none absolute top-1/2 left-3.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
            <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search models" className="h-8 pl-8 text-sm" />
          </div>
        )}
        {multiple && <div className="shrink-0 px-2 pt-1.5 pb-1 text-xs text-muted-foreground">Tick more than one to make a version with each.</div>}
        <div className="min-h-0 overflow-y-auto overscroll-contain">
        {shown.map((m) => {
          const on = ids.includes(m.id);
          const why = noFitReason(m);
          return (
            <button
              key={m.id}
              onClick={() => toggle(m.id)}
              disabled={!!why}
              title={why || undefined}
              data-model={m.id}
              className={cn(
                "flex w-full items-start gap-2.5 rounded-md px-2 py-1.5 text-left hover:bg-accent disabled:cursor-not-allowed disabled:hover:bg-transparent",
                on && "bg-accent/60",
              )}
            >
              <span className={cn("mt-0.5 grid size-4 shrink-0 place-items-center rounded border", on && "border-primary bg-primary text-primary-foreground", !multiple && "rounded-full")}>
                {on && <Check className="size-3" />}
              </span>
              <span className="min-w-0 flex-1">
                <span className={cn("flex flex-wrap items-center gap-1.5 text-sm font-medium", why && "text-muted-foreground")}>
                  {m.name}
                  {m.gated && !m.here && <Lock className="size-3 text-muted-foreground" />}
                  {m.nonCommercial && <NonCommercialBadge licence={m.licence} />}
                </span>
                <span title={m.about} className={cn("line-clamp-1 text-xs text-muted-foreground", why && "opacity-60")}>{m.about}</span>
                {why ? (
                  <span className="mt-1 block text-xs font-medium text-destructive/90">{why}</span>
                ) : (
                  m.workers && m.workers.length > 1 && <Where workers={m.workers} />
                )}
              </span>
              <span className={cn("shrink-0 text-xs text-muted-foreground tabular-nums", why && "opacity-60")}>
                {m.here ? (
                  bytes(m.size)
                ) : (
                  <span className="flex items-center gap-1 text-amber-600 dark:text-amber-400">
                    <Download className="size-3" />
                    {bytes(m.size)}
                  </span>
                )}
              </span>
            </button>
          );
        })}
        {!shown.length && <div className="px-2 py-3 text-sm text-muted-foreground">{models.length ? "No model matches." : "No models of this kind."}</div>}
        </div>
      </PopoverContent>
    </Popover>
  );
}

/** Where a model can run when several Macs are connected: "Mac Studio · loaded", "MacBook · too little memory". */
function Where({ workers }: { workers: ModelWorker[] }) {
  return (
    <span className="mt-1 flex flex-col gap-0.5" data-model-where="">
      {rankWorkers(workers).map((w) => (
        <span key={w.id} className={cn("flex min-w-0 items-center gap-1.5 text-[11px]", w.fits ? "text-muted-foreground" : "text-muted-foreground/60")}>
          <Laptop className="size-3 shrink-0" />
          <span className="truncate">{w.name || "Mac"}</span>
          <span className="shrink-0">·</span>
          <span
            className={cn(
              "shrink-0",
              !w.fits ? "text-destructive/75" : w.loaded ? "text-emerald-600 dark:text-emerald-400" : !w.here && "text-amber-600 dark:text-amber-400",
            )}
          >
            {whereText(w)}
          </span>
        </span>
      ))}
    </span>
  );
}
