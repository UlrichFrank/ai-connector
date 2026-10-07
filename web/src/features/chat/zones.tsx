// The zones a chat can use (docs/chat-tools.md): chosen when a chat starts, fixed for the conversation.

import { Check, ChevronDown, FileText, Globe, House, Lock, MessageSquare, type LucideIcon } from "lucide-react";
import type { Zone } from "@/api/types";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";

export interface ZoneInfo {
  zone: Zone;
  name: string;
  icon: LucideIcon;
  /** What it does, in one line. */
  blurb: string;
  /** What it can never do. */
  never?: string;
  /** Text and soft background in the zone's colour. */
  tone: string;
  soft: string;
}

export const zones: ZoneInfo[] = [
  { zone: "", name: "Plain", icon: MessageSquare, blurb: "Just the model, no tools", tone: "text-muted-foreground", soft: "bg-muted" },
  {
    zone: "home",
    name: "Home",
    icon: House,
    blurb: "Smart home and vboard",
    never: "Never sees web content",
    tone: "text-teal-700 dark:text-teal-300",
    soft: "bg-teal-500/10",
  },
  {
    zone: "web",
    name: "Web",
    icon: Globe,
    blurb: "Search and read the web in a sandbox",
    never: "Can't reach your home",
    tone: "text-sky-700 dark:text-sky-300",
    soft: "bg-sky-500/10",
  },
  {
    zone: "files",
    name: "Files",
    icon: FileText,
    blurb: "Write PDFs, documents, charts",
    never: "Offline: no network at all",
    tone: "text-indigo-700 dark:text-indigo-300",
    soft: "bg-indigo-500/10",
  },
];

export const zoneInfo = (z: Zone | undefined): ZoneInfo => zones.find((i) => i.zone === (z ?? "")) ?? zones[0];

/** The zone of a conversation, as a small badge (nothing for a plain chat). */
export function ZoneBadge({ zone, className, compact }: { zone: Zone | undefined; className?: string; compact?: boolean }) {
  if (!zone) return null;
  const z = zoneInfo(zone);
  const Icon = z.icon;
  return (
    <span
      className={cn("inline-flex h-5 shrink-0 items-center gap-1 rounded-full px-1.5 text-[11px] font-medium", z.tone, z.soft, compact && "size-5 justify-center px-0", className)}
      title={`${z.name} chat: ${z.blurb.toLowerCase()}${z.never ? ". " + z.never + "." : ""}`}
    >
      <Icon className="size-3" />
      {!compact && z.name}
    </span>
  );
}

/** The tools of a new chat, as cards (the empty state). */
export function ZoneCards({ value, onChange }: { value: Zone; onChange: (z: Zone) => void }) {
  return (
    <div className="flex w-full max-w-2xl flex-col gap-2">
      <div className="text-xs font-medium text-muted-foreground">Tools for this chat</div>
      <div role="radiogroup" aria-label="Tools for this chat" className="grid grid-cols-2 gap-2 sm:grid-cols-4">
        {zones.map((z) => {
          const on = z.zone === value;
          const Icon = z.icon;
          return (
            <button
              key={z.name}
              role="radio"
              aria-checked={on}
              onClick={() => onChange(z.zone)}
              className={cn(
                "relative flex flex-col gap-1 rounded-xl border bg-card/40 px-3 py-2.5 text-left transition-colors hover:bg-accent",
                on && "border-primary/60 bg-primary/5 ring-1 ring-primary/30 hover:bg-primary/5",
              )}
            >
              <span className="flex items-center gap-1.5 text-sm font-medium">
                <span className={cn("grid size-6 place-items-center rounded-md", z.soft, z.tone)}>
                  <Icon className="size-3.5" />
                </span>
                {z.name}
                {on && <Check className="ml-auto size-3.5 text-primary" />}
              </span>
              <span className="text-xs leading-snug text-muted-foreground">{z.blurb}</span>
              {z.never && <span className="text-[11px] leading-snug text-muted-foreground/80">{z.never}</span>}
            </button>
          );
        })}
      </div>
    </div>
  );
}

/** The zone in the composer: a menu for a new chat, a lock in a conversation. */
export function ZonePicker({ value, onChange, locked, onNewChat }: { value: Zone; onChange: (z: Zone) => void; locked: boolean; onNewChat: () => void }) {
  const z = zoneInfo(value);
  const Icon = z.icon;
  const label = (
    <>
      <Icon className={cn("size-3.5", value && z.tone)} />
      <span className="max-sm:hidden">{value ? z.name : "No tools"}</span>
    </>
  );
  const cls = "h-7 gap-1.5 px-2 text-[0.8rem] font-normal text-muted-foreground hover:text-foreground";
  if (locked)
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <Button variant="ghost" size="sm" className={cls} onClick={onNewChat} aria-label={`${z.name} chat. Start a new chat to use other tools`}>
            {label}
            <Lock className="size-3 opacity-60" />
          </Button>
        </TooltipTrigger>
        <TooltipContent className="max-w-60">
          {value ? `This chat uses ${z.name} tools (${z.blurb.toLowerCase()}).` : "This chat has no tools."} Start a new chat to use other tools.
        </TooltipContent>
      </Tooltip>
    );
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="sm" className={cls} title="Tools for this chat">
          {label}
          <ChevronDown className="size-3 opacity-60 max-sm:hidden" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="w-72">
        <DropdownMenuLabel className="text-xs font-normal text-muted-foreground">Tools for this chat (fixed once it starts)</DropdownMenuLabel>
        <DropdownMenuSeparator />
        {zones.map((o) => {
          const I = o.icon;
          return (
            <DropdownMenuItem key={o.name} onSelect={() => onChange(o.zone)} className="items-start gap-2.5 py-2">
              <span className={cn("mt-0.5 grid size-6 shrink-0 place-items-center rounded-md", o.soft, o.tone)}>
                <I className="size-3.5" />
              </span>
              <span className="flex min-w-0 flex-1 flex-col">
                <span className="text-sm">{o.name}</span>
                <span className="text-xs text-muted-foreground">
                  {o.blurb}
                  {o.never && ` · ${o.never.charAt(0).toLowerCase() + o.never.slice(1)}`}
                </span>
              </span>
              {o.zone === value && <Check className="mt-1 size-3.5 text-primary" />}
            </DropdownMenuItem>
          );
        })}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
