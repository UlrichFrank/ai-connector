import { Activity, CircleX, Clock, ListX, Loader2, Server } from "lucide-react";
import { useMemo, useState, type ComponentType } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Job, Worker } from "@/api/types";
import { Dot, Empty, Page, PageHeader } from "@/components/common";
import { StateBadge } from "@/components/StateBadge";
import { Button } from "@/components/ui/button";
import { age } from "@/lib/format";
import { dateTime, t } from "@/lib/i18n";
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
      <PageHeader title={t("Overview")} description={t("The connected workers and the queue of every worker, live.")} />

      <div className="mb-6 grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Tile icon={Server} label={t("Workers connected")} value={`${connected.length}`} sub={workers.length ? t("of {n} enrolled", { n: workers.length }) : t("none enrolled")} onClick={() => navigate("workers")} />
        <Tile icon={Activity} label={t("Running")} value={`${stats.running}`} tone={stats.running ? "primary" : undefined} />
        <Tile icon={Clock} label={t("Queued")} value={`${stats.queued}`} />
        <Tile icon={CircleX} label={t("Failed")} value={`${stats.failed}`} sub={t("in the job history")} tone={stats.failed ? "bad" : undefined} />
      </div>

      <section className="mb-8">
        <h3 className="sec-title">{t("Workers")}</h3>
        {connected.length ? (
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            {connected.map((w) => (
              <WorkerTile key={w.id} w={w} />
            ))}
          </div>
        ) : (
          <Empty icon={Server} title={t("No worker is connected")}>
            {t("Jobs run on workers: Macs, Linux and Windows PCs.")}{" "}
            <button className="font-medium text-[var(--teal-ink)] hover:underline" onClick={() => navigate("workers", "connect")}>
              {t("Connect a worker")}
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
    <Comp onClick={onClick} className={cn("card flex flex-col gap-1 p-4 text-left", onClick && "transition-shadow hover:shadow-[var(--elevation-2),var(--highlight)]")}>
      <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
        <Icon className="size-3.5" /> {label}
      </span>
      <span className={cn("text-[26px] leading-tight font-semibold tracking-[-0.03em] tabular-nums", tone === "primary" && "text-[var(--teal-ink)]", tone === "bad" && "text-destructive")}>{value}</span>
      {sub && <span className="text-xs text-muted-foreground">{sub}</span>}
    </Comp>
  );
}

function WorkerTile({ w }: { w: Worker }) {
  const busy = !!w.state?.running;
  const loaded = loadedModels(w);
  return (
    <button onClick={() => navigate("workers", w.id)} className="card flex flex-col gap-1.5 p-4 text-left transition-shadow hover:shadow-[var(--elevation-2),var(--highlight)]">
      <div className="flex items-center gap-2">
        <PlatformIcon platform={platformOf(w.specs)} />
        <span className="min-w-0 flex-1 truncate font-medium">{w.name}</span>
        {w.primary && <span className="rounded bg-primary/15 px-1.5 text-[11px] font-medium text-[var(--teal-ink)]">{t("primary")}</span>}
        <Dot tone={busy ? "busy" : "ok"} pulse={busy} />
      </div>
      <div className="truncate text-xs text-muted-foreground">{specLine(w) || t("specs not known yet")}</div>
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
      <div className="mb-3 flex items-center gap-2">
        <h3 className="sec-title mb-0 flex-1">
          {t("Queue")} {live ? "" : t("(not live)")}
        </h3>
        <div role="tablist" className="seg">
          {[
            [false, t("Active ({n})", { n: active.length })],
            [true, t("With history")],
          ].map(([v, label]) => (
            <button key={String(v)} role="tab" aria-selected={all === v} aria-pressed={all === v} onClick={() => setAll(v as boolean)}>
              {label}
            </button>
          ))}
        </div>
      </div>
      {shown.length ? (
        <div className="card overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="text-left text-xs text-muted-foreground">
              <tr>
                <th className="px-3 py-2.5 font-medium">{t("Job")}</th>
                <th className="px-3 py-2.5 font-medium">{t("State")}</th>
                <th className="px-3 py-2.5 font-medium max-sm:hidden">{t("Worker")}</th>
                <th className="px-3 py-2.5 font-medium max-md:hidden">{t("Client")}</th>
                <th className="px-3 py-2.5 text-right font-medium">{t("Age")}</th>
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
        <Empty icon={ListX} title={all ? t("No jobs yet") : t("Nothing queued or running")}>
          {all ? t("Jobs from the mlxlab app and from clients (the OpenAI API, the native jobs API) show up here.") : t("Switch to “With history” to see finished jobs.")}
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
      toast.success(t("Cancelled"), { description: jobTitle(j) });
    } catch (e) {
      toast.error(t("Could not cancel"), { description: errorText(e) });
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
      <td className="px-3 py-2 text-right whitespace-nowrap text-muted-foreground tabular-nums" title={dateTime(j.created)}>
        {age(j.created)}
      </td>
      <td className="px-2 py-1.5">
        {canCancel(j) && (
          <Button variant="ghost" size="xs" onClick={cancel} disabled={busy} aria-label={t("Cancel {name}", { name: jobTitle(j) })}>
            {busy ? <Loader2 className="animate-spin" /> : <CircleX />} {t("Cancel")}
          </Button>
        )}
      </td>
    </tr>
  );
}
