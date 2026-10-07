import { ChevronRight, Clapperboard, Download, Film, Link2, RotateCcw, Scissors, Square, Trash2 } from "lucide-react";
import { useRef, useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Job } from "@/api/types";
import { DeleteJob, JobError, JobProgress, StarButton } from "@/components/kit/JobBits";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ago, duration, isMovie, took } from "@/lib/format";
import { cn } from "@/lib/utils";
import { isActive, useApp } from "@/store/app";
import { ClipStrip } from "./ClipStrip";
import { Player, type PlayerHandle } from "./Player";
import { clipState, movieFrames, secondsOf, secs } from "./units";
import { OnMac } from "@/components/kit/MacTag";

/** One video or movie: the player (or the movie so far), what it is, and what to do with it. */
export function VideoCard({ job: j, onUse, highlight }: { job: Job; onUse: (j: Job) => void; highlight?: boolean }) {
  const player = useRef<PlayerHandle>(null);
  const [at, setAt] = useState<number>();
  const modelName = useApp((s) => s.models.find((m) => m.id === j.model)?.name ?? j.model);
  const movie = isMovie(j);
  const r = j.request;
  const w = r.width || Number(j.stats?.width) || 704;
  const h = r.height || Number(j.stats?.height) || 448;
  const fps = r.fps || Number(j.stats?.fps) || 24;
  const file = j.files?.at(-1);
  const active = isActive(j);
  const failed = j.state === "failed" || j.state === "canceled";
  const { done, total } = clipState(j);
  const frames = movie ? movieFrames(r.script!, r.frames || 0) : r.frames || Number(j.stats?.frames) || 0;
  const length = frames ? secs(secondsOf(frames, fps)) : "";
  const text = movie ? r.script?.[0]?.prompt : r.prompt;

  let media: React.ReactNode;
  if (file) media = <Player ref={player} src={file} aspect={w / h} onTime={movie ? setAt : undefined} />;
  else if (j.preview)
    media = (
      <Player
        key={j.preview}
        src={j.preview}
        aspect={w / h}
        label={
          <span className="flex items-center gap-1.5 rounded-full bg-black/50 px-2 py-0.5 backdrop-blur">
            <Film className="size-3" /> {active ? "The movie so far" : "Finished so far"} · {done} of {total} clips
          </span>
        }
      />
    );
  else if (active || r.images?.length) media = <Waiting job={j} aspect={w / h} />;
  // a failed one without anything to watch stays small

  return (
    <article
      id={`video-${j.id}`}
      className={cn("overflow-hidden rounded-xl border bg-card shadow-xs transition-shadow", highlight && "ring-2 ring-primary")}
    >
      {media}
      <div className="flex flex-col gap-3 p-4">
        <div className="flex items-start gap-2">
          <div className="min-w-0 flex-1">
            <div className="mb-1 flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
              {movie && (
                <Badge variant="secondary" className="gap-1">
                  <Clapperboard className="size-3" /> Movie · {r.script!.length} clips
                </Badge>
              )}
              {r.images?.length ? <Badge variant="outline">from an image</Badge> : null}
              <span>{modelName}</span>
              <span>·</span>
              <span className="tabular-nums">
                {w}×{h}
              </span>
              {length && (
                <>
                  <span>·</span>
                  <span className="tabular-nums">{length}</span>
                </>
              )}
              {r.noAudio && (
                <>
                  <span>·</span>
                  <span>no sound</span>
                </>
              )}
              <span>·</span>
              <span title={new Date(j.created).toLocaleString()}>{ago(j.created)}</span>
              {j.state === "done" && took(j) ? <span className="max-sm:hidden">· took {duration(took(j))}</span> : null}
              <OnMac job={j} />
            </div>
            {movie && r.prompt && <p className="line-clamp-1 text-xs text-muted-foreground italic">{r.prompt}</p>}
            <p className={cn("text-sm", movie ? "line-clamp-2" : "line-clamp-3")}>{text}</p>
          </div>
          <StarButton job={j} className="-mt-1 -mr-1" />
        </div>

        {active && <JobProgress job={j} />}
        {movie && <ClipStrip job={j} at={at} onSeek={(t) => player.current?.seek(t)} />}
        {movie && <Script job={j} />}

        {failed && (
          <div className="flex flex-col gap-1.5">
            <JobError job={j} />
            {movie && j.state !== "canceled" && (
              <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
                <RotateCcw className="size-3" />
                Retry resumes the movie: finished clips are kept{done ? ` (${done} of ${total} done)` : ""}.
              </p>
            )}
          </div>
        )}

        <div className="-mx-1.5 -mb-1.5 flex flex-wrap items-center gap-0.5">
          {active && (
            <Button
              variant="ghost"
              size="sm"
              onClick={async () => {
                try {
                  await api.cancel(j.id);
                } catch (e) {
                  toast.error("Could not stop", { description: errorText(e) });
                }
              }}
            >
              <Square className="fill-current" /> {j.state === "queued" ? "Cancel" : "Stop"}
            </Button>
          )}
          {file && (
            <Button variant="ghost" size="sm" asChild>
              <a href={file} download>
                <Download /> Download
              </a>
            </Button>
          )}
          <Button variant="ghost" size="sm" onClick={() => onUse(j)} title={movie ? "Load the whole script into the composer" : "Load this prompt and its settings into the composer"}>
            <RotateCcw /> Use again
          </Button>
          <div className="flex-1" />
          {!active && (
            <DeleteJob
              job={j}
              trigger={(open) => (
                <Button variant="ghost" size="icon-sm" title="Delete" className="text-muted-foreground hover:text-destructive" onClick={open}>
                  <Trash2 />
                </Button>
              )}
            />
          )}
        </div>
      </div>
    </article>
  );
}

