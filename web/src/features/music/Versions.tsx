import { GitBranch, Loader2, TriangleAlert } from "lucide-react";
import { memo, useMemo } from "react";
import type { Job } from "@/api/types";
import { StarButton } from "@/components/kit/JobBits";
import { ago, progress } from "@/lib/format";
import { navigate } from "@/lib/route";
import { cn } from "@/lib/utils";
import { isActive } from "@/store/app";
import { PlayButton } from "./Library";
import { mmss, rootOf, songLen, tree, versionLabel } from "./songs";

/** The song's family as a tree: the original, what was made from it, and from those. */
export function Versions({ job: j, songs, jobs }: { job: Job; songs: Job[]; jobs: Record<string, Job> }) {
  const rows = useMemo(() => tree(rootOf(j, jobs), songs), [j, jobs, songs]);
  return (
    <section className="flex flex-col gap-2">
      <h3 className="flex items-center gap-1.5 text-xs font-semibold tracking-wide text-muted-foreground">
        <GitBranch className="size-3.5" /> Versions <span className="font-normal tabular-nums">{rows.length}</span>
      </h3>
      <div className="flex flex-col rounded-xl border bg-card/40 p-1">
        {rows.map(([v, depth]) => (
          <VersionRow key={v.id} job={v} depth={depth} current={v.id === j.id} />
        ))}
      </div>
      {rows.length < 2 && <p className="px-1 text-xs text-muted-foreground">Every change you make with the tools becomes a new version here; the original stays.</p>}
    </section>
  );
}

const VersionRow = memo(function VersionRow({ job: v, depth, current }: { job: Job; depth: number; current: boolean }) {
  const busy = isActive(v);
  const failed = v.state === "failed" || v.state === "canceled";
  const p = busy ? progress(v) : undefined;
  const r = v.request;
  const range = (r.task === "repaint" || r.task === "lego") && r.end ? ` ${mmss(r.start ?? 0)}–${mmss(r.end)}` : r.task === "extend" && r.extend ? ` +${r.extend} s` : r.track ? ` ${r.track.replace("_", " ")}` : "";
  return (
    <div
      role="button"
      tabIndex={0}
      onClick={() => navigate("music", v.id)}
      onKeyDown={(e) => e.key === "Enter" && e.target === e.currentTarget && navigate("music", v.id)}
      className={cn("group relative flex min-h-10 items-center gap-2 rounded-lg py-1 pr-1 text-sm transition-colors hover:bg-accent/60", current && "bg-accent hover:bg-accent")}
      style={{ paddingLeft: 6 + depth * 18 }}
    >
      {depth > 0 && (
        <span className="pointer-events-none absolute top-0 bottom-1/2 w-2.5 rounded-bl-md border-b border-l border-foreground/20" style={{ left: depth * 18 - 8 }} />
      )}
      <div className="grid size-7 shrink-0 place-items-center">
        {busy ? (
          <Loader2 className="size-4 animate-spin text-primary" />
        ) : failed ? (
          <TriangleAlert className="size-4 text-destructive" />
        ) : (
          <PlayButton job={v} className="size-7" />
        )}
      </div>
      <div className="min-w-0 flex-1">
        <div className={cn("truncate", current && "font-medium")}>
          {versionLabel(v)}
          <span className="text-muted-foreground">{range}</span>
        </div>
        <div className="truncate text-xs text-muted-foreground">
          {busy ? (v.state === "queued" ? "waiting for the GPU" : p != null ? `${Math.round(p * 100)}% · ${v.status ?? ""}` : (v.status ?? "starting")) : failed ? (v.problem?.title ?? "Failed") : [v.name, songLen(v) ? mmss(songLen(v)) : "", ago(v.created)].filter(Boolean).join(" · ")}
        </div>
        {busy && (
          <div className="mt-1 h-0.5 overflow-hidden rounded-full bg-primary/15">
            <div className={cn("h-full bg-primary transition-[width]", p == null && "w-1/3 animate-pulse")} style={p != null ? { width: `${p * 100}%` } : undefined} />
          </div>
        )}
      </div>
      {!busy && !failed && <StarButton job={v} className={cn(!v.starred && "opacity-0 group-hover:opacity-100 max-lg:opacity-100")} />}
    </div>
  );
});
