import type { Job } from "@/api/types";
import { isMovie } from "@/lib/format";

/** The file a job made (its picture, video or song), if it is there yet. */
export const mainFile = (j: Job): string | undefined =>
  // an avatar may also keep the speech it made first: its video is the mp4
  j.kind === "avatar" ? (j.files?.find((f) => /\.(mp4|mov|webm)$/i.test(f)) ?? undefined) : j.files?.[0];

/** Images and videos (avatars too) open in the viewer; songs live in the studio, speech and sounds play. */
export const isVisual = (j: Job) => j.kind === "image" || j.kind === "video" || j.kind === "avatar";

/** Speech and sound effects: audio that plays in the bar at the bottom. */
export const isAudio = (j: Job) => j.kind === "speech" || j.kind === "sound";

export type MediaType = "image" | "video" | "song" | "audio";

export function mediaType(j: Job): MediaType {
  return j.kind === "music" ? "song" : j.kind === "video" || j.kind === "avatar" ? "video" : isAudio(j) ? "audio" : "image";
}

const num = (v: unknown): number | undefined => (typeof v === "number" && isFinite(v) && v > 0 ? v : undefined);

/** Width and height: what was made (stats), else what was asked for, else a square. */
export function dims(j: Job): { w: number; h: number } {
  const w = num(j.stats?.width) ?? j.request.width ?? 1024;
  const h = num(j.stats?.height) ?? j.request.height ?? 1024;
  return { w, h };
}

/** The seed that was used (the server picks one when none was asked for). */
export const seedOf = (j: Job): number | undefined => num(j.stats?.seed) ?? (j.request.seed || undefined);
export const stepsOf = (j: Job): number | undefined => num(j.stats?.steps) ?? (j.request.steps || undefined);
/** Seconds of the model's own work (without loading), when the backend says. */
export const secondsOf = (j: Job): number | undefined => num(j.stats?.seconds);

/** A video's length in seconds, from its frames and fps. */
export function videoSeconds(j: Job): number | undefined {
  const frames = num(j.stats?.frames) ?? j.request.frames ?? j.request.script?.reduce((n, c) => n + (c.frames ?? 0), 0);
  const fps = num(j.stats?.fps) ?? j.request.fps ?? 24;
  return frames ? frames / fps : undefined;
}

/** "Image", "Video", "Movie", "Song". */
export function typeLabel(j: Job): string {
  if (j.kind === "music") return "Song";
  if (j.kind === "video") return isMovie(j) ? "Movie" : "Video";
  if (j.kind === "avatar") return "Avatar";
  if (j.kind === "speech") return "Speech";
  if (j.kind === "sound") return "Sound effect";
  return "Image";
}

/** The text a job was made from: its prompt, a movie's scenes, or what was said (speech, an avatar without a scene). */
export function promptOf(j: Job): string {
  if (j.kind === "speech") return j.request.text || "";
  return j.request.prompt || j.request.script?.map((c) => c.prompt).join("\n\n") || j.request.text || "";
}

/** Saves a file under its own name. */
export function download(url: string) {
  const a = document.createElement("a");
  a.href = url;
  a.download = url.split("/").pop() ?? "download";
  document.body.appendChild(a);
  a.click();
  a.remove();
}

/** A calm gradient that is always the same for a job: the art of a song without a cover. */
export function gradientFor(id: string): string {
  let h = 0;
  for (const c of id) h = (h * 31 + c.charCodeAt(0)) >>> 0;
  const a = h % 360;
  const b = (a + 40 + (h >> 9) % 80) % 360;
  return `radial-gradient(120% 120% at 15% 10%, oklch(0.72 0.14 ${a}) 0%, transparent 60%), linear-gradient(135deg, oklch(0.45 0.16 ${a}) 0%, oklch(0.3 0.12 ${b}) 100%)`;
}
