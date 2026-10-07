import { AudioLines, Clapperboard, Film, Music, Pause, Play, SquareUserRound, Waves, X } from "lucide-react";
import { useRef, type ReactNode } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Job } from "@/api/types";
import { JobError, JobProgress, StarButton } from "@/components/kit/JobBits";
import { OnMac } from "@/components/kit/MacTag";
import { Button } from "@/components/ui/button";
import { duration, isMovie, title } from "@/lib/format";
import { cn } from "@/lib/utils";
import { isActive } from "@/store/app";
import { usePlayer, usePlaying } from "@/store/player";
import { gradientFor, isAudio, mainFile, promptOf, typeLabel, videoSeconds } from "./media";

/**
 * One thing made (an image, a video or a song) as a tile. It fills its parent: the grid decides the size (a square in
 * the library, the picture's own shape in a masonry). Running jobs show their progress, failed ones what went wrong.
 * `actions` sit in the hover bar at the bottom; `badge` marks videos and songs in a mixed grid.
 */
export function MediaCard({
  job: j,
  onOpen,
  actions,
  badge,
  className,
}: {
  job: Job;
  onOpen?: () => void;
  actions?: ReactNode;
  badge?: boolean;
  className?: string;
}) {
  const file = mainFile(j);
  const active = isActive(j);
  const failed = j.state === "failed" || j.state === "canceled";
  const open = onOpen && !active && !failed && file ? onOpen : undefined;

  return (
    <div
      role={open ? "button" : undefined}
      tabIndex={open ? 0 : undefined}
      aria-label={open ? title(j) : undefined}
      onClick={open}
      onKeyDown={(e) => open && (e.key === "Enter" || e.key === " ") && (e.preventDefault(), open())}
      className={cn(
        "group/card relative size-full overflow-hidden rounded-xl bg-muted/60 ring-1 ring-foreground/5 outline-none",
        open && "cursor-zoom-in focus-visible:ring-2 focus-visible:ring-ring",
        className,
      )}
    >
      {active ? (
        <Running job={j} />
      ) : failed ? (
        <Failed job={j} actions={actions} />
      ) : j.kind === "music" || isAudio(j) ? (
        <SongArt job={j} />
      ) : j.kind === "video" || j.kind === "avatar" ? (
        <VideoThumb job={j} />
      ) : (
        <ImageThumb job={j} />
      )}

      {!active && !failed && (
        <>
          {badge && j.kind !== "image" && (
            <span className="pointer-events-none absolute top-2 left-2 flex items-center gap-1 rounded-full bg-black/50 px-2 py-0.5 text-[11px] font-medium text-white backdrop-blur-sm">
              {j.kind === "music" ? (
                <Music className="size-3" />
              ) : j.kind === "speech" ? (
                <AudioLines className="size-3" />
              ) : j.kind === "sound" ? (
                <Waves className="size-3" />
              ) : j.kind === "avatar" ? (
                <SquareUserRound className="size-3" />
              ) : isMovie(j) ? (
                <Film className="size-3" />
              ) : (
                <Clapperboard className="size-3" />
              )}
              {j.kind === "music"
                ? j.request.duration
                  ? duration(j.request.duration)
                  : "Song"
                : isAudio(j)
                  ? typeLabel(j)
                  : j.kind === "avatar"
                    ? "Avatar"
                    : duration(videoSeconds(j)) || (isMovie(j) ? "Movie" : "Video")}
            </span>
          )}
          <div className={cn("absolute top-1.5 right-1.5 transition-opacity", !j.starred && "opacity-0 group-hover/card:opacity-100 group-focus-within/card:opacity-100")}>
            <StarButton job={j} className="bg-black/35 text-white backdrop-blur-sm hover:bg-black/55 hover:text-white" />
          </div>
          {j.kind !== "music" && !isAudio(j) && (
            <div className="pointer-events-none absolute inset-x-0 bottom-0 flex items-end gap-2 bg-gradient-to-t from-black/75 via-black/30 to-transparent p-2.5 pt-10 opacity-0 transition-opacity group-hover/card:opacity-100 group-focus-within/card:opacity-100 max-sm:hidden">
              <div className="min-w-0 flex-1">
                <OnMac job={j} plain className="block text-[11px] font-medium text-white/70" />
                <p className="line-clamp-2 text-xs leading-snug text-white/90">{promptOf(j)}</p>
              </div>
              {actions && (
                <div className="pointer-events-auto flex shrink-0 gap-0.5 [&_button]:text-white [&_button:hover]:bg-white/15 [&_button:hover]:text-white" onClick={(e) => e.stopPropagation()}>
                  {actions}
                </div>
              )}
            </div>
          )}
        </>
      )}
    </div>
  );
}

