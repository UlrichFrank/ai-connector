import { CircleCheck, CircleX, Loader2, Trash2 } from "lucide-react";
import { useMemo, type ReactNode } from "react";
import { OfflineError } from "@/api/client";
import type { Job, Kind } from "@/api/types";
import { ErrorNotice } from "@/components/connection/ErrorNotice";
import { DeleteJob } from "@/components/kit/JobBits";
import { SectionHeader } from "@/components/kit/Layout";
import { Button } from "@/components/ui/button";
import { ago, duration } from "@/lib/format";
import { navigate } from "@/lib/route";
import { cn } from "@/lib/utils";
import { isActive, jobsOf, useApp } from "@/store/app";

/** The runs of a lab page, newest first, and the one to show: the one in the URL, else the newest. */
export function useRuns(kind: Kind, arg: string) {
  const jobs = useApp((s) => s.jobs);
  const runs = useMemo(() => jobsOf(jobs, kind), [jobs, kind]);
  const current = arg ? jobs[arg] : runs[0];
  return { runs, current, missing: !!arg && !jobs[arg] };
}

/** Why the page has nothing to show when the Mac or the server is away (null when connected). */
export function ConnectionNotice({ className }: { className?: string }) {
  const connection = useApp((s) => s.connection);
  const refresh = useApp((s) => s.refreshJobs);
  if (connection === "live" || connection === "connecting") return null;
  const error = connection === "waiting" ? new OfflineError() : new TypeError("unreachable");
  return <ErrorNotice error={error} onRetry={() => void refresh()} className={className} />;
}

/** The earlier runs, to open again; the shown one is highlighted. */
export function RunHistory({ runs, current, page, summary }: { runs: Job[]; current?: Job; page: string; summary: (j: Job) => { title: string; meta: string } }) {
  const models = useApp((s) => s.models);
  if (runs.length < 1) return null;
  return (
    <section className="mt-10">
      <SectionHeader title={`Earlier runs · ${runs.length}`} />
      <ul className="flex flex-col gap-1">
        {runs.map((j) => {
          const s = summary(j);
          const on = j.id === current?.id;
          const model = models.find((m) => m.id === j.model)?.name ?? j.model;
          return (
            <li key={j.id} className={cn("group flex items-center gap-1 rounded-lg border border-transparent pr-1 transition-colors hover:bg-accent/50", on && "border-border bg-accent/60")}>
              <button type="button" onClick={() => navigate(page, j.id)} className="flex min-w-0 flex-1 items-center gap-3 px-3 py-2 text-left" aria-current={on || undefined}>
                <StateIcon job={j} />
                <span className="min-w-0 flex-1">
                  <span className="block truncate text-sm font-medium">{s.title}</span>
                  <span className="block truncate text-xs text-muted-foreground">
                    {s.meta} · {model}
                    {j.finished && j.started ? ` · ${duration((new Date(j.finished).getTime() - new Date(j.started).getTime()) / 1000)}` : ""}
                  </span>
                </span>
                <span className="shrink-0 text-xs text-muted-foreground tabular-nums">{ago(j.created)}</span>
              </button>
              <DeleteJob
                job={j}
                after={() => on && navigate(page)}
                trigger={(open) => (
                  <Button variant="ghost" size="icon-sm" aria-label="Delete this run" className="text-muted-foreground opacity-60 hover:text-destructive sm:opacity-0 sm:group-hover:opacity-100 focus-visible:opacity-100" onClick={open}>
                    <Trash2 />
                  </Button>
                )}
              />
            </li>
          );
        })}
      </ul>
    </section>
  );
}

function StateIcon({ job: j }: { job: Job }) {
  if (isActive(j)) return <Loader2 className="size-4 shrink-0 animate-spin text-primary" />;
  if (j.state === "done") return <CircleCheck className="size-4 shrink-0 text-muted-foreground" />;
  return <CircleX className="size-4 shrink-0 text-destructive" />;
}

/** A small "label value" pair in a result's header. */
export function Stat({ label, children }: { label: string; children: ReactNode }) {
  return (
    <span className="flex items-baseline gap-1.5">
      <span className="text-muted-foreground">{label}</span>
      <span className="font-medium tabular-nums">{children}</span>
    </span>
  );
}
