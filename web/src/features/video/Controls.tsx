import { RectangleHorizontal, RectangleVertical, Square, Wand2 } from "lucide-react";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Slider } from "@/components/ui/slider";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { cn } from "@/lib/utils";
import { MAX_FRAMES, SCALES, SHAPES, framesFor, presetSize, secondsOf, secs, whichPreset, type Shape } from "./units";

/** A clip's length in seconds at the frame rate (snapped to 8k+1 frames), or Auto: LTX-2.5 picks one for the prompt. */
export function LengthControl({ frames, fps, onChange }: { frames: number; fps: number; onChange: (frames: number) => void }) {
  const auto = frames === 0;
  const shown = auto ? 97 : frames;
  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-3">
        <Slider
          className={cn("flex-1", auto && "opacity-40")}
          min={9}
          max={MAX_FRAMES}
          step={8}
          value={[shown]}
          onValueChange={([v]) => onChange(v)}
          aria-label="Length"
        />
        <button
          type="button"
          onClick={() => onChange(auto ? 97 : 0)}
          title="Let LTX-2.5 choose a length that fits the prompt"
          className={cn(
            "flex h-7 shrink-0 items-center gap-1 rounded-md border px-2 text-xs font-medium transition-colors",
            auto ? "border-primary/60 bg-primary/15 text-primary" : "text-muted-foreground hover:bg-muted",
          )}
        >
          <Wand2 className="size-3.5" /> Auto
        </button>
      </div>
      <div className="flex items-baseline gap-2 text-xs text-muted-foreground">
        {auto ? (
          <span>LTX-2.5 picks a length that fits the prompt.</span>
        ) : (
          <>
            <span className="text-sm font-semibold text-foreground tabular-nums">{secs(secondsOf(frames, fps))}</span>
            <span className="tabular-nums">
              {frames} frames at {fps} fps
            </span>
          </>
        )}
      </div>
    </div>
  );
}

/** The lengths a movie's clip can have, as a compact select. `fallback` is the movie's own (0: Auto). */
export function ClipLength({
  frames,
  fps,
  onChange,
  fallback,
  className,
}: {
  frames: number;
  fps: number;
  onChange: (frames: number) => void;
  /** For a clip: the movie's length, offered as "Movie default". Without it the select is the movie's own. */
  fallback?: number;
  className?: string;
}) {
  const options = [...new Set([1, 2, 3, 4, 5, 6, 8, 10].map((s) => framesFor(s, fps)).concat(frames ? [frames] : []))].sort((a, b) => a - b);
  const isClip = fallback !== undefined;
  const label = (f: number) => secs(secondsOf(f, fps));
  return (
    <Select value={String(frames)} onValueChange={(v) => onChange(+v)}>
      <SelectTrigger size="sm" className={cn("h-7 gap-1 px-2 text-xs", className)} title="Length of this clip">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value="0">{isClip ? `Default (${fallback ? label(fallback) : "auto"})` : "Auto (LTX chooses)"}</SelectItem>
        {options.map((f) => (
          <SelectItem key={f} value={String(f)}>
            {label(f)}
            <span className="ml-1 text-muted-foreground tabular-nums">· {f} frames</span>
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

const shapeIcon: Record<Shape, React.ReactNode> = {
  landscape: <RectangleHorizontal />,
  portrait: <RectangleVertical />,
  square: <Square />,
};

/** Landscape / portrait / square, at three sizes around the model's default. */
export function SizePicker({
  width,
  height,
  base,
  onChange,
}: {
  width: number;
  height: number;
  base: { width: number; height: number };
  onChange: (s: { width: number; height: number }) => void;
}) {
  const now = whichPreset(width, height, base);
  return (
    <div className="flex flex-col gap-2">
      <ToggleGroup
        type="single"
        variant="outline"
        spacing={0}
        className="w-full"
        value={now.shape}
        onValueChange={(v) => v && onChange(presetSize(v as Shape, now.scale ?? "medium", base))}
      >
        {SHAPES.map((s) => (
          <ToggleGroupItem key={s} value={s} className="flex-1 capitalize data-[state=on]:text-primary">
            {shapeIcon[s]} {s}
          </ToggleGroupItem>
        ))}
      </ToggleGroup>
      <ToggleGroup
        type="single"
        variant="outline"
        size="sm"
        spacing={0}
        className="w-full"
        value={now.scale ?? ""}
        onValueChange={(v) => v && onChange(presetSize(now.shape, v as (typeof SCALES)[number]["id"], base))}
      >
        {SCALES.map((s) => {
          const p = presetSize(now.shape, s.id, base);
          return (
            <ToggleGroupItem key={s.id} value={s.id} className="h-auto flex-1 flex-col gap-0 py-1 data-[state=on]:text-primary">
              <span>{s.label}</span>
              <span className="text-[10px] font-normal text-muted-foreground tabular-nums">
                {p.width}×{p.height}
              </span>
            </ToggleGroupItem>
          );
        })}
      </ToggleGroup>
      {!now.scale && (
        <p className="text-xs text-muted-foreground">
          {width}×{height} (from a video you used again) · pick a size to change it
        </p>
      )}
    </div>
  );
}