/** Before there is anything to watch: the first frame if there is one, else a calm placeholder. */
function Waiting({ job: j, aspect }: { job: Job; aspect: number }) {
  const img = j.request.images?.[0];
  const active = isActive(j);
  return (
    <div className="relative grid max-h-[70vh] place-items-center overflow-hidden bg-muted/60" style={{ aspectRatio: aspect }}>
      {img && <img src={img} alt="" className={cn("absolute inset-0 size-full object-cover", active ? "opacity-50 blur-[2px]" : "opacity-30 grayscale")} />}
      {active && <div className="absolute inset-0 animate-pulse bg-gradient-to-br from-primary/10 via-transparent to-primary/5" />}
      <div className="relative flex flex-col items-center gap-1.5 text-sm text-muted-foreground">
        <Film className={cn("size-7", active && "text-primary")} />
        {j.state === "queued" ? "Waiting for the GPU" : active ? (isMovie(j) ? "The first clip is rendering" : "Rendering") : "No video"}
      </div>
    </div>
  );
}

/** A movie's script, folded: each clip and how it joins the one before. */
function Script({ job: j }: { job: Job }) {
  const [open, setOpen] = useState(false);
  const script = j.request.script ?? [];
  return (
    <div>
      <button type="button" onClick={() => setOpen(!open)} className="flex items-center gap-1 text-xs font-medium text-muted-foreground hover:text-foreground">
        <ChevronRight className={cn("size-3.5 transition-transform", open && "rotate-90")} />
        Script
      </button>
      {open && (
        <ol className="mt-2 flex flex-col gap-1.5 text-xs">
          {j.request.prompt && (
            <li className="rounded-md bg-muted/50 px-2.5 py-1.5 text-muted-foreground">
              <span className="font-medium text-foreground">Story · </span>
              {j.request.prompt}
            </li>
          )}
          {script.map((c, i) => (
            <li key={i} className="flex gap-2">
              <span className="w-4 shrink-0 text-right font-semibold text-muted-foreground tabular-nums">{i + 1}</span>
              <span className="mt-0.5 shrink-0 text-muted-foreground" title={i === 0 ? "Opening" : c.continue ? "Continues from the last frame" : "New scene"}>
                {i === 0 ? <Film className="size-3" /> : c.continue ? <Link2 className="size-3 text-primary" /> : <Scissors className="size-3 text-amber-500" />}
              </span>
              <span>{c.prompt}</span>
            </li>
          ))}
        </ol>
      )}
    </div>
  );
}
