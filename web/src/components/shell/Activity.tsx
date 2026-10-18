import { Loader2, X } from "lucide-react";
import { api } from "@/api/client";
import type { Job } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Progress } from "@/components/ui/progress";
import { duration, progress, title, took } from "@/lib/format";
import { openJob } from "@/lib/open";
import { isActive, useApp } from "@/store/app";
import { useShallow } from "zustand/react/shallow";
import { OnMac } from "@/components/kit/MacTag";
import { t } from "@/lib/i18n";

/** The queue in the top bar: what runs and what waits, with progress and cancel. */
export function Activity() {
  const active = useApp(
    useShallow((s) =>
      Object.values(s.jobs)
        .filter(isActive)
        .sort((a, b) => (a.state === b.state ? a.created.localeCompare(b.created) : a.state === "running" ? -1 : 1)),
    ),
  );
  const running = active.find((j) => j.state === "running");
  // green only while the job stream is live (not while the Mac or the server is away)
  const live = useApp((s) => s.connection === "live");
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button variant="ghost" size="sm" className="gap-2 text-muted-foreground">
          {running ? <Loader2 className="size-4 animate-spin text-primary" /> : <span className={live ? "dot" : "dot idle"} />}
          {running ? (
            <span className="max-w-56 truncate text-foreground max-sm:max-w-24">
              {title(running)}
              {progress(running) != null && <span className="ml-1.5 text-muted-foreground tabular-nums">{Math.round(progress(running)! * 100)}%</span>}
            </span>
          ) : (
            t("Idle")
          )}
          {active.length > 1 && <span className="rounded-full bg-muted px-1.5 text-xs">+{active.length - 1}</span>}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-96 p-0">
        <div className="border-b px-4 py-2.5 text-sm font-medium">{t("Activity")}</div>
        {active.length === 0 ? (
          <div className="px-4 py-6 text-center text-sm text-muted-foreground">{t("Nothing running. One job runs on the GPU at a time; the rest waits here.")}</div>
        ) : (
          <div className="max-h-96 overflow-y-auto">
            {active.map((j) => (
              <ActivityRow key={j.id} job={j} />
            ))}
          </div>
        )}
      </PopoverContent>
    </Popover>
  );
}

function ActivityRow({ job: j }: { job: Job }) {
  const p = progress(j);
  return (
    <div className="flex items-start gap-3 border-b px-4 py-3 last:border-0">
      <button className="min-w-0 flex-1 text-left" onClick={() => openJob(j)}>
        <div className="truncate text-sm font-medium">{title(j)}</div>
        <div className="truncate text-xs text-muted-foreground">
          {j.state === "queued" ? t("waiting") : (j.status ?? t("starting …"))}
          {j.state === "running" && ` · ${duration(took(j))}`}
          <OnMac job={j} plain prefix=" · " />
        </div>
        {j.state === "running" && <Progress value={p != null ? p * 100 : undefined} className="mt-2 h-1" />}
      </button>
      <Button size="icon-sm" variant="ghost" title={t("Cancel")} onClick={() => api.cancel(j.id)}>
        <X />
      </Button>
    </div>
  );
}
