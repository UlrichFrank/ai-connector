import { AudioLines, Download, Maximize2, Square, Trash2, Wand2 } from "lucide-react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Job } from "@/api/types";
import { DeleteJob, JobError, JobProgress, StarButton } from "@/components/kit/JobBits";
import { download, mainFile } from "@/components/media/media";
import { Button } from "@/components/ui/button";
import { ago, duration, took } from "@/lib/format";
import { cn } from "@/lib/utils";
import { isActive, useApp } from "@/store/app";
import { useVoices, voiceName } from "@/features/voice";
import { OnMac } from "@/components/kit/MacTag";

/** How long the video is, when the backend says (stats: duration, or frames and fps). */
export function avatarSeconds(j: Job): number | undefined {
  const d = Number(j.stats?.duration);
  if (d > 0) return d;
  const f = Number(j.stats?.frames);
  const fps = Number(j.stats?.fps) || 25;
  return f > 0 ? f / fps : undefined;
}

/** One avatar video: the video (or the portrait while it is made), what it says, and what to do with it. */
export function AvatarCard({ job: j, onOpen, onUseAgain, highlight }: { job: Job; onOpen: () => void; onUseAgain: (j: Job) => void; highlight?: boolean }) {
  const file = mainFile(j);
  const active = isActive(j);
  const failed = j.state === "failed" || j.state === "canceled";
  const voices = useVoices((s) => s.voices);
  const modelName = useApp((s) => s.models.find((m) => m.id === j.model)?.name ?? j.name ?? j.model);
  const r = j.request;
  const w = r.width || Number(j.stats?.width) || 640;
  const h = r.height || Number(j.stats?.height) || 640;
  const portrait = r.images?.[0];
  const len = avatarSeconds(j);

  return (
    <article id={`avatar-${j.id}`} className={cn("overflow-hidden rounded-xl border bg-card shadow-xs", highlight && "ring-2 ring-primary")}>
      <div className="relative flex max-h-[60vh] justify-center bg-black" style={{ aspectRatio: w / h }}>
        {file && !active ? (
          <video src={`${file}#t=0.05`} controls playsInline preload="metadata" className="size-full object-contain" />
        ) : portrait ? (
          <img src={portrait} alt="" className={cn("size-full object-contain", active && "opacity-60 blur-[2px]", failed && "opacity-40 grayscale")} />
        ) : null}
        {active && (
          <div className="absolute inset-x-3 bottom-3 rounded-lg bg-background/85 p-2.5 backdrop-blur-md">
            <JobProgress job={j} />
          </div>
        )}
        {file && !active && (
          <Button size="icon-sm" variant="ghost" title="Open" className="absolute top-2 right-2 bg-black/40 text-white backdrop-blur-sm hover:bg-black/60 hover:text-white" onClick={onOpen}>
            <Maximize2 />
          </Button>
        )}
      </div>

      <div className="flex flex-col gap-3 p-4">
        <div className="flex items-start gap-2">
          <div className="min-w-0 flex-1">
            <div className="mb-1 flex flex-wrap items-center gap-x-1.5 gap-y-0.5 text-xs text-muted-foreground">
              <span>{modelName}</span>
              <span>·</span>
              <span className="tabular-nums">
                {w}×{h}
              </span>
              {len ? (
                <>
                  <span>·</span>
                  <span className="tabular-nums">{duration(len)}</span>
                </>
              ) : null}
              {r.text ? (
                <>
                  <span>·</span>
                  <span>{voiceName(r.voice, voices)}</span>
                </>
              ) : null}
              <span>·</span>
              <span title={new Date(j.created).toLocaleString()}>{ago(j.created)}</span>
              {j.state === "done" && took(j) ? <span className="max-sm:hidden">· took {duration(took(j))}</span> : null}
              <OnMac job={j} />
            </div>
            {r.text ? (
              <p className="line-clamp-3 text-sm">“{r.text}”</p>
            ) : (
              <p className="flex items-center gap-1.5 text-sm text-muted-foreground">
                <AudioLines className="size-3.5" /> Speaks a recording
              </p>
            )}
            {r.prompt && <p className="mt-0.5 line-clamp-1 text-xs text-muted-foreground italic">{r.prompt}</p>}
          </div>
          {!active && <StarButton job={j} className="-mt-1 -mr-1" />}
        </div>

        {failed && <JobError job={j} />}

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
          {file && !active && (
            <Button variant="ghost" size="sm" onClick={() => download(file)}>
              <Download /> Download
            </Button>
          )}
          {!active && (
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