function ImageThumb({ job: j }: { job: Job }) {
  return <img src={mainFile(j)} alt={promptOf(j)} loading="lazy" decoding="async" draggable={false} className="size-full object-cover transition-transform duration-500 group-hover/card:scale-[1.02]" />;
}

/** The first frame; plays (muted) while the pointer is over it. */
function VideoThumb({ job: j }: { job: Job }) {
  const ref = useRef<HTMLVideoElement>(null);
  return (
    <video
      ref={ref}
      src={`${mainFile(j)}#t=0.1`}
      muted
      loop
      playsInline
      preload="metadata"
      className="size-full object-cover"
      onPointerEnter={() => void ref.current?.play().catch(() => {})}
      onPointerLeave={() => {
        const v = ref.current;
        if (!v) return;
        v.pause();
        v.currentTime = 0.1;
      }}
    />
  );
}

/** A song: its cover (or a gradient of its own), the title, and play. */
function SongArt({ job: j }: { job: Job }) {
  const file = mainFile(j);
  const playing = usePlaying(file);
  const toggle = usePlayer((s) => s.toggle);
  return (
    <div className="relative size-full" style={j.cover ? undefined : { background: gradientFor(j.id) }}>
      {j.cover ? (
        <img src={j.cover} alt="" loading="lazy" decoding="async" className="size-full object-cover" />
      ) : j.kind === "speech" ? (
        <AudioLines className="absolute top-1/2 left-1/2 size-10 -translate-1/2 text-white opacity-25" />
      ) : j.kind === "sound" ? (
        <Waves className="absolute top-1/2 left-1/2 size-10 -translate-1/2 text-white opacity-25" />
      ) : (
        <Music className="absolute top-1/2 left-1/2 size-10 -translate-1/2 text-white opacity-25" />
      )}
      <div className="absolute inset-x-0 bottom-0 flex items-end gap-2 bg-gradient-to-t from-black/70 to-transparent p-3 pt-12">
        <div className="min-w-0 flex-1">
          <div className={cn("text-sm font-semibold text-white", isAudio(j) ? "line-clamp-2" : "truncate")}>{title(j)}</div>
          {j.kind === "music" && j.request.prompt && <div className="truncate text-xs text-white/70">{j.request.prompt}</div>}
          {j.kind === "speech" && j.request.instruct && <div className="truncate text-xs text-white/70">{j.request.instruct}</div>}
        </div>
        {file && (
          <Button
            size="icon"
            className="shrink-0 rounded-full shadow-lg"
            title={playing ? "Pause" : "Play"}
            onClick={(e) => (e.stopPropagation(), toggle(j))}
          >
            {playing ? <Pause className="fill-current" /> : <Play className="fill-current" />}
          </Button>
        )}
      </div>
    </div>
  );
}

function Running({ job: j }: { job: Job }) {
  return (
    <div className="relative size-full">
      {j.preview ? (
        <img src={j.preview} alt="" className="size-full object-cover opacity-80 blur-[1px]" />
      ) : (
        <div className="size-full animate-pulse bg-gradient-to-br from-primary/15 via-muted to-primary/5" />
      )}
      <Button
        size="icon-sm"
        variant="ghost"
        title="Cancel"
        className="absolute top-1.5 right-1.5 bg-background/60 backdrop-blur-sm"
        onClick={async (e) => {
          e.stopPropagation();
          try {
            await api.cancel(j.id);
          } catch (err) {
            toast.error("Could not cancel", { description: errorText(err) });
          }
        }}
      >
        <X />
      </Button>
      <div className="absolute inset-x-2 bottom-2 flex flex-col gap-2 rounded-lg bg-background/80 p-2.5 backdrop-blur-md">
        <p className="line-clamp-2 text-xs leading-snug">{promptOf(j) || title(j)}</p>
        <OnMac job={j} className="self-start bg-foreground/10" />
        <JobProgress job={j} />
      </div>
    </div>
  );
}

function Failed({ job: j, actions }: { job: Job; actions?: ReactNode }) {
  return (
    <div className="flex size-full flex-col gap-2 overflow-y-auto p-2.5">
      <p className="line-clamp-3 text-xs text-muted-foreground">{promptOf(j) || title(j)}</p>
      <div className="mt-auto" onClick={(e) => e.stopPropagation()}>
        <JobError job={j} compact />
      </div>
      {actions && (
        <div className="flex justify-end gap-0.5" onClick={(e) => e.stopPropagation()}>
          {actions}
        </div>
      )}
    </div>
  );
}
