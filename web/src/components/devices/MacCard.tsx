import { Laptop, Plus } from "lucide-react";
import { Ago } from "@/components/connection/Ago";
import { Code } from "@/components/connection/ErrorNotice";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";
import { useSession } from "@/store/session";

/** The Mac that runs the jobs: online or not, and since when. Offline on the hub: `onConnect` offers "Connect a Mac". */
export function MacCard({ onConnect }: { onConnect?: () => void }) {
  const role = useApp((s) => s.role);
  const worker = useApp((s) => s.worker);
  const connection = useApp((s) => s.connection);
  const name = useSession((s) => s.info?.name);

  // against `mlxlab serve` the server is the Mac
  const local = role !== "hub";
  const online = local ? connection === "live" : !!worker?.connected;

  return (
    <Card className="flex-row flex-wrap items-center gap-x-4 gap-y-3 px-4" data-mac-card={online ? "online" : "offline"}>
      <div className={cn("grid size-10 shrink-0 place-items-center rounded-xl", online ? "bg-emerald-500/10 text-emerald-600 dark:text-emerald-400" : "bg-amber-500/10 text-amber-600 dark:text-amber-400")}>
        <Laptop className="size-5" />
      </div>
      <div className="min-w-0 flex-1 basis-56">
        <div className="flex items-center gap-2">
          <span className="truncate font-medium">{(local ? name : worker?.name) || "Your Mac"}</span>
          <span className={cn("inline-flex items-center gap-1.5 text-xs", online ? "text-emerald-600 dark:text-emerald-400" : "text-amber-600 dark:text-amber-400")}>
            <span className={cn("size-1.5 rounded-full", online ? "bg-emerald-500" : "bg-amber-500")} />
            {online ? "Online" : "Offline"}
          </span>
        </div>
        <div className="mt-0.5 text-xs text-muted-foreground">
          {local ? (
            online ? (
              "This Mac runs the models and serves the app itself (mlxlab serve)."
            ) : (
              "Reconnecting to mlxlab serve …"
            )
          ) : online ? (
            <>
              Runs your jobs · connected <Ago iso={worker?.since} />
            </>
          ) : (
            <>
              {worker?.since && (
                <>
                  Away since <Ago iso={worker.since} />.{" "}
                </>
              )}
              Jobs wait until it is back. Start it with <Code>mlxlab worker</Code>.
            </>
          )}
        </div>
      </div>
      {!local && !online && onConnect && (
        <Button variant="outline" onClick={onConnect} className="max-sm:w-full">
          <Plus /> Connect a Mac
        </Button>
      )}
    </Card>
  );
}
