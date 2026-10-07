import { Play, Volume2 } from "lucide-react";
import { forwardRef, useImperativeHandle, useRef, useState } from "react";
import { cn } from "@/lib/utils";

export interface PlayerHandle {
  seek: (t: number) => void;
}

/**
 * A video that plays muted while the pointer rests on it, and becomes a real player (controls, sound) on click.
 * The element stays the same while its job updates, so it keeps playing.
 */
export const Player = forwardRef<PlayerHandle, { src: string; aspect: number; className?: string; label?: React.ReactNode; onTime?: (t: number) => void }>(
  function Player({ src, aspect, className, label, onTime }, ref) {
    const video = useRef<HTMLVideoElement>(null);
    const [active, setActive] = useState(false);
    const [playing, setPlaying] = useState(false);

    const activate = () => {
      const v = video.current;
      if (!v) return;
      setActive(true);
      v.muted = false;
      v.play().catch(() => {});
    };

    useImperativeHandle(ref, () => ({
      seek: (t) => {
        const v = video.current;
        if (!v) return;
        v.currentTime = t;
        if (!active) activate();
        else v.play().catch(() => {});
      },
    }));

    return (
      <div
        className={cn("group/player relative max-h-[70vh] overflow-hidden bg-black", className)}
        style={{ aspectRatio: aspect }}
        onMouseEnter={() => {
          if (active || !video.current) return;
          video.current.muted = true;
          video.current.play().catch(() => {});
        }}
        onMouseLeave={() => {
          if (active) return;
          video.current?.pause();
        }}
      >
        <video
          ref={video}
          // the first frame shows before it plays
          src={src.includes("#") ? src : src + "#t=0.05"}
          className="size-full object-contain"
          preload="metadata"
          playsInline
          loop
          muted={!active}
          controls={active}
          onPlay={() => setPlaying(true)}
          onPause={() => setPlaying(false)}
          onTimeUpdate={(e) => onTime?.(e.currentTarget.currentTime)}
        />
        {!active && (
          <button
            type="button"
            onClick={activate}
            className="absolute inset-0 flex items-end justify-between bg-gradient-to-t from-black/50 via-transparent to-transparent p-3 text-left text-white opacity-100 transition-opacity"
            aria-label="Play with sound"
          >
            <span className="text-xs font-medium drop-shadow">{label}</span>
            <span
              className={cn(
                "grid size-10 place-items-center rounded-full bg-white/15 backdrop-blur-md transition-all group-hover/player:bg-primary",
                playing && "size-8",
              )}
            >
              {playing ? <Volume2 className="size-4" /> : <Play className="size-4 translate-x-px fill-current" />}
            </span>
          </button>
        )}
      </div>
    );
  },
);
