import { ArrowDown, ArrowUp, Copy, GripVertical, Link2, MoreHorizontal, Plus, Scissors, Trash2 } from "lucide-react";
import { useRef, useState } from "react";
import { PromptBox } from "@/components/kit/PromptBox";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { cn } from "@/lib/utils";
import { ClipLength } from "./Controls";
import { useDraft, type DraftClip } from "./draft";
import { FirstFrame } from "./FirstFrame";

/** The script of a movie: clip cards, with a cut or a continuation between each two. */
export function MovieEditor() {
  const clips = useDraft((s) => s.clips);
  const addClip = useDraft((s) => s.addClip);
  const moveClip = useDraft((s) => s.moveClip);
  const [dragging, setDragging] = useState<string | null>(null);
  const [target, setTarget] = useState<number | null>(null);
  const list = useRef<HTMLOListElement>(null);

  /** Where a drop at this pointer height would put the dragged clip. */
  const slot = (y: number) => {
    const cards = [...(list.current?.querySelectorAll<HTMLElement>("[data-clip]") ?? [])];
    const i = cards.findIndex((c) => {
      const r = c.getBoundingClientRect();
      return y < r.top + r.height / 2;
    });
    return i < 0 ? cards.length : i;
  };

  return (
    <div className="flex flex-col">
      <ol
        ref={list}
        className="flex flex-col"
        onDragOver={(e) => {
          if (!dragging) return;
          e.preventDefault();
          e.dataTransfer.dropEffect = "move";
          setTarget(slot(e.clientY));
        }}
        onDrop={(e) => {
          if (!dragging) return;
          e.preventDefault();
          const from = clips.findIndex((c) => c.id === dragging);
          const to = slot(e.clientY);
          moveClip(dragging, to > from ? to - 1 : to);
          setDragging(null);
          setTarget(null);
        }}
      >
        {clips.map((c, i) => (
          <li key={c.id} className="relative">
            {i > 0 && <Seam clip={c} />}
            {target === i && dragging && <DropLine />}
            <ClipCard
              clip={c}
              index={i}
              count={clips.length}
              dragging={dragging === c.id}
              onDragStart={() => setDragging(c.id)}
              onDragEnd={() => (setDragging(null), setTarget(null))}
            />
            {target === clips.length && dragging && i === clips.length - 1 && <DropLine bottom />}
          </li>
        ))}
      </ol>
      <Button
        variant="outline"
        className="mt-3 h-10 border-dashed text-muted-foreground"
        onClick={() => {
          addClip();
          // the new card's text box, ready to type
          requestAnimationFrame(() => [...(list.current?.querySelectorAll("textarea") ?? [])].pop()?.focus());
        }}
      >
        <Plus /> Add clip
      </Button>
    </div>
  );
}

function DropLine({ bottom }: { bottom?: boolean }) {
  return <div className={cn("pointer-events-none absolute inset-x-0 z-10 h-0.5 rounded bg-primary", bottom ? "-bottom-1.5" : "-top-px")} />;
}

/**
 * What happens between two clips: the picture flows on from the last frame (a solid line) or the film cuts to a new
 * scene (a gap with scissors). It is also the switch.
 */
function Seam({ clip }: { clip: DraftClip }) {
  const setClip = useDraft((s) => s.setClip);
  const cont = clip.continue;
  return (
    <div className="relative flex h-11 items-center">
      {/* the film strip: continuous, or cut */}
      <div className={cn("absolute top-0 bottom-0 left-[22px] w-0.5", cont ? "bg-primary/60" : "bg-transparent")} />
      {!cont && (
        <>
          <div className="absolute top-0 left-[22px] h-3 w-0.5 bg-border" />
          <div className="absolute bottom-0 left-[22px] h-3 w-0.5 bg-border" />
        </>
      )}
      <div className="relative z-10 ml-10 flex rounded-full border bg-background p-0.5 text-xs shadow-xs">
        <button
          type="button"
          onClick={() => setClip(clip.id, { continue: true })}
          className={cn("flex items-center gap-1 rounded-full px-2.5 py-1 transition-colors", cont ? "bg-primary/15 font-medium text-primary" : "text-muted-foreground hover:text-foreground")}
          title="This clip starts from the last frame of the one before: one continuous shot"
        >
          <Link2 className="size-3.5" /> Continue from the last frame
        </button>
        <button
          type="button"
          onClick={() => setClip(clip.id, { continue: false })}
          className={cn("flex items-center gap-1 rounded-full px-2.5 py-1 transition-colors", !cont ? "bg-amber-500/15 font-medium text-amber-600 dark:text-amber-400" : "text-muted-foreground hover:text-foreground")}
          title="A cut: this clip is a new scene, made from its description alone"
        >
          <Scissors className="size-3.5" /> New scene
        </button>
      </div>
    </div>
  );
}

