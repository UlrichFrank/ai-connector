import { Check, Link2, Loader2, Scissors } from "lucide-react";
import type { Job } from "@/api/types";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { progress } from "@/lib/format";
import { cn } from "@/lib/utils";
import { clipState, secs, timeline } from "./units";

/**
 * A movie's clips as a strip, each as wide as it is long: done, being rendered (with its progress), or still to come;
 * a gap marks a cut. For a finished movie a click on a clip jumps there.
 */
export function ClipStrip({ job: j, onSeek, at }: { job: Job; onSeek?: (t: number) => void; at?: number }) {
  const script = j.request.script ?? [];
  const fps = j.request.fps || Number(j.stats?.fps) || 24;
  const clips = timeline(script, j.request.frames || 0, fps);
  const running = j.state === "running" || j.state === "queued";
  const finished = j.state === "done";
  const { done, current, total } = clipState(j);
  const p = progress(j);
  const known = clips.every((c) => c.frames);
  // the one playing, when the times are known
  const playing = finished && at != null ? clips.findLastIndex((c) => c.start != null && at >= c.start - 0.01) : -1;

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex h-7 items-stretch gap-0.5">
        {clips.map((c, i) => {
          const state = finished || i < done ? "done" : running && i === current ? "now" : "todo";
          const cut = i > 0 && !script[i].continue;
          return (
            <Tooltip key={i}>
              <TooltipTrigger asChild>
                <button
                  type="button"
                  disabled={!(finished && onSeek && c.start != null)}
                  onClick={() => c.start != null && onSeek?.(c.start)}
                  style={{ flexGrow: known ? c.play : 1 }}
                  className={cn(
                    "relative flex min-w-5 basis-0 items-center justify-center overflow-hidden rounded-[5px] text-[10px] font-semibold tabular-nums transition-colors",
                    cut && "ml-1.5",
                    state === "done" && "bg-primary/25 text-primary",
                    state === "now" && "bg-primary/10 text-primary ring-1 ring-primary/60",
                    state === "todo" && "bg-muted text-muted-foreground",
                    j.state === "failed" && i === current && i >= done && "bg-destructive/15 text-destructive ring-1 ring-destructive/50",
                    finished && onSeek && "cursor-pointer hover:bg-primary/40",
                    playing === i && "bg-primary text-primary-foreground hover:bg-primary",
                  )}
                >
                  {state === "now" && <span className="absolute inset-y-0 left-0 bg-primary/30 transition-[width]" style={{ width: `${(p ?? 0) * 100}%` }} />}
                  <span className="relative flex items-center gap-0.5">
                    {state === "now" ? <Loader2 className="size-3 animate-spin" /> : state === "done" && !finished ? <Check className="size-3" /> : null}
                    {i + 1}
                  </span>
                </button>
              </TooltipTrigger>
              <TooltipContent className="max-w-72">
                <div className="mb-0.5 flex items-center gap-1 font-medium">
                  Clip {i + 1}
                  {i > 0 && (script[i].continue ? <><Link2 className="size-3" /> continues</> : <><Scissors className="size-3" /> new scene</>)}
                  {c.frames ? <span className="font-normal opacity-70">· {secs((c.frames - 1) / fps)}</span> : null}
                </div>
                <div className="opacity-80">{script[i].prompt}</div>
              </TooltipContent>
            </Tooltip>
          );
        })}
      </div>
      {running && total > 0 && (
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <span>
            <span className="font-medium text-foreground tabular-nums">
              {done} of {total}
            </span>{" "}
            clips done
          </span>
          {j.state === "running" && current < total && <span>· rendering clip {current + 1}</span>}
          {j.state === "running" && done >= total && <span>· joining</span>}
        </div>
      )}
    </div>
  );
}
