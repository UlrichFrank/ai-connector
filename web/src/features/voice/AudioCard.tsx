import { AudioLines, Download, Pause, Play, Square, Trash2, Wand2 } from "lucide-react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Job } from "@/api/types";
import { DeleteJob, JobError, JobProgress, StarButton } from "@/components/kit/JobBits";
import { download, gradientFor, mainFile } from "@/components/media/media";
import { Button } from "@/components/ui/button";
import { ago, duration, took } from "@/lib/format";
import { cn } from "@/lib/utils";
import { isActive, useApp } from "@/store/app";
import { usePlayer, usePlaying } from "@/store/player";
import { languageName, voiceName, useVoices } from "./voices";
import { OnMac } from "@/components/kit/MacTag";

/** How long the audio is: what the backend said (stats.seconds is the length of speech), else what was asked for. */
export function audioSeconds(j: Job): number | undefined {
  const s = j.stats?.duration ?? (j.kind === "speech" ? j.stats?.seconds : undefined);
  return typeof s === "number" && s > 0 ? s : j.request.duration || undefined;
}

/**
 * One speech or sound effect: ▶ in the player bar, what was said (or asked for), how, and what to do with it.
 * Running jobs show their progress, failed ones what went wrong.
 */
export function AudioCard({ job: j, onUseAgain, highlight }: { job: Job; onUseAgain?: (j: Job) => void; highlight?: boolean }) {
  const file = mainFile(j);
  const active = isActive(j);
  const failed = j.state === "failed" || j.state === "canceled";
  const playing = usePlaying(file);
  const toggle = usePlayer((s) => s.toggle);
  const voices = useVoices((s) => s.voices);
  const modelName = useApp((s) => s.models.find((m) => m.id === j.model)?.name ?? j.name ?? j.model);
  const r = j.request;
  const speech = j.kind === "speech";
  const len = audioSeconds(j);

  const meta = [
    modelName,
    speech ? voiceName(r.voice, voices) : "",
    speech ? languageName(r.language) : "",
    speech && r.speed && r.speed !== 1 ? `${r.speed}×` : "",
    len ? duration(len) : "",
  ].filter(Boolean);

  return (
    <article id={`audio-${j.id}`} className={cn("flex gap-3 rounded-xl border bg-card p-3.5 shadow-xs transition-shadow", highlight && "ring-2 ring-primary")}>
      <div className="relative grid size-12 shrink-0 place-items-center overflow-hidden rounded-lg" style={{ background: gradientFor(j.id) }}>
        {file && !active ? (
          <Button size="icon" className="size-12 rounded-lg bg-transparent shadow-none hover:bg-black/20" title={playing ? "Pause" : "Play"} onClick={() => toggle(j)}>
            {playing ? <Pause className="size-5 fill-current text-white" /> : <Play className="size-5 fill-current text-white" />}
          </Button>
        ) : (
          <AudioLines className={cn("size-5 text-white opacity-70", active && "animate-pulse")} />
        )}
      </div>

      <div className="flex min-w-0 flex-1 flex-col gap-2">
        <div className="flex items-start gap-2">
          <div className="min-w-0 flex-1">
            <div className="mb-0.5 flex flex-wrap items-center gap-x-1.5 gap-y-0.5 text-xs text-muted-foreground">
              {meta.map((m, i) => (
                <span key={i} className="flex items-center gap-1.5">
                  {i > 0 && <span>·</span>}
                  <span className={cn(i > 0 && "tabular-nums")}>{m}</span>
                </span>
              ))}
              <span>·</span>
              <span title={new Date(j.created).toLocaleString()}>{ago(j.created)}</span>
              {j.state === "done" && took(j) ? <span className="max-sm:hidden">· took {duration(took(j))}</span> : null}
              <OnMac job={j} />
            </div>
            <p className="line-clamp-3 text-sm whitespace-pre-line">{speech ? r.text : r.prompt}</p>
            {speech && r.instruct && <p className="mt-0.5 line-clamp-1 text-xs text-muted-foreground italic">{r.instruct}</p>}
          </div>
          {!active && <StarButton job={j} className="-mt-1 -mr-1" />}
        </div>

        {file && !active && <Scrubber job={j} file={file} />}
        {active && <JobProgress job={j} />}
        {failed && <JobError job={j} />}

        <div className="-mx-1.5 -mb-1 flex flex-wrap items-center gap-0.5">
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
          {file && !active && (
            <Button variant="ghost" size="sm" onClick={() => download(file)}>
              <Download /> Download
            </Button>
          )}
          {onUseAgain && !active && (
            <Button variant="ghost" size="sm" onClick={() => onUseAgain(j)}>
              <Wand2 /> Use again
            </Button>
          )}
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

/** Where the player is in this file (only while it is the one loaded); click to jump. */
function Scrubber({ job: j, file }: { job: Job; file: string }) {
  const loaded = usePlayer((s) => s.file === file);
  const time = usePlayer((s) => (s.file === file ? s.time : 0));
  const len = usePlayer((s) => (s.file === file ? s.duration : 0));
  const play = usePlayer((s) => s.play);
  if (!loaded) return null;
  return (
    <div className="flex items-center gap-2 text-[11px] text-muted-foreground tabular-nums">
      <span className="w-8">{duration(time)}</span>
      <button
        type="button"
        aria-label="Seek"
        className="relative h-1.5 flex-1 overflow-hidden rounded-full bg-muted"
        onClick={(e) => {
          const b = e.currentTarget.getBoundingClientRect();
          if (len) play(j, file, ((e.clientX - b.left) / b.width) * len);
        }}
      >
        <span className="absolute inset-y-0 left-0 rounded-full bg-primary" style={{ width: `${len ? (time / len) * 100 : 0}%` }} />
      </button>
      <span className="w-8 text-right">{duration(len)}</span>
    </div>
  );
}
