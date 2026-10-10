import { Activity, CircleX, Clock, ListX, Loader2, Server } from "lucide-react";
import { useMemo, useState, type ComponentType } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Job, Worker } from "@/api/types";
import { Dot, Empty, Page, PageHeader } from "@/components/common";
import { StateBadge } from "@/components/StateBadge";
import { Button } from "@/components/ui/button";
import { age } from "@/lib/format";
import { canCancel, isActive, jobTitle, progress, queueStats, sortJobs } from "@/lib/jobs";
import { useNow } from "@/lib/now";
import { navigate } from "@/lib/route";
import { loadText, loadedModels, platformOf, specLine } from "@/lib/specs";
import { cn } from "@/lib/utils";
import { useLive } from "@/store/live";
import { PlatformIcon } from "./Workers";

/** Finished jobs shown under the active ones. */
const RECENT = 50;

export function OverviewPage() {
  const jobs = useLive((s) => s.jobs);
  const workers = useLive((s) => s.workers);
  const connection = useLive((s) => s.connection);
  const list = useMemo(() => sortJobs(Object.values(jobs)), [jobs]);
  const stats = queueStats(list);
  const connected = workers.filter((w) => w.connected);

  return (
    <Page wide>
      <PageHeader title="Overview" description="The connected workers and the queue of every worker, live." />

      <div className="mb-6 grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Tile icon={Server} label="Workers connected" value={`${connected.length}`} sub={workers.length ? `of ${workers.length} enrolled` : "none enrolled"} onClick={() => navigate("workers")} />
        <Tile icon={Activity} label="Running" value={`${stats.running}`} tone={stats.running ? "primary" : undefined} />
        <Tile icon={Clock} label="Queued" value={`${stats.queued}`} />
        <Tile icon={CircleX} label="Failed" value={`${stats.failed}`} sub="in the job history" tone={stats.failed ? "bad" : undefined} />
      </div>

      <section className="mb-8">
        <h3 className="mb-2 text-sm font-medium text-muted-foreground">Workers</h3>
        {connected.length ? (
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            {connected.map((w) => (
              <WorkerTile key={w.id} w={w} />
            ))}
          </div>
        ) : (
          <Empty icon={Server} title="No worker is connected">
            Jobs run on workers: Macs, Linux and Windows PCs.{" "}
            <button className="font-medium text-primary hover:underline" onClick={() => navigate("workers", "connect")}>
              Connect a worker
            </button>
          </Empty>
        )}
      </section>

      <Queue list={list} live={connection === "live"} />
    </Page>
  );
}

function Tile({ icon: Icon, label, value, sub, tone, onClick }: { icon: ComponentType<{ className?: string }>; label: string; value: string; sub?: string; tone?: "primary" | "bad"; onClick?: () => void }) {
  const Comp = onClick ? "button" : "div";
  return (
    <Comp onClick={onClick} className={cn("flex flex-col gap-1 rounded-xl border bg-card p-4 text-left", onClick && "transition-colors hover:bg-muted/50")}>
      <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
        <Icon className="size-3.5" /> {label}
      </span>
      <span className={cn("text-2xl font-semibold tabular-nums", tone === "primary" && "text-primary", tone === "bad" && "text-destructive")}>{value}</span>
      {sub && <span className="text-xs text-muted-foreground">{sub}</span>}
    </Comp>
  );
}

function WorkerTile({ w }: { w: Worker }) {
  const busy = !!w.state?.running;
  const loaded = loadedModels(w);
  return (
    <button onClick={() => navigate("workers", w.id)} className="flex flex-col gap-1.5 rounded-xl border bg-card p-4 text-left transition-colors hover:bg-muted/50">
      <div className="flex items-center gap-2">
        <PlatformIcon platform={platformOf(w.specs)} />
        <span className="min-w-0 flex-1 truncate font-medium">{w.name}</span>
        {w.primary && <span className="rounded bg-primary/15 px-1.5 text-[11px] font-medium text-primary">primary</span>}
        <Dot tone={busy ? "busy" : "ok"} pulse={busy} />
      </div>
      <div className="truncate text-xs text-muted-foreground">{specLine(w) || "specs not known yet"}</div>
      <div className="flex items-center justify-between gap-2 text-xs">
        <span>{loadText(w)}</span>
        {loaded.length > 0 && <span className="truncate text-muted-foreground" title={loaded.join(", ")}>{loaded.join(", ")}</span>}
      </div>
    </button>
  );
}

