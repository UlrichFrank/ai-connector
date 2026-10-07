// Songs: what the studio knows about a music job (title, length, art, folders, families of versions).
import type { Job, Model, Request } from "@/api/types";
import { isActive } from "@/store/app";

export const DEFAULT_LENGTH = 210;
/** The pseudo folder of the ★ view. */
export const STARRED = "\u0000starred";

/** A song's title: the one it was given, else the first sung line, else the start of its style. */
export function songTitle(j: Job): string {
  const r = j.request;
  if (r.title) return r.title;
  const line = (r.lyrics ?? "")
    .split("\n")
    .map((l) => l.trim())
    .find((l) => l && !l.startsWith("["));
  return line || (r.prompt || "Untitled").split(",")[0];
}

export const songFile = (j: Job): string | undefined => j.files?.[0];
export const songLen = (j: Job): number => Number(j.stats?.duration) || j.request.duration || 0;
/** The seed a song was made with (the server picks one when none was asked for). */
export const songSeed = (j: Job): number | undefined => Number(j.stats?.seed) || j.request.seed || undefined;
/** YuE2 songs carry their melody as an ABC score. */
export const songScore = (j: Job): string | undefined => (j.stats?.score as string | undefined) || j.score || undefined;
export const isUpload = (j: Job) => j.model === "upload";
export const folderOf = (j: Job) => j.request.folder ?? "";
export const inFolder = (j: Job, f: string) => folderOf(j) === f || folderOf(j).startsWith(f + "/");
export const leaf = (f: string) => f.split("/").pop() ?? f;
export const parentFolder = (f: string) => (f.includes("/") ? f.slice(0, f.lastIndexOf("/")) : "");

// ---------- the studio's tools ----------

export type Task = NonNullable<Request["task"]>;
export type ToolId = "remix" | "replace" | "extend" | "lego" | "stems" | "vary" | "restyle";

export interface Tool {
  id: ToolId;
  task: Task;
  label: string;
  note: string;
}

export const TOOLS: Tool[] = [
  { id: "remix", task: "cover", label: "Remix", note: "A new style over this song: the structure and melody stay, the sound changes." },
  { id: "replace", task: "repaint", label: "Replace section", note: "Select a part on the waveform; it is made again, the rest stays as it is." },
  { id: "extend", task: "extend", label: "Extend", note: "Adds more music after the end, continuing the song." },
  { id: "lego", task: "lego", label: "Add instrument", note: "Plays a new instrument track over the song, or over the selected part." },
  { id: "stems", task: "extract", label: "Stems", note: "Pulls one instrument out of the mix as its own track." },
  { id: "vary", task: "generate", label: "Variations", note: "The same song request again, each with a new seed." },
  { id: "restyle", task: "restyle", label: "Restyle melody", note: "Sings this song's melody score again in a new style: the tune and the lyrics stay. Edit the score to change the melody itself." },
];

/** What made a version, in a word. */
export function versionLabel(j: Job): string {
  if (!j.request.parent) return isUpload(j) ? "Imported" : "Original";
  return (
    ({ cover: "Remix", repaint: "New section", extend: "Extended", lego: "+ Instrument", extract: "Stem", restyle: "Restyled", generate: "Variation" } as Record<string, string>)[j.request.task ?? "generate"] ?? "Version"
  );
}

export const TRACKS = ["vocals", "backing_vocals", "drums", "bass", "guitar", "keyboard", "percussion", "strings", "synth", "fx", "brass", "woodwinds"];
export const trackName = (t: string) => t.replace("_", " ").replace(/^./, (c) => c.toUpperCase());

/** Can this model do a task? Every music model can make a song ("generate"). */
export const canDo = (m: Model, task: Task) => task === "generate" || !!m.tasks?.includes(task);

// ---------- art ----------

/** A colour per song: FNV-1a over its id, spread over the hue circle. */
export function hue(id: string): number {
  let x = 2166136261;
  for (const c of id) {
    x ^= c.charCodeAt(0);
    x = Math.imul(x, 16777619) >>> 0;
  }
  return x % 360;
}

export function gradient(id: string): string {
  const h = hue(id);
  return `linear-gradient(135deg, hsl(${h} 70% 58%), hsl(${(h + 55) % 360} 65% 32%))`;
}

// ---------- families: a song and every version made from it ----------

export interface Family {
  root: Job;
  members: Job[];
  /** The newest member that is still being made, if any. */
  busy?: Job;
}

/** The original of a version (as far as its parents are still there). */
export function rootOf(j: Job, jobs: Record<string, Job>): Job {
  let r = j;
  const seen = new Set<string>();
  while (r.request.parent && jobs[r.request.parent] && !seen.has(r.id)) {
    seen.add(r.id);
    r = jobs[r.request.parent];
  }
  return r;
}

