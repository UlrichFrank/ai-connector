import { useEffect } from "react";
import { create } from "zustand";
import { api } from "@/api/client";
import type { Job, Model, Voice } from "@/api/types";
import { isTranscription } from "@/lib/format";
import { useApp } from "@/store/app";

/** The voice library (GET /api/voices), shared by the Voice and Avatar pages. */
interface VoicesState {
  voices: Voice[];
  presets: Record<string, string[]>;
  loaded: boolean;
  error: unknown;
  refresh: () => Promise<void>;
  put: (v: Voice) => void;
  drop: (id: string) => void;
}

export const useVoices = create<VoicesState>((set, get) => ({
  voices: [],
  presets: {},
  loaded: false,
  error: null,
  refresh: async () => {
    try {
      const r = await api.voices();
      set({ voices: r.voices, presets: r.presets ?? {}, loaded: true, error: null });
    } catch (e) {
      set({ loaded: true, error: e });
    }
  },
  put: (v) => {
    const l = get().voices;
    set({ voices: l.some((x) => x.id === v.id) ? l.map((x) => (x.id === v.id ? v : x)) : [v, ...l] });
  },
  drop: (id) => set({ voices: get().voices.filter((v) => v.id !== id) }),
}));

/** Loads the library once a page needs it (and again when the connection comes back). */
export function useVoiceLibrary() {
  const live = useApp((s) => s.connection === "live");
  const refresh = useVoices((s) => s.refresh);
  useEffect(() => {
    if (live) void refresh();
  }, [live, refresh]);
  return useVoices();
}

/** The model that writes down what a recording says: a speech model, but not one to speak with. */
export const isAsr = (m: Model) => m.tasks?.includes("transcribe") || /asr|whisper/i.test(m.id);
/** Speech models to speak with. */
export const speaks = (m: Model) => !isAsr(m);

/** The transcription model, if the Mac has one. */
export function asrModel(): Model | undefined {
  const all = useApp.getState().models.filter((m) => m.kind === "speech" && isAsr(m));
  return all.find((m) => m.here) ?? all[0];
}

/** Speech jobs that speak (not transcriptions of voice samples). */
export const isSpoken = (j: Job) => j.kind === "speech" && !isTranscription(j);

export const languages: { id: string; label: string }[] = [
  { id: "auto", label: "Detect from the text" },
  { id: "en", label: "English" },
  { id: "de", label: "German" },
  { id: "fr", label: "French" },
  { id: "es", label: "Spanish" },
  { id: "it", label: "Italian" },
  { id: "pt", label: "Portuguese" },
  { id: "nl", label: "Dutch" },
  { id: "pl", label: "Polish" },
  { id: "ru", label: "Russian" },
  { id: "ja", label: "Japanese" },
  { id: "ko", label: "Korean" },
  { id: "zh", label: "Chinese" },
];

export const languageName = (id?: string) => (id && id !== "auto" ? (languages.find((l) => l.id === id)?.label ?? id) : "");

/** Quick ways to say how it should sound (the instruct field). */
export const moods: { label: string; text: string }[] = [
  { label: "Calm", text: "Calm and warm, unhurried, relaxed." },
  { label: "Excited", text: "Excited and energetic, a big smile in the voice." },
  { label: "Whispering", text: "Whispering softly, close to the microphone." },
  { label: "Sad", text: "Sad and quiet, a little hesitant, with heavy pauses." },
  { label: "Newsreader", text: "A clear, neutral newsreader: steady pace, crisp articulation." },
  { label: "Storyteller", text: "A storyteller reading to children: lively, expressive, with dramatic pauses." },
];

/** Roughly how long a text takes to say: ~15 characters a second at normal speed. */
export function estimateSeconds(text: string, speed = 1): number {
  const chars = text.replace(/\s+/g, " ").trim().length;
  return chars / 15 / (speed || 1);
}

/** The name of a voice in a request: a library voice by its id, else the preset's name, else the model's own. */
export function voiceName(id: string | undefined, voices: Voice[]): string {
  if (!id) return "Default voice";
  return voices.find((v) => v.id === id)?.name ?? id;
}

/** Examples of designed voices. */
export const designExamples = [
  "A calm older woman with a low, warm voice and a slight Bavarian accent",
  "A young man, bright and fast-talking, like a tech podcast host",
  "A deep, gravelly male voice, slow and deliberate, a film trailer narrator",
  "A cheerful girl of about ten, curious and quick",
  "A soft-spoken British gentleman, precise and a little dry",
];
