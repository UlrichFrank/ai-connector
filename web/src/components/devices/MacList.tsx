import { Cpu, Crown, Laptop, Loader2, Plus, TriangleAlert } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { toast } from "sonner";
import { api, ApiError, errorText } from "@/api/client";
import type { Job, Worker } from "@/api/types";
import { Ago } from "@/components/connection/Ago";
import { ErrorNotice } from "@/components/connection/ErrorNotice";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { title } from "@/lib/format";
import { isOutdated, referenceVersion, specParts } from "@/lib/macs";
import { openJob } from "@/lib/open";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";
import { MacCard } from "./MacCard";

/**
 * Devices → Macs on the hub: every Mac enrolled with its own token — what it is (chip, memory, GPU, macOS, mlxlab),
 * what it does now (idle, running a job, queued, loaded models), connected or not (live from /api/worker/events), the
 * primary one (serves prompts and tools; another can be made primary), any one revocable. The Mac on the tunnel with
 * the shared token (not in the list), or none: its card on top. `added` (the Mac just made in the Connect dialog) loads
 * the list again.
 */
export function MacList({ added, onConnect }: { added: Worker | null; onConnect: () => void }) {
  const [workers, setWorkers] = useState<Worker[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const live = useApp((s) => s.worker);
  const macs = useApp((s) => s.macs);
  const hubVersion = useApp((s) => s.hubVersion);

  const load = useCallback(
    () =>
      api.workers().then(
        (list) => {
          setWorkers(list ?? []);
          setError(null);
        },
        (e: unknown) => setError(e),
      ),
    [],
  );
  const macIds = macs.map((m) => m.id).join();
  // again whenever a Mac comes or goes (its "last seen" changed), and when one was just added
  useEffect(() => {
    load();
  }, [load, live?.id, live?.connected, macIds, added]);
  // what each Mac does (state, loaded models): the hub keeps it current; we look again now and then
  useEffect(() => {
    const t = setInterval(() => document.visibilityState === "visible" && load(), 15000);
    return () => clearInterval(t);
  }, [load]);

  const revoke = async (w: Worker) => {
    try {
      await api.revokeWorker(w.id);
      setWorkers((list) => list?.filter((x) => x.id !== w.id) ?? null);
      toast.success(`“${w.name}” can no longer connect`);
    } catch (e) {
      toast.error("Could not revoke the Mac", { description: errorText(e) });
    }
  };

  const makePrimary = async (w: Worker) => {
    // at once; the list from the hub follows
    setWorkers((list) => list?.map((x) => ({ ...x, primary: x.id === w.id })) ?? null);
    try {
      await api.makePrimary(w.id);
      toast.success(`“${w.name}” is the primary Mac`, { description: "Prompts, tools and approvals come from it now." });
    } catch (e) {
      toast.error("Could not make it primary", { description: errorText(e) });
    }
    load();
  };

  const autoPrimary = async (w: Worker) => {
    try {
      await api.autoPrimary(w.id);
      toast.success("The primary Mac is chosen automatically again", { description: "The connected Mac with the most memory." });
    } catch (e) {
      toast.error("Could not change the primary Mac", { description: errorText(e) });
    }
    load();
  };

  // on the tunnel: as /api/worker/events says (live), else as the list said when it was loaded
  const isConnected = (w: Worker) => (live ? !!live.connected && (macs.some((m) => m.id === w.id) || live.id === w.id) : w.connected);

  // a hub from before per-Mac tokens: just the Mac on the tunnel
  if (error instanceof ApiError && error.status === 404 && !workers) return <MacCard />;
  if (error && !workers) return <ErrorNotice error={error} onRetry={load} />;
  if (!workers) return <ListSkeleton />;

  const connectedHere = workers.some(isConnected);
  // connected, the primary first, then the most recently seen, then the newest
  const sorted = [...workers].sort(
    (a, b) =>
      Number(isConnected(b)) - Number(isConnected(a)) ||
      Number(!!b.primary) - Number(!!a.primary) ||
      (b.lastSeen ?? "").localeCompare(a.lastSeen ?? "") ||
      b.created.localeCompare(a.created),
  );
  const reference = referenceVersion(hubVersion, workers);
  const several = workers.length > 1;

  return (
    <div className="flex flex-col gap-3">
      {/* nobody on the tunnel, or a Mac with the shared token (not in the list): its state, and the way forward */}
      {!connectedHere && (workers.length > 0 || live?.connected) && <MacCard onConnect={onConnect} />}
      {workers.length === 0 && !live?.connected ? (
        <NoMac onConnect={onConnect} />
      ) : (
        workers.length > 0 && (
          <ul className="flex flex-col gap-3" data-mac-list="">
            {sorted.map((w) => (
              <MacRow
                key={w.id}
                worker={w}
                connected={isConnected(w)}
                reference={reference}
                several={several}
                onRevoke={() => revoke(w)}
                onPrimary={() => makePrimary(w)}
                onAutoPrimary={() => autoPrimary(w)}
              />
            ))}
          </ul>
        )
      )}
    </div>
  );
}

function MacRow({
  worker: w,
  connected,
  reference,
  several,
  onRevoke,
  onPrimary,
  onAutoPrimary,
}: {
  worker: Worker;
  connected: boolean;
  reference?: string;
  several: boolean;
  onRevoke: () => void;
  onPrimary: () => void;
  onAutoPrimary: () => void;
}) {
  const specs = specParts(w);
  const version = w.specs?.version;
  // older than the others, or too old to route (its jobs and files would clash with another Mac's)
  const old = isOutdated(version, reference) || (connected && w.routes === false);
  return (
    <li data-mac-row={w.id} data-connected={connected || undefined}>
      <Card className="gap-0 px-4 py-3.5">
        <div className="flex items-start gap-3 max-sm:flex-wrap">
          <div
            className={cn(
              "relative grid size-9 shrink-0 place-items-center rounded-lg",
              connected ? "bg-emerald-500/10 text-emerald-600 dark:text-emerald-400" : "bg-muted text-muted-foreground",
            )}
          >
            <Laptop className="size-4" />
          </div>
          <div className="min-w-0 flex-1">
            <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
              <span className="truncate text-sm font-medium">{w.name || "Mac"}</span>
              {connected ? (
                <span className="inline-flex shrink-0 items-center gap-1.5 text-xs text-emerald-600 dark:text-emerald-400">
                  <span className="relative flex size-1.5">
                    <span className="absolute inline-flex size-full animate-ping rounded-full bg-emerald-500 opacity-60" />
                    <span className="relative inline-flex size-1.5 rounded-full bg-emerald-500" />
                  </span>
                  Connected
                </span>
              ) : (
                <span className="inline-flex shrink-0 items-center gap-1.5 text-xs text-muted-foreground">
                  <span className="size-1.5 rounded-full bg-muted-foreground/40" />
                  Offline
                </span>
              )}
              {several && w.primary && (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <span className="inline-flex h-5 shrink-0 cursor-default items-center gap-1 rounded-full bg-primary/12 px-2 text-[11px] font-medium text-primary" data-primary="">
                      <Crown className="size-3" /> Primary
                    </span>
                  </TooltipTrigger>
                  <TooltipContent side="top" className="max-w-64">
                    Prompts, tools and approvals come from this Mac; changes go to every connected Mac.
                    {w.pinned ? " Chosen by hand." : " Chosen because it has the most memory."}
                  </TooltipContent>
                </Tooltip>
              )}
            </div>
            <div className="mt-0.5 text-xs text-muted-foreground">
              {connected ? (
                <MacActivity worker={w} />
              ) : w.lastSeen ? (
                <>
                  Last seen <Ago iso={w.lastSeen} />
                </>
              ) : (
                "Not connected yet"
              )}
              {!connected && (
                <span className="max-sm:hidden">
                  {" "}
                  · added <Ago iso={w.created} />
                </span>
              )}
            </div>
          </div>
          <div className="flex shrink-0 items-center gap-0.5 max-sm:order-last max-sm:-mt-1 max-sm:-mb-1 max-sm:w-full max-sm:justify-end">
            {several && connected && !w.primary && (
              <Button variant="ghost" size="sm" className="text-muted-foreground" onClick={onPrimary} data-make-primary="">
                Make primary
              </Button>
            )}
            {several && w.primary && w.pinned && (
              <Button variant="ghost" size="sm" className="text-muted-foreground" onClick={onAutoPrimary} title="Let the hub pick the primary Mac (the one with the most memory)">
                Pick automatically
              </Button>
            )}
            {!w.legacy && <RevokeMac name={w.name} onRevoke={onRevoke} />}
          </div>
        </div>
        {(specs.length > 0 || version || old) && (
          <div className="mt-3 flex flex-wrap items-center gap-1.5 pl-12 max-sm:pl-0" data-mac-specs="">
            {specs.map((x) => (
              <span key={x} className="inline-flex h-6 items-center rounded-md bg-muted/70 px-2 text-xs text-foreground/80 tabular-nums">
                {x}
              </span>
            ))}
            {(version || old) &&
              (old ? (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <span
                      className="inline-flex h-6 cursor-default items-center gap-1 rounded-md bg-amber-500/15 px-2 text-xs font-medium text-amber-700 tabular-nums dark:text-amber-300"
                      data-outdated=""
                    >
                      <TriangleAlert className="size-3" /> mlxlab {version || "(old)"} · update
                    </span>
                  </TooltipTrigger>
                  <TooltipContent side="top" className="max-w-64">
                    {w.routes === false ? "Too old to share the hub with other Macs." : `Older than ${reference}.`} Update mlxlab on this Mac, then start
                    mlxlab worker again.
                  </TooltipContent>
                </Tooltip>
              ) : (
                <span className="inline-flex h-6 items-center rounded-md bg-muted/70 px-2 text-xs text-foreground/80 tabular-nums">mlxlab {version}</span>
              ))}
          </div>
        )}
        {connected && <Loaded worker={w} />}
      </Card>
    </li>
  );
}

/** What a connected Mac does now: "Idle", "Running “A fox in the snow” · 2 queued". Live from the job stream. */
function MacActivity({ worker: w }: { worker: Worker }) {
  const jobs = useApp((s) => s.jobs);
  const { running, queued } = useMemo(() => {
    const all = Object.values(jobs);
    // the jobs tell it live once they carry their Mac; else the hub's last look
    if (all.some((j) => j.worker)) {
      const mine = all.filter((j) => j.worker?.id === w.id);
      return { running: mine.find((j) => j.state === "running"), queued: mine.filter((j) => j.state === "queued").length };
    }
    const id = w.state?.running;
    return { running: id ? (jobs[id] ?? ({ id } as Job)) : undefined, queued: w.state?.queued ?? 0 };
  }, [jobs, w.id, w.state]);
  if (!running && !queued) return <span data-mac-state="idle">Idle · ready for jobs</span>;
  return (
    <span className="flex max-w-full min-w-0 items-center gap-1" data-mac-state="busy">
      {running && (
        <>
          <Loader2 className="size-3 shrink-0 animate-spin text-primary" />
          <span className="min-w-0 truncate">
            Running{" "}
            {running.kind ? (
              <button className="font-medium text-foreground hover:underline" onClick={() => openJob(running)}>
                “{title(running)}”
              </button>
            ) : (
              "a job"
            )}
          </span>
        </>
      )}
      {queued > 0 && (
        <span className="shrink-0">
          {running ? "· " : ""}
          {queued} queued
        </span>
      )}
    </span>
  );
}

/** The models in a connected Mac's memory. */
function Loaded({ worker: w }: { worker: Worker }) {
  const models = useApp((s) => s.models);
  const ids = Object.values(w.state?.loaded ?? {}).filter(Boolean);
  if (!ids.length) return null;
  const name = (id: string) => models.find((m) => m.id === id)?.name ?? id;
  return (
    <div className="mt-2 flex flex-wrap items-center gap-x-1.5 gap-y-1 pl-12 text-xs text-muted-foreground max-sm:pl-0" data-mac-loaded="">
      <Cpu className="size-3.5" />
      <span>In memory</span>
      {ids.map((id) => (
        <span key={id} className="inline-flex items-center gap-1.5">
          <span>·</span>
          <span className="font-medium text-foreground/80">{name(id)}</span>
        </span>
      ))}
    </div>
  );
}

function RevokeMac({ name, onRevoke }: { name: string; onRevoke: () => void }) {
  return (
    <AlertDialog>
      <AlertDialogTrigger asChild>
        <Button variant="ghost" size="sm" className="text-muted-foreground hover:text-destructive">
          Revoke
        </Button>
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Revoke “{name}”?</AlertDialogTitle>
          <AlertDialogDescription>This Mac can no longer connect; it is disconnected now if it is connected.</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction variant="destructive" onClick={onRevoke}>
            Revoke
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

/** No Mac at all yet: what it is for, and the one way forward. */
function NoMac({ onConnect }: { onConnect: () => void }) {
  return (
    <div className="flex flex-col items-center gap-2 rounded-xl border border-dashed px-6 py-10 text-center" data-mac-empty="">
      <Laptop className="size-8 text-muted-foreground" />
      <div className="text-sm font-medium">No Mac connected</div>
      <p className="max-w-sm text-sm text-muted-foreground">
        A Mac with Apple Silicon runs the models and keeps your files. Connect one with a single command in its Terminal.
      </p>
      <Button className="mt-2" onClick={onConnect}>
        <Plus /> Connect a Mac
      </Button>
    </div>
  );
}

function ListSkeleton() {
  return (
    <Card className="gap-0 py-0">
      {[0, 1].map((i) => (
        <div key={i} className="flex items-center gap-3 border-b px-4 py-3 last:border-0">
          <Skeleton className="size-9 rounded-lg" />
          <div className="flex-1 space-y-1.5">
            <Skeleton className="h-3.5 w-40" />
            <Skeleton className="h-3 w-28" />
          </div>
        </div>
      ))}
    </Card>
  );
}