function Queue({ list, live }: { list: Job[]; live: boolean }) {
  useNow(5000);
  const [all, setAll] = useState(false);
  const active = list.filter(isActive);
  const rest = list.filter((j) => !isActive(j));
  const shown = all ? [...active, ...rest.slice(0, RECENT)] : active;
  return (
    <section>
      <div className="mb-2 flex items-center gap-2">
        <h3 className="flex-1 text-sm font-medium text-muted-foreground">Queue {live ? "" : "(not live)"}</h3>
        <div role="tablist" className="flex rounded-md bg-muted p-0.5 text-xs">
          {[
            [false, `Active (${active.length})`],
            [true, "With history"],
          ].map(([v, label]) => (
            <button key={String(v)} role="tab" aria-selected={all === v} onClick={() => setAll(v as boolean)} className={cn("rounded px-2 py-1 text-muted-foreground", all === v && "bg-background text-foreground shadow-sm")}>
              {label}
            </button>
          ))}
        </div>
      </div>
      {shown.length ? (
        <div className="overflow-x-auto rounded-xl border">
          <table className="w-full text-sm">
            <thead className="bg-muted/40 text-left text-xs text-muted-foreground">
              <tr>
                <th className="px-3 py-2 font-medium">Job</th>
                <th className="px-3 py-2 font-medium">State</th>
                <th className="px-3 py-2 font-medium max-sm:hidden">Worker</th>
                <th className="px-3 py-2 font-medium max-md:hidden">Client</th>
                <th className="px-3 py-2 text-right font-medium">Age</th>
                <th className="w-0 px-2 py-2" />
              </tr>
            </thead>
            <tbody>
              {shown.map((j) => (
                <JobRow key={j.id} j={j} />
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <Empty icon={ListX} title={all ? "No jobs yet" : "Nothing queued or running"}>
          {all ? "Jobs from the mlxlab app and from clients (the OpenAI API, the native jobs API) show up here." : "Switch to “With history” to see finished jobs."}
        </Empty>
      )}
    </section>
  );
}

function JobRow({ j }: { j: Job }) {
  const [busy, setBusy] = useState(false);
  const p = progress(j);
  const cancel = async () => {
    setBusy(true);
    try {
      await api.cancel(j.id);
      toast.success("Cancelled", { description: jobTitle(j) });
    } catch (e) {
      toast.error("Could not cancel", { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <tr className="border-t align-top" data-job={j.id} data-state={j.state}>
      <td className="max-w-0 px-3 py-2">
        <div className="flex items-center gap-1.5">
          <span className="truncate font-medium" title={j.model}>
            {j.model}
          </span>
          <span className="shrink-0 rounded bg-muted px-1.5 text-[11px] text-muted-foreground">{j.kind}</span>
        </div>
        <div className="truncate text-xs text-muted-foreground" title={jobTitle(j)}>
          {j.state === "failed" && j.error ? <span className="text-destructive">{j.error}</span> : j.status && isActive(j) ? j.status : jobTitle(j)}
        </div>
        {j.state === "running" && p !== undefined && (
          <div className="mt-1 h-1 overflow-hidden rounded bg-muted">
            <div className="h-full bg-primary transition-all" style={{ width: `${Math.round(p * 100)}%` }} />
          </div>
        )}
      </td>
      <td className="px-3 py-2 whitespace-nowrap">
        <StateBadge state={j.state} />
      </td>
      <td className="px-3 py-2 whitespace-nowrap text-muted-foreground max-sm:hidden">{j.worker?.name ?? "—"}</td>
      <td className="px-3 py-2 whitespace-nowrap max-md:hidden">{j.client ? <span className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">{j.client}</span> : <span className="text-muted-foreground">—</span>}</td>
      <td className="px-3 py-2 text-right whitespace-nowrap text-muted-foreground tabular-nums" title={new Date(j.created).toLocaleString()}>
        {age(j.created)}
      </td>
      <td className="px-2 py-1.5">
        {canCancel(j) && (
          <Button variant="ghost" size="xs" onClick={cancel} disabled={busy} aria-label={`Cancel ${jobTitle(j)}`}>
            {busy ? <Loader2 className="animate-spin" /> : <CircleX />} Cancel
          </Button>
        )}
      </td>
    </tr>
  );
}
