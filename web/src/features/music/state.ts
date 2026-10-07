// The studio's own state (the open folder, the form, the waveform selection) and its actions.
import { useMemo } from "react";
import { toast } from "sonner";
import { create } from "zustand";
import { api, errorText } from "@/api/client";
import type { Job } from "@/api/types";
import { navigate } from "@/lib/route";
import { local } from "@/lib/storage";
import { useApp } from "@/store/app";
import { usePlayer } from "@/store/player";
import { allFolders, DEFAULT_LENGTH, families, STARRED } from "./songs";

export interface Draft {
  mode: "simple" | "custom";
  idea: string;
  ideaInstrumental: boolean;
  title: string;
  style: string;
  lyrics: string;
  instrumental: boolean;
  duration: number;
  seed: string;
  /** A trained style (StylesPage) for the chosen model, and its strength. */
  trainedStyle: string;
  styleStrength: number;
  /** the tempo ("" = from the style) */
  bpm: string;
  lockTempo: boolean;
  takes: number;
}

export const emptyDraft: Draft = {
  mode: "simple",
  idea: "",
  ideaInstrumental: false,
  title: "",
  style: "",
  lyrics: "",
  instrumental: false,
  duration: DEFAULT_LENGTH,
  seed: "",
  trainedStyle: "",
  styleStrength: 1,
  bpm: "",
  lockTempo: true,
  takes: 1,
};

/** A part of a song, in seconds. */
export interface Selection {
  song: string;
  start: number;
  end: number;
}

interface Studio {
  /** The open folder ("" = all songs, STARRED = the ★ view). */
  folder: string;
  /** Folders made here that have no song yet (the server only knows folders through songs). */
  newFolders: string[];
  search: string;
  draft: Draft;
  /** A model to pick in the form once it shows (Load into the form). */
  loadModel: string | null;
  /** Briefly true after Load into the form (the form flashes). */
  loaded: number;
  sel: Selection | null;
  tool: string;
  setFolder: (f: string) => void;
  setSearch: (q: string) => void;
  setDraft: (d: Partial<Draft>) => void;
  setSel: (s: Selection | null) => void;
  setTool: (t: string) => void;
  addFolder: (f: string) => void;
  renameNewFolders: (from: string, to: string) => void;
  load: (d: Partial<Draft>, model: string | null) => void;
}

export const useStudio = create<Studio>((set, get) => ({
  folder: local.get("music.folder", ""),
  newFolders: local.get<string[]>("music.newFolders", []),
  search: "",
  draft: { ...emptyDraft, ...local.get<Partial<Draft>>("music.draft", {}) },
  loadModel: null,
  loaded: 0,
  sel: null,
  tool: local.get("music.tool", "remix"),
  setFolder: (folder) => (local.set("music.folder", folder), set({ folder })),
  setSearch: (search) => set({ search }),
  setDraft: (d) => {
    const draft = { ...get().draft, ...d };
    local.set("music.draft", draft);
    set({ draft });
  },
  setSel: (sel) => set({ sel }),
  setTool: (tool) => (local.set("music.tool", tool), set({ tool })),
  addFolder: (f) => {
    const newFolders = [...new Set([...get().newFolders, f])];
    local.set("music.newFolders", newFolders);
    set({ newFolders });
  },
  renameNewFolders: (from, to) => {
    const newFolders = get()
      .newFolders.map((x) => (x === from || x.startsWith(from + "/") ? (to ? to + x.slice(from.length) : "") : x))
      .filter(Boolean);
    local.set("music.newFolders", newFolders);
    set({ newFolders });
  },
  load: (d, model) => {
    const draft = { ...get().draft, ...d, mode: "custom" as const };
    local.set("music.draft", draft);
    set({ draft, loadModel: model, loaded: Date.now() });
  },
}));

// ---------- the songs ----------

