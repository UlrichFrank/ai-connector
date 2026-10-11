import { Cpu, Download, HardDrive, Loader2, Lock, MemoryStick, Scale } from "lucide-react";
import { NonCommercialBadge } from "@/components/kit/ModelPicker";
import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Job, Kind, Model } from "@/api/types";
import { Code, ErrorNotice } from "@/components/connection/ErrorNotice";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Progress } from "@/components/ui/progress";
import { Skeleton } from "@/components/ui/skeleton";
import { bytes, progress } from "@/lib/format";
import { cn } from "@/lib/utils";
import { isActive, useApp } from "@/store/app";

const kinds: { kind: Kind; title: string; about: string }[] = [
  { kind: "chat", title: "Chat", about: "Language models for Chat, ✨ Enhance and song writing." },
  { kind: "image", title: "Image", about: "Text to image, and editing with reference images." },
  { kind: "video", title: "Video", about: "Text and image to video, with sound." },
  { kind: "music", title: "Music", about: "Songs from a style and lyrics; some can also rework a song." },
  { kind: "speech", title: "Speech", about: "Text to speech with preset, cloned or designed voices; writing down what a recording says; and timing each spoken word." },
  { kind: "sound", title: "Sound effects", about: "Sounds from a description." },
  { kind: "avatar", title: "Avatar", about: "A portrait that speaks: a video from a picture and speech." },
  { kind: "embed", title: "Embeddings", about: "Turn texts into vectors to compare their meaning." },
  { kind: "classify", title: "Classify", about: "Pick an answer to a question for each text." },
];

/** What a music model can do besides making a song (catalog `tasks`). */
const taskNames: Record<string, string> = {
  cover: "Cover",
  repaint: "Repaint",
  extend: "Extend",
  lego: "Add a track",
  extract: "Extract a track",
  restyle: "Restyle",
};

/** The ids of the models in memory now: /api/models says per backend, e.g. "qwen3-embedding-0.6b, laya-typed-decisions". */
function inMemory(loaded: Record<string, string>): Set<string> {
  return new Set(
    Object.values(loaded)
      .flatMap((s) => s.split(","))
      .map((s) => s.trim())
      .filter(Boolean),
  );
}

/**
 * Where a model is, short: "LM Studio", "the Hugging Face cache", or the last folders of a path. The full place is
 * in the tooltip.
 */
