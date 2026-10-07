import { Check, ChevronDown, Library, Sparkles, UserRound } from "lucide-react";
import { useState } from "react";
import type { Model, Voice } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";
import { PreviewButton } from "./Preview";
import { useVoiceLibrary } from "./voices";

/** "cloned" / "designed": where a library voice comes from. */
export function VoiceKind({ voice, className }: { voice: Pick<Voice, "kind">; className?: string }) {
  const clone = voice.kind === "clone";
  return (
    <span
      className={cn(
        "inline-flex h-[18px] shrink-0 items-center gap-1 rounded-full border px-1.5 text-[11px] font-medium whitespace-nowrap",
        clone ? "border-sky-500/30 bg-sky-500/10 text-sky-700 dark:text-sky-300" : "border-violet-500/30 bg-violet-500/10 text-violet-700 dark:text-violet-300",
        className,
      )}
    >
      {clone ? <UserRound className="size-3" /> : <Sparkles className="size-3" />}
      {clone ? "cloned" : "designed"}
    </span>
  );
}

/**
 * Which voice speaks: the model's own voice, one of the library (cloned or designed; ▶ plays its sample), or one of
 * the model's named presets. `value` is a library id or a preset name ("" = the model's default).
 */
export function VoicePicker({ value, onChange, model, onManage, className }: { value: string; onChange: (v: string) => void; model?: Model; onManage?: () => void; className?: string }) {
  const { voices, presets } = useVoiceLibrary();
  const [open, setOpen] = useState(false);
  const own = model ? (presets[model.id] ?? []) : [];
  const lib = voices.find((v) => v.id === value);
  const pick = (v: string) => (onChange(v), setOpen(false));

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button variant="outline" className={cn("h-9 w-full justify-between gap-2 font-medium", className)}>
          <span className="truncate">{lib ? lib.name : value || "Default voice"}</span>
          {lib && <VoiceKind voice={lib} />}
          {!lib && value && <span className="text-xs font-normal text-muted-foreground">preset</span>}
          <ChevronDown className="ml-auto size-4 opacity-60" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="flex max-h-[min(70vh,520px)] w-(--radix-popover-trigger-width) min-w-72 flex-col p-0">
        <div className="min-h-0 flex-1 overflow-y-auto p-1">
          <Row on={!value} onClick={() => pick("")} title="Default voice" sub={model ? `${model.name} picks its own` : "The model's own"} />

          <Group title="Your voices">
            {voices.map((v) => (
              <Row
                key={v.id}
                on={value === v.id}
                onClick={() => pick(v.id)}
                title={v.name}
                badge={<VoiceKind voice={v} />}
                sub={v.kind === "design" ? v.description : v.transcript ? `“${v.transcript}”` : undefined}
                extra={v.sample ? <PreviewButton url={v.sample} /> : undefined}
              />
            ))}
            {!voices.length && <p className="px-2 py-1.5 text-xs text-muted-foreground">None yet. Clone a voice from a recording, or design one from a description.</p>}
          </Group>

          {own.length > 0 && (
            <Group title={`${model?.name ?? "Model"} voices`}>
              <div className="grid grid-cols-2 gap-0.5">
                {own.map((p) => (
                  <Row key={p} on={value === p} onClick={() => pick(p)} title={p} />
                ))}
              </div>
            </Group>
          )}
        </div>
        {onManage && (
          <div className="border-t p-1">
            <Button variant="ghost" size="sm" className="w-full justify-start" onClick={() => (setOpen(false), onManage())}>
              <Library /> Manage voices …
            </Button>
          </div>
        )}
      </PopoverContent>
    </Popover>
  );
}

function Group({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="mt-1 border-t pt-1">
      <div className="px-2 pt-1 pb-1 text-[11px] font-medium tracking-wide text-muted-foreground uppercase">{title}</div>
      {children}
    </div>
  );
}

function Row({ on, onClick, title, sub, badge, extra }: { on: boolean; onClick: () => void; title: string; sub?: string; badge?: React.ReactNode; extra?: React.ReactNode }) {
  return (
    <div className={cn("flex items-center gap-1 rounded-md hover:bg-accent", on && "bg-accent/60")}>
      <button type="button" onClick={onClick} className="flex min-w-0 flex-1 items-start gap-2 px-2 py-1.5 text-left">
        <Check className={cn("mt-0.5 size-4 shrink-0 text-primary", !on && "invisible")} />
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-1.5 text-sm font-medium">
            <span className="truncate">{title}</span>
            {badge}
          </span>
          {sub && <span className="line-clamp-1 text-xs text-muted-foreground">{sub}</span>}
        </span>
      </button>
      {extra && <div className="pr-1">{extra}</div>}
    </div>
  );
}
