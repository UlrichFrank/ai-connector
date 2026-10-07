import { create } from "zustand";
import type { Clip, Example, Job, Request } from "@/api/types";
import { local } from "@/lib/storage";

/** What the composer is writing: one clip, or a movie of several. */
export type Mode = "clip" | "movie";

/** A clip of the movie being written (`id` only keeps the cards stable while they move). */
export interface DraftClip {
  id: string;
  prompt: string;
  /** 0: the movie's clip length. */
  frames: number;
  /** Starts from the last frame of the clip before (otherwise a cut: a new scene). Ignored on the first clip. */
  continue: boolean;
}

export interface Draft {
  mode: Mode;
  // clip
  prompt: string;
  /** The first frame (a /files/ url from api.upload), or "". */
  image: string;
  /** 0: LTX-2.5 chooses the length. */
  frames: number;
  // movie
  story: string;
  clips: DraftClip[];
  /** Every clip's length unless it has its own (0: LTX chooses). */
  clipFrames: number;
  /** The movie's opening frame (the first clip starts from it), or "". */
  movieImage: string;
  // both (0 = the model's default)
  width: number;
  height: number;
  fps: number;
  /** 0: a new random one each time. */
  seed: number;
  sound: boolean;
  lowRam: boolean;
}

export const newId = () => Math.random().toString(36).slice(2, 10);
export const newClip = (c: Partial<DraftClip> = {}): DraftClip => ({ id: newId(), prompt: "", frames: 0, continue: true, ...c });

const blank: Draft = {
  mode: "clip",
  prompt: "",
  image: "",
  frames: 97,
  story: "",
  clips: [newClip({ continue: false })],
  clipFrames: 97,
  movieImage: "",
  width: 0,
  height: 0,
  fps: 24,
  seed: 0,
  sound: true,
  lowRam: false,
};

const KEY = "video.draft";

interface DraftState extends Draft {
  set: (d: Partial<Draft>) => void;
  setClip: (id: string, c: Partial<DraftClip>) => void;
  addClip: () => void;
  duplicateClip: (id: string) => void;
  removeClip: (id: string) => void;
  /** Moves a clip to another place in the list. */
  moveClip: (id: string, to: number) => void;
  /** Starts the movie over (an empty first clip). */
  clearMovie: () => void;
  /** Fills the composer from an example. */
  example: (e: Example) => void;
  /** Fills the composer from a job's request ("Use again"). */
  load: (r: Request) => void;
}

function save(s: Draft) {
  const d: Record<string, unknown> = {};
  for (const k of Object.keys(blank)) d[k] = s[k as keyof Draft];
  local.set(KEY, d);
}

function size(s: string | undefined): Partial<Draft> {
  const m = s?.match(/^(\d+)x(\d+)$/);
  return m ? { width: +m[1], height: +m[2] } : {};
}

const fromScript = (script: Clip[]): DraftClip[] =>
  script.map((c, i) => newClip({ prompt: c.prompt, frames: c.frames ?? 0, continue: i > 0 && !!c.continue }));

export const useDraft = create<DraftState>((set, get) => {
  const kept = local.get<Partial<Draft>>(KEY, {});
  const start: Draft = { ...blank, ...kept };
  if (!start.clips?.length) start.clips = blank.clips;
  start.clips = start.clips.map((c) => ({ ...newClip(), ...c, id: c.id || newId() }));

  // every change is kept at once: a reload does not lose the script
  const put = (d: Partial<Draft>) => {
    set(d);
    save(get());
  };

  return {
    ...start,
    set: put,
    setClip: (id, c) => put({ clips: get().clips.map((x) => (x.id === id ? { ...x, ...c } : x)) }),
    addClip: () => put({ clips: [...get().clips, newClip()] }),
    duplicateClip: (id) => {
      const clips = get().clips;
      const i = clips.findIndex((c) => c.id === id);
      if (i < 0) return;
      put({ clips: [...clips.slice(0, i + 1), { ...clips[i], id: newId(), continue: true }, ...clips.slice(i + 1)] });
    },
    removeClip: (id) => {
      const clips = get().clips.filter((c) => c.id !== id);
      put({ clips: clips.length ? clips : [newClip({ continue: false })] });
    },
    moveClip: (id, to) => {
      const clips = [...get().clips];
      const from = clips.findIndex((c) => c.id === id);
      if (from < 0 || to < 0 || to >= clips.length || from === to) return;
      const [c] = clips.splice(from, 1);
      clips.splice(to, 0, c);
      put({ clips });
    },
    clearMovie: () => put({ story: "", clips: [newClip({ continue: false })], movieImage: "" }),
    example: (e) => {
      if (e.script?.length) put({ mode: "movie", story: e.prompt ?? "", clips: fromScript(e.script), clipFrames: e.frames ?? 97, movieImage: "", ...size(e.size) });
      else put({ mode: "clip", prompt: e.prompt ?? "", frames: e.frames ?? 97, image: "", ...size(e.size) });
    },
    load: (r) => {
      const common: Partial<Draft> = {
        width: r.width ?? 0,
        height: r.height ?? 0,
        fps: r.fps || 24,
        seed: r.seed ?? 0,
        sound: !r.noAudio,
        lowRam: !!r.lowRam,
      };
      if (r.script?.length)
        put({ ...common, mode: "movie", story: r.prompt ?? "", clips: fromScript(r.script), clipFrames: r.frames ?? 0, movieImage: r.images?.[0] ?? "" });
      else put({ ...common, mode: "clip", prompt: r.prompt ?? "", frames: r.frames ?? 0, image: r.images?.[0] ?? "" });
    },
  };
});

/** The request the composer makes (without the model). */
export function request(d: Draft, width: number, height: number): Omit<Request, "model"> {
  const common = {
    width,
    height,
    fps: d.fps,
    seed: d.seed || undefined,
    noAudio: !d.sound || undefined,
    lowRam: d.lowRam || undefined,
  };
  if (d.mode === "movie")
    return {
      ...common,
      prompt: d.story.trim(),
      frames: d.clipFrames,
      images: d.movieImage ? [d.movieImage] : undefined,
      script: d.clips.map((c, i) => ({ prompt: c.prompt.trim(), frames: c.frames || undefined, continue: (i > 0 && c.continue) || undefined })),
    };
  return { ...common, prompt: d.prompt.trim(), frames: d.frames, images: d.image ? [d.image] : undefined };
}

/** The request's model, so "Use again" can pick it too. */
export const jobModel = (j: Job) => j.request.model || j.model;
