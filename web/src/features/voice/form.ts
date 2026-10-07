import type { Job, Request } from "@/api/types";

/** The speech composer (remembered per browser). */
export interface SpeechForm {
  text: string;
  /** A library voice id, a preset name, or "" (the model's own). */
  voice: string;
  instruct: string;
  language: string;
  speed: number;
  seed: number | null;
}

export const defaultSpeech: SpeechForm = { text: "", voice: "", instruct: "", language: "auto", speed: 1, seed: null };

export function speechFrom(j: Job): SpeechForm {
  const r = j.request;
  return { text: r.text ?? "", voice: r.voice ?? "", instruct: r.instruct ?? "", language: r.language || "auto", speed: r.speed || 1, seed: r.seed ?? null };
}

export function speechRequest(f: SpeechForm): Omit<Request, "model"> {
  return {
    text: f.text.trim(),
    ...(f.voice ? { voice: f.voice } : {}),
    ...(f.instruct.trim() ? { instruct: f.instruct.trim() } : {}),
    ...(f.language && f.language !== "auto" ? { language: f.language } : {}),
    ...(f.speed !== 1 ? { speed: f.speed } : {}),
    ...(f.seed != null ? { seed: f.seed } : {}),
  };
}

/** The sound effects composer. */
export interface SoundForm {
  prompt: string;
  duration: number;
  seed: number | null;
}

export const defaultSound: SoundForm = { prompt: "", duration: 5, seed: null };

export function soundFrom(j: Job): SoundForm {
  return { prompt: j.request.prompt ?? "", duration: j.request.duration || 5, seed: j.request.seed ?? null };
}

export function soundRequest(f: SoundForm): Omit<Request, "model"> {
  return { prompt: f.prompt.trim(), duration: f.duration, ...(f.seed != null ? { seed: f.seed } : {}) };
}

/** Sound ideas for when the server has no examples. */
export const soundExamples: { title: string; prompt: string; duration: number }[] = [
  { title: "Rain on a tin roof", prompt: "Steady rain on a tin roof, distant thunder rolling, cozy and close", duration: 10 },
  { title: "Door creak", prompt: "An old wooden door creaking open slowly in an empty hallway", duration: 4 },
  { title: "Café", prompt: "A busy café: espresso machine hissing, cups clinking, soft chatter", duration: 12 },
  { title: "Sci-fi UI", prompt: "A short futuristic interface beep, soft and glassy, rising pitch", duration: 2 },
  { title: "Footsteps in snow", prompt: "Footsteps crunching through fresh snow, slow pace, winter quiet", duration: 6 },
  { title: "Campfire", prompt: "A crackling campfire at night, crickets in the background", duration: 10 },
];