function place(where: string): string {
  const named = where.match(/^([^/(]+?)\s*\(/);
  if (named) return named[1];
  if (!where.startsWith("/")) return where;
  const parts = where.split("/").filter(Boolean);
  return parts.length > 2 ? "…/" + parts.slice(-2).join("/") : where;
}

/** A running job that is fetching its model (fetch.go says "fetching <repo>: 1.2 GB of 3.4 GB"). */
const downloading = (j: Job) => isActive(j) && /fetching|download/i.test(j.status ?? "");

/** Settings → Models: everything mlxlab can run, what is on disk, and what is in memory. */
export function ModelsPage(_: { arg: string }) {
  const models = useApp((s) => s.models);
  const loaded = useApp((s) => s.loaded);
  const jobs = useApp((s) => s.jobs);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(!models.length);

  // our own load (the store's refresh keeps quiet about errors), into the store
  const load = useCallback(() => {
    api.models().then(
      (r) => {
        useApp.setState({ models: r.models, loaded: r.loaded, examples: r.examples ?? {} });
        setError(null);
        setLoading(false);
      },
      (e: unknown) => {
        setError(e);
        setLoading(false);
      },
    );
  }, []);
  useEffect(load, [load]);

  const memory = useMemo(() => inMemory(loaded), [loaded]);
  const fetching = useMemo(() => {
    const m = new Map<string, Job>();
    for (const j of Object.values(jobs)) if (downloading(j)) m.set(j.model, j);
    return m;
  }, [jobs]);

  const here = models.filter((m) => m.here);
  const onDisk = here.reduce((n, m) => n + (m.size || 0), 0);

  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-8 px-4 py-6 sm:px-6 sm:py-8">
      <header className="flex flex-col gap-1">
        <h1 className="text-lg font-semibold">Models</h1>
        <p className="text-sm text-muted-foreground">
          Everything this Mac can run. A model downloads the first time you use it — its progress shows here and with the job.
        </p>
      </header>

      {error != null && <ErrorNotice error={error} onRetry={load} />}

      {models.length > 0 && (
        <>
          <div className="grid gap-3 sm:grid-cols-2">
            <Summary icon={<HardDrive />} label="On disk">
              {here.length} of {models.length} models · {bytes(onDisk)}
            </Summary>
            <MemoryCard ids={memory} models={models} />
          </div>
          {kinds.map(({ kind, title, about }) => {
            const list = models.filter((m) => m.kind === kind);
            if (!list.length) return null;
            return (
              <section key={kind} className="flex flex-col gap-3">
                <div>
                  <h2 className="text-sm font-semibold">
                    {title} <span className="font-normal text-muted-foreground">· {list.length}</span>
                  </h2>
                  <p className="mt-0.5 text-sm text-muted-foreground">{about}</p>
                </div>
                <ul className="flex flex-col divide-y overflow-hidden rounded-xl border bg-card/40">
                  {list.map((m) => (
                    <ModelRow key={m.id} model={m} loaded={memory.has(m.id)} fetching={fetching.get(m.id)} />
                  ))}
                </ul>
              </section>
            );
          })}
        </>
      )}

      {!models.length && loading && <ListSkeleton />}
    </div>
  );
}

function Summary({ icon, label, children, action }: { icon: ReactNode; label: string; children: ReactNode; action?: ReactNode }) {
  return (
    <div className="flex min-w-0 items-center gap-3 rounded-xl border bg-card/40 px-4 py-3">
      <div className="text-muted-foreground [&_svg]:size-4">{icon}</div>
      <div className="min-w-0 flex-1">
        <div className="text-xs text-muted-foreground">{label}</div>
        <div className="line-clamp-2 text-sm font-medium">{children}</div>
      </div>
      {action}
    </div>
  );
}

function MemoryCard({ ids, models }: { ids: Set<string>; models: Model[] }) {
  const [busy, setBusy] = useState(false);
  const refresh = useApp((s) => s.refreshModels);
  const names = [...ids].map((id) => models.find((m) => m.id === id)?.name ?? id);
  const unload = async () => {
    setBusy(true);
    try {
      await api.unload();
      await refresh();
      toast.success("Memory freed", { description: "Models load again when they are next used." });
    } catch (e) {
      toast.error("Could not unload", { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <Summary
      icon={<MemoryStick />}
      label="In memory now"
      action={
        <Button variant="outline" size="sm" disabled={busy || !ids.size} onClick={unload} title="Frees the memory; a running job finishes first">
          {busy && <Loader2 className="animate-spin" />} Unload all
        </Button>
      }
    >
      <span title={names.join(", ")}>{names.length ? names.join(", ") : <span className="font-normal text-muted-foreground">nothing</span>}</span>
    </Summary>
  );
}

function ModelRow({ model: m, loaded, fetching }: { model: Model; loaded: boolean; fetching?: Job }) {
  const p = fetching ? progress(fetching) : undefined;
  return (
    <li className="flex flex-col gap-2 px-4 py-3.5 sm:flex-row sm:items-start sm:gap-4">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span className="font-medium">{m.name}</span>
          {loaded && (
            <Badge className="gap-1 border-emerald-500/30 bg-emerald-500/15 text-emerald-700 dark:text-emerald-300" variant="outline">
              <span className="size-1.5 rounded-full bg-emerald-500" /> in memory
            </Badge>
          )}
          {m.gated && <Gated here={m.here} />}
          {m.nonCommercial && <NonCommercialBadge licence={m.licence} />}
          {m.tasks?.map((t) => (
            <Badge key={t} variant="secondary" className="font-normal">
              {taskNames[t] ?? t}
            </Badge>
          ))}
        </div>
        {m.about && <p className="mt-1 text-sm text-pretty text-muted-foreground">{m.about}</p>}
        <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
          <span className="inline-flex items-center gap-1">
            <Cpu className="size-3" /> {m.backend}
          </span>
          {m.licence && (
            <span className={cn("inline-flex items-center gap-1", m.nonCommercial && "text-amber-700 dark:text-amber-400")} title="Licence">
              <Scale className="size-3" /> {m.licence}
            </span>
          )}
          <span className="font-mono text-[11px] opacity-80">{m.id}</span>
        </div>
        {fetching && (
          <div className="mt-2.5 flex flex-col gap-1">
            <div className="flex items-center gap-1.5 text-xs text-primary">
              <Loader2 className="size-3 animate-spin" />
              <span className="truncate">{fetching.status}</span>
            </div>
            <Progress value={p != null ? p * 100 : undefined} className="h-1" />
          </div>
        )}
      </div>
      <div className="flex shrink-0 flex-wrap items-baseline gap-x-2 gap-y-0.5 text-xs sm:w-44 sm:flex-col sm:flex-nowrap sm:items-end sm:text-right">
        {m.here ? (
          <>
            <span className="inline-flex items-center gap-1 font-medium text-foreground">
              <HardDrive className="size-3 text-muted-foreground" /> {bytes(m.size) || "on disk"}
            </span>
            {m.where && (
              <span className="min-w-0 truncate text-muted-foreground sm:max-w-full" title={m.where}>
                {place(m.where)}
              </span>
            )}
          </>
        ) : (
          <>
            <span className={cn("inline-flex items-center gap-1 font-medium", fetching ? "text-primary" : "text-amber-600 dark:text-amber-400")}>
              <Download className="size-3" /> {bytes(m.size)} download
            </span>
            <span className="text-muted-foreground">{fetching ? "downloading now" : "on first use"}</span>
          </>
        )}
      </div>
    </li>
  );
}

function Gated({ here }: { here: boolean }) {
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button type="button">
          <Badge variant="outline" className="gap-1 hover:bg-accent font-normal text-muted-foreground">
            <Lock className="size-3" /> gated
          </Badge>
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-80 text-sm">
        <div className="font-medium">A licence to accept first</div>
        <p className="mt-1 text-muted-foreground">
          {here ? "It is downloaded already. " : ""}To download it, accept the model's licence on its Hugging Face page, then sign in on the Mac with{" "}
          <Code>huggingface-cli login</Code>.
        </p>
      </PopoverContent>
    </Popover>
  );
}

function ListSkeleton() {
  return (
    <div className="flex flex-col gap-3">
      {[0, 1, 2, 3].map((i) => (
        <Skeleton key={i} className="h-20 w-full rounded-xl" />
      ))}
    </div>
  );
}