/** Every song (a music job), newest first, and the families they form. */
export function useSongs() {
  const jobs = useApp((s) => s.jobs);
  return useMemo(() => {
    const songs = Object.values(jobs)
      .filter((j) => j.kind === "music" && j.request.task !== "train-style") // a style's training is no song
      .sort((a, b) => b.created.localeCompare(a.created));
    return { jobs, songs, families: families(songs, jobs) };
  }, [jobs]);
}

export function useFolders(songs: Job[]) {
  const extra = useStudio((s) => s.newFolders);
  // the same array while the folders stay the same (the rows that get it are memoised)
  const key = useMemo(() => allFolders(songs, extra).join("\n"), [songs, extra]);
  return useMemo(() => key.split("\n").filter(Boolean), [key]);
}

/** The image job painting a song's cover, while it runs. */
export function useCoverJob(id: string): Job | undefined {
  const jobs = useApp((s) => s.jobs);
  return useMemo(() => Object.values(jobs).find((x) => x.request.coverFor === id && (x.state === "queued" || x.state === "running")), [jobs, id]);
}

// ---------- actions ----------

export async function moveSong(id: string, folder: string) {
  if (folder === STARRED) return;
  try {
    await api.move(id, folder); // the whole family; the event stream brings the changed jobs
  } catch (e) {
    toast.error("Could not move", { description: errorText(e) });
  }
}

export async function renameFolder(from: string, to: string) {
  try {
    await api.renameFolder(from, to);
    useStudio.getState().renameNewFolders(from, to);
    return true;
  } catch (e) {
    toast.error(to ? "Could not rename the folder" : "Could not remove the folder", { description: errorText(e) });
    return false;
  }
}

export async function setCover(j: Job, image: string) {
  try {
    useApp.getState().put(await api.setCover(j.id, image));
  } catch (e) {
    toast.error("Cover", { description: errorText(e) });
  }
}

// ---------- playing a part ----------

let stopAt: { file: string; start: number; end: number; inside?: boolean } | null = null;
usePlayer.subscribe((s) => {
  if (!stopAt) return;
  if (s.file !== stopAt.file) return void (stopAt = null); // another song
  const inside = s.time >= stopAt.start - 0.5 && s.time <= stopAt.end + 1;
  if (!stopAt.inside) return void (stopAt.inside = inside); // still loading / seeking there
  // the listener jumped away from the part: play on
  if (!inside) stopAt = null;
  else if (!s.paused && s.time >= stopAt.end) {
    stopAt = null;
    s.pause();
  }
});

/** Plays a part of a song and stops at its end. */
export function playRange(j: Job, file: string, start: number, end: number) {
  usePlayer.getState().play(j, file, start);
  stopAt = { file, start, end };
}

/** Is this a drag of files from outside (Finder), not one of our songs? */
export const draggingFiles = (e: { dataTransfer: DataTransfer }) => [...e.dataTransfer.types].includes("Files");

/**
 * Imports audio files (dropped or picked) into a folder as songs of their own; opens the song when it was one.
 * Other files are skipped with a note.
 */
export async function importFiles(files: File[], folder: string) {
  const audio = files.filter((f) => f.type.startsWith("audio/") || /\.(mp3|wav|m4a|flac|ogg|aac|aiff?)$/i.test(f.name));
  if (audio.length < files.length) toast.message(`${files.length - audio.length} file(s) skipped: only audio can be imported`);
  if (!audio.length) return;
  const into = folder === STARRED ? "" : folder;
  const id = toast.loading(audio.length > 1 ? `Importing ${audio.length} songs …` : `Importing ${audio[0].name} …`);
  let last: string | undefined;
  let done = 0;
  for (const f of audio) {
    try {
      const r = await api.upload(f, { import: true, folder: into || undefined });
      if (r.job) {
        useApp.getState().put(r.job);
        last = r.job.id;
        done++;
      }
    } catch (err) {
      toast.error(`Import of ${f.name} failed`, { description: errorText(err) });
    }
  }
  toast.success(done === 1 ? "Imported — remix, extend or split it in the studio" : `${done} songs imported`, { id });
  if (done === 1 && last) navigate("music", last);
}
