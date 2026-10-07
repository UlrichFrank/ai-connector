import { Pause, Play, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Slider } from "@/components/ui/slider";
import { duration, title } from "@/lib/format";
import { navigate } from "@/lib/route";
import { usePlayer } from "@/store/player";

/** The player at the bottom of every page while something is loaded. */
export function PlayerBar() {
  const { job, paused, time, duration: len, toggle, seek, stop } = usePlayer();
  if (!job) return null;
  return (
    <div className="flex items-center gap-3 border-t bg-card px-4 py-2">
      <Button size="icon" className="rounded-full" onClick={() => toggle(job)}>
        {paused ? <Play className="fill-current" /> : <Pause className="fill-current" />}
      </Button>
      <button className="w-48 min-w-0 text-left max-sm:w-28" onClick={() => navigate(job.kind === "speech" || job.kind === "sound" ? "voice" : "music", job.id)}>
        <div className="truncate text-sm font-medium">{title(job)}</div>
        <div className="truncate text-xs text-muted-foreground">{job.kind === "speech" ? (job.request.instruct ?? "Speech") : job.kind === "sound" ? "Sound effect" : job.request.prompt}</div>
      </button>
      <span className="w-10 text-right text-xs text-muted-foreground tabular-nums">{duration(time)}</span>
      <Slider className="flex-1" value={[time]} max={len || 1} step={0.1} onValueChange={([v]) => seek(v)} />
      <span className="w-10 text-xs text-muted-foreground tabular-nums">{duration(len)}</span>
      <Button size="icon-sm" variant="ghost" title="Close" onClick={stop}>
        <X />
      </Button>
    </div>
  );
}
