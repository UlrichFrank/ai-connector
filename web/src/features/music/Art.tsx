import { Music } from "lucide-react";
import type { Job } from "@/api/types";
import { cn } from "@/lib/utils";
import { gradient } from "./songs";

/**
 * A song's cover, or a gradient of its own colour with a note. A version without a cover of its own looks like its
 * original (`base`).
 */
export function SongArt({ job: j, base, className, children, icon = true }: { job: Job; base?: Job; className?: string; children?: React.ReactNode; icon?: boolean }) {
  const cover = j.cover || base?.cover;
  return (
    <div
      className={cn("relative grid shrink-0 place-items-center overflow-hidden rounded-md bg-muted bg-cover bg-center", className)}
      style={{ backgroundImage: cover ? `url("${cover}")` : gradient((base ?? j).id) }}
    >
      {!cover && icon && <Music className="size-[38%] text-white opacity-70 drop-shadow" strokeWidth={1.75} />}
      {children}
    </div>
  );
}
