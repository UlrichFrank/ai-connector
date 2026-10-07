import { Cpu, WifiOff } from "lucide-react";
import { api } from "@/api/client";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { useApp } from "@/store/app";
import { MacStatus } from "@/components/connection/MacStatus";
import { Activity } from "./Activity";

/** Above every page: the page's title, what is in memory, and the queue. */
export function TopBar({ title }: { title: string }) {
  // the server itself is away (not just the Mac: the Mac pill says that)
  const lost = useApp((s) => s.connection === "offline" && s.worker?.connected !== false);
  return (
    <header className="flex h-12 shrink-0 items-center gap-2 overflow-hidden border-b px-4 max-sm:gap-1 max-sm:px-3">
      <h1 className="shrink-0 text-sm font-semibold">{title}</h1>
      <div className="flex-1" />
      <MacStatus />
      {lost && (
        <span className="flex items-center gap-1.5 rounded-md bg-destructive/15 px-2 py-1 text-xs text-destructive">
          <WifiOff className="size-3.5" /> reconnecting …
        </span>
      )}
      <Memory />
      <Activity />
    </header>
  );
}

function Memory() {
  const loaded = useApp((s) => s.loaded);
  const models = useApp((s) => s.models);
  const refresh = useApp((s) => s.refreshModels);
  const live = Object.values(loaded).filter(Boolean);
  if (!live.length) return null;
  const name = (id: string) => models.find((m) => m.id === id)?.name ?? id;
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button variant="ghost" size="sm" className="gap-1.5 text-muted-foreground">
          <Cpu className="size-4" />
          <span className="max-w-48 truncate max-md:hidden">{live.map(name).join(", ")}</span>
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-72">
        <div className="mb-1 text-sm font-medium">In memory</div>
        <ul className="mb-3 text-sm text-muted-foreground">
          {live.map((id) => (
            <li key={id}>{name(id)}</li>
          ))}
        </ul>
        <Button
          size="sm"
          variant="secondary"
          className="w-full"
          onClick={async () => {
            await api.unload();
            setTimeout(refresh, 300);
          }}
        >
          Unload all
        </Button>
      </PopoverContent>
    </Popover>
  );
}
