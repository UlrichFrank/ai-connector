import type { Clip, Job } from "@/api/types";

/** Rounds a frame count to 8k+1 (what LTX's VAE wants), at least 9; 0 stays 0 ("let LTX choose"). */
export function frames8k1(n: number): number {
  if (n <= 0) return 0;
  return Math.max(9, Math.round((n - 1) / 8) * 8 + 1);
}

/** The frame count closest to `s` seconds at `fps`. */
export const framesFor = (s: number, fps: number) => frames8k1(Math.round(s * fps) + 1);

/** How long `frames` play at `fps`, in seconds. */
export const secondsOf = (frames: number, fps: number) => (frames > 1 ? (frames - 1) / (fps || 24) : 0);

/** 4 → "4 s", 4.04 → "4.0 s", 72.5 → "1:13". */
export function secs(s: number): string {
  if (s >= 60) return `${Math.floor(s / 60)}:${String(Math.round(s % 60)).padStart(2, "0")}`;
  const r = Math.round(s * 10) / 10;
  return `${Number.isInteger(r) ? r : r.toFixed(1)} s`;
}

/** The longest clip we offer (LTX-2.5 can do more, but memory and time grow fast). */
export const MAX_FRAMES = 257;

/** A movie's clips with their frame counts (0 when LTX chooses) and where each starts in the joined film. */
export function timeline(script: Clip[], movieFrames: number, fps: number) {
  let at = 0;
  let known = true;
  return script.map((c, i) => {
    const frames = frames8k1(c.frames || movieFrames);
    if (!frames) known = false;
    // a continuation starts with the last frame of the one before: the join drops it
    const play = frames ? (i > 0 && c.continue ? frames - 1 : frames) : 0;
    const start = known ? secondsOf(at + 1, fps) : undefined;
    at += play;
    return { clip: c, frames, play, start: known ? start : undefined };
  });
}

/** The joined length of a movie (frames), or undefined when a clip's length is up to LTX. */
export function movieFrames(script: Clip[], movieFrames: number): number | undefined {
  let n = 0;
  for (const [i, c] of script.entries()) {
    const f = frames8k1(c.frames || movieFrames);
    if (!f) return undefined;
    n += i > 0 && c.continue ? f - 1 : f;
  }
  return n;
}

// ---------- sizes ----------

export type Shape = "landscape" | "portrait" | "square";
export type Scale = "small" | "medium" | "large";

export const SHAPES: Shape[] = ["landscape", "portrait", "square"];
export const SCALES: { id: Scale; label: string; factor: number }[] = [
  { id: "small", label: "Quick", factor: 0.72 },
  { id: "medium", label: "Standard", factor: 1 },
  { id: "large", label: "Large", factor: 1.37 },
];

const r64 = (n: number) => Math.max(256, Math.round(n / 64) * 64);

/** A preset size, from the model's default (704×448 for LTX-2.5); multiples of 64 (its two-stage pipeline). */
export function presetSize(shape: Shape, scale: Scale, base: { width: number; height: number }): { width: number; height: number } {
  const f = SCALES.find((s) => s.id === scale)!.factor;
  const long = Math.max(base.width, base.height);
  const short = Math.min(base.width, base.height);
  if (shape === "square") {
    const side = r64(Math.sqrt(long * short) * f);
    return { width: side, height: side };
  }
  const [w, h] = [r64(long * f), r64(short * f)];
  return shape === "landscape" ? { width: w, height: h } : { width: h, height: w };
}

/** Which preset a size is, if any. */
export function whichPreset(width: number, height: number, base: { width: number; height: number }): { shape: Shape; scale?: Scale } {
  for (const shape of SHAPES)
    for (const s of SCALES) {
      const p = presetSize(shape, s.id, base);
      if (p.width === width && p.height === height) return { shape, scale: s.id };
    }
  return { shape: width > height ? "landscape" : width < height ? "portrait" : "square" };
}

// ---------- jobs ----------

/** Clips done, the clip being rendered (0-based), and how many, for a movie job. */
export function clipState(j: Job): { done: number; current: number; total: number } {
  const total = Number(j.stats?.clips) || j.request.script?.length || 0;
  const done = Number(j.stats?.clipsDone) || 0;
  const m = j.status?.match(/^clip (\d+)\/(\d+)/);
  const current = m ? +m[1] - 1 : done;
  return { done, current, total };
}

/** The status without the "clip 2/5 · " in front (the strip shows that). */
export const clipStatus = (j: Job) => (j.status ?? "").replace(/^clip \d+\/\d+ · /, "");