/** Songs grouped into families, newest family first (by its newest member). */
export function families(songs: Job[], jobs: Record<string, Job>): Family[] {
  const by = new Map<string, Family>();
  for (const s of songs) {
    const root = rootOf(s, jobs);
    let f = by.get(root.id);
    if (!f) by.set(root.id, (f = { root, members: [] }));
    f.members.push(s);
    if (isActive(s) && !f.busy) f.busy = s;
  }
  const latest = (f: Family) => f.members.reduce((a, m) => (m.created > a ? m.created : a), "");
  return [...by.values()].sort((a, b) => latest(b).localeCompare(latest(a)));
}

/** A song's version tree, depth-first: [job, depth]. */
export function tree(root: Job, songs: Job[]): [Job, number][] {
  const kids = new Map<string, Job[]>();
  for (const s of songs) {
    const p = s.request.parent;
    if (!p) continue;
    if (!kids.has(p)) kids.set(p, []);
    kids.get(p)!.push(s);
  }
  for (const k of kids.values()) k.sort((a, b) => a.created.localeCompare(b.created));
  const out: [Job, number][] = [];
  const walk = (j: Job, d: number) => {
    out.push([j, d]);
    for (const k of kids.get(j.id) ?? []) walk(k, d + 1);
  };
  walk(root, 0);
  return out;
}

/** Every folder: those songs are in (and their parents), plus the new empty ones. */
export function allFolders(songs: Job[], extra: string[]): string[] {
  const set = new Set(extra);
  for (const j of songs) {
    const parts = folderOf(j).split("/").filter(Boolean);
    for (let i = 1; i <= parts.length; i++) set.add(parts.slice(0, i).join("/"));
  }
  return [...set].sort((a, b) => a.localeCompare(b));
}

// ---------- writing ----------

/**
 * The tempo a style names: "124 bpm", or "BPM: 120" / "bpm is 92" as MiniMax Music 3's structured captions write it
 * (the same pattern as styleBPM in internal/engine/music.go); undefined if none.
 */
export function styleBPM(style: string): string | undefined {
  const m = /\b(\d{2,3})(?:\.\d+)?\s*-?\s*bpm\b|\bbpm(?:\s*[:=]\s*|\s+is\s+|\s+of\s+|\s+)(\d{2,3})(?:\.\d+)?\b/i.exec(style);
  return m ? (m[1] ?? m[2]) : undefined;
}

/**
 * Estimates a song's length from its lyrics: a sung line is about two bars, sections without words get the length
 * such parts usually have; the tempo comes from the style ("124 bpm"), else 110.
 */
export function fitLength(style: string, lyrics: string): { seconds: number; bpm: number } {
  const bpm = +(styleBPM(style) ?? 110);
  const beatsFor: Record<string, number> = {
    intro: 16, outro: 16, break: 16, breakdown: 16, "build-up": 16, buildup: 16, drop: 32, inst: 16, instrumental: 32, interlude: 16, solo: 32,
  };
  let beats = 0;
  for (const raw of lyrics.split("\n")) {
    const l = raw.trim();
    if (!l) continue;
    const tag = l.match(/^\[([^\]]+)\]$/)?.[1];
    if (tag) {
      const t = tag.toLowerCase().replace(/[^a-z-]/g, "").replace(/-?(short|medium|long)$/, "");
      beats += beatsFor[t] ?? 0;
    } else beats += 8;
  }
  if (!beats) beats = 64 * 4; // instrumental: about four 16-bar parts
  beats += 8; // a breath at the end
  return { seconds: Math.min(600, Math.max(20, Math.round((beats * 60) / bpm / 5) * 5)), bpm };
}

/** Parses the "write-song" answer: TITLE: …, STYLE: …, LYRICS: … */
export function parseSong(text: string): { title: string; style: string; lyrics: string } {
  const close = text.indexOf("</think>");
  if (close >= 0) text = text.slice(close + 8);
  const title = (text.match(/TITLE:\s*(.+)/i)?.[1] ?? "").trim().replace(/^["“*]+|["”*]+$/g, "");
  const style = (text.match(/STYLE:\s*(.+)/i)?.[1] ?? "").trim();
  const li = text.search(/LYRICS:\s*/i);
  const lyrics = li >= 0 ? text.slice(li).replace(/^LYRICS:\s*/i, "").trim() : "";
  return { title, style, lyrics };
}

/** "1:23.4" for a time in a song (finer than lib/format's duration). */
export function clock(s: number): string {
  if (!isFinite(s) || s < 0) s = 0;
  const t = Math.round(s * 10);
  return `${Math.floor(t / 600)}:${((t % 600) / 10).toFixed(1).padStart(4, "0")}`;
}

/** "3:30" */
export function mmss(s: number): string {
  const t = Math.round(s);
  return `${Math.floor(t / 60)}:${String(t % 60).padStart(2, "0")}`;
}
