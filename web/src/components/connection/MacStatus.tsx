import { CloudOff, Laptop } from "lucide-react";
import { Code } from "@/components/connection/ErrorNotice";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { ago } from "@/lib/format";
import { navigate } from "@/lib/route";
import { t } from "@/lib/i18n";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";

/** In the top bar on the hub: whether the Mac is there. Calm when it is, amber when it is not. Opens Devices. */
export function MacStatus() {
  const worker = useApp((s) => s.worker);
  const role = useApp((s) => s.role);
  const macs = useApp((s) => s.macs);
  if (role !== "hub") return null;
  const online = !!worker?.connected;
  const many = online && macs.length > 1;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button
          onClick={() => navigate("devices")}
          className={cn(
            "flex h-7 items-center gap-1.5 rounded-full px-2.5 text-xs font-medium transition-colors",
            online
              ? "text-muted-foreground hover:bg-muted hover:text-foreground"
              : "bg-amber-500/15 text-amber-700 hover:bg-amber-500/25 dark:text-amber-400",
          )}
        >
          {online ? <span className="size-1.5 rounded-full bg-emerald-500" /> : <CloudOff className="size-3.5" />}
          {many ? (
            <span data-macs-online={macs.length}>
              {macs.length} Macs<span className="max-sm:hidden"> online</span>
            </span>
          ) : online ? (
            <span>
              Mac<span className="max-sm:hidden"> online</span>
            </span>
          ) : (
            <span>Mac offline</span>
          )}
        </button>
      </TooltipTrigger>
      <TooltipContent side="bottom" align="end" className="block max-w-64 py-2 leading-relaxed">
        {many ? (
          <span className="flex flex-col gap-1">
            {macs.map((m) => (
              <span key={m.id} className="flex items-center gap-1.5">
                <Laptop className="size-3.5 shrink-0" />
                <span className="truncate">{m.name || "Mac"}</span>
                {m.since && <span className="shrink-0 opacity-70">· {t("connected")} {ago(m.since)}</span>}
              </span>
            ))}
          </span>
        ) : online ? (
          <span className="flex items-center gap-1.5">
            <Laptop className="size-3.5" />
            {worker?.name || t("The Mac")} · {t("connected")} {ago(worker?.since)}
          </span>
        ) : (
          <>
            {t("Jobs wait until the Mac is back. Start it with")} <Code className="bg-background/15 text-background">mlxlab worker</Code>
            {t(", or click to connect a Mac.")}
          </>
        )}
      </TooltipContent>
    </Tooltip>
  );
}