function ClipCard({
  clip: c,
  index: i,
  count,
  dragging,
  onDragStart,
  onDragEnd,
}: {
  clip: DraftClip;
  index: number;
  count: number;
  dragging: boolean;
  onDragStart: () => void;
  onDragEnd: () => void;
}) {
  const setClip = useDraft((s) => s.setClip);
  const duplicateClip = useDraft((s) => s.duplicateClip);
  const removeClip = useDraft((s) => s.removeClip);
  const moveClip = useDraft((s) => s.moveClip);
  const story = useDraft((s) => s.story);
  const fps = useDraft((s) => s.fps);
  const clipFrames = useDraft((s) => s.clipFrames);
  const movieImage = useDraft((s) => s.movieImage);
  const set = useDraft((s) => s.set);
  // the card can be dragged only by its handle (the text inside stays selectable)
  const [grab, setGrab] = useState(false);
  const cont = i > 0 && c.continue;

  return (
    <div
      data-clip
      draggable={grab}
      onDragStart={(e) => {
        e.dataTransfer.effectAllowed = "move";
        e.dataTransfer.setData("text/plain", c.id);
        onDragStart();
      }}
      onDragEnd={() => (setGrab(false), onDragEnd())}
      className={cn(
        "flex gap-2.5 rounded-xl border bg-card py-3 pr-3 pl-2.5 shadow-xs transition-opacity",
        cont ? "border-primary/40" : "",
        dragging && "opacity-40",
      )}
    >
      <div className="flex flex-col items-center gap-1 pt-0.5">
        <span className={cn("grid size-6 shrink-0 place-items-center rounded-full text-xs font-semibold tabular-nums", cont ? "bg-primary/15 text-primary" : "bg-muted text-foreground")}>
          {i + 1}
        </span>
        <span
          className="cursor-grab touch-none rounded p-0.5 text-muted-foreground/60 hover:bg-muted hover:text-muted-foreground active:cursor-grabbing max-sm:hidden"
          onPointerDown={() => setGrab(true)}
          onPointerUp={() => setGrab(false)}
          title="Drag to reorder"
        >
          <GripVertical className="size-4" />
        </span>
      </div>
      <div className="flex min-w-0 flex-1 flex-col gap-2">
        <PromptBox
          label={i === 0 ? "Opening" : cont ? "Continues the shot" : "New scene"}
          value={c.prompt}
          onChange={(v) => setClip(c.id, { prompt: v })}
          enhance="enhance-video"
          context={story.trim() ? `Characters and look of the whole film: ${story.trim()}\n\nThis shot:` : undefined}
          rows={3}
          className="[&_textarea]:min-h-16 [&_textarea]:resize-y"
          placeholder={i === 0 ? "Wide shot: the squirrel carries a giant acorn across a clearing …" : cont ? "What happens next in the same shot …" : "Cut to … (describe the new scene)"}
        />
        <div className="flex items-center gap-1.5">
          {i === 0 ? (
            <div className="min-w-0 flex-1">
              <FirstFrame compact value={movieImage} onChange={(url) => set({ movieImage: url })} />
            </div>
          ) : (
            <div className="flex-1" />
          )}
          <ClipLength frames={c.frames} fps={fps} fallback={clipFrames} onChange={(f) => setClip(c.id, { frames: f })} />
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon-sm" title="Move, duplicate, delete" className="text-muted-foreground">
                <MoreHorizontal />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem disabled={i === 0} onSelect={() => moveClip(c.id, i - 1)}>
                <ArrowUp /> Move up
              </DropdownMenuItem>
              <DropdownMenuItem disabled={i === count - 1} onSelect={() => moveClip(c.id, i + 1)}>
                <ArrowDown /> Move down
              </DropdownMenuItem>
              <DropdownMenuItem onSelect={() => duplicateClip(c.id)}>
                <Copy /> Duplicate
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem variant="destructive" onSelect={() => removeClip(c.id)}>
                <Trash2 /> Delete clip
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
    </div>
  );
}
