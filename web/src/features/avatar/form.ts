import type { Job, Model, Request } from "@/api/types";

export type Source = "text" | "audio";
export type SizeId = "match" | "portrait" | "square" | "landscape";

/** The avatar composer (remembered per browser). */
export interface AvatarForm {
  /** The portrait (/files/ url). */
  portrait: string;
  /** Its shape, to match the video to it. */
  portraitW: number;
  portraitH: number;
  source: Source;
  // type text
  text: string;
  voice: string;
  instruct: string;
  // use audio
  audio: string;
  audioName: string;
  prompt: string;
  size: SizeId;
  seed: number | null;
}

export const defaultAvatar: AvatarForm = {
  portrait: "",
  portraitW: 0,
  portraitH: 0,
  source: "text",
  text: "",
  voice: "",
  instruct: "",
  audio: "",
  audioName: "",
  prompt: "",
  size: "match",
  seed: null,
};

export const sizes: { id: Exclude<SizeId, "match">; label: string; w: number; h: number }[] = [
  { id: "portrait", label: "Portrait", w: 480, h: 832 },
  { id: "square", label: "Square", w: 640, h: 640 },
  { id: "landscape", label: "Landscape", w: 832, h: 480 },
];

const snap32 = (v: number) => Math.max(256, Math.round(v / 32) * 32);

/** The video size: a preset, or the portrait's shape at about 640×640 pixels (multiples of 32). */
export function sizeOf(f: AvatarForm): { w: number; h: number } {
  const p = sizes.find((s) => s.id === f.size);
  if (p) return { w: p.w, h: p.h };
  if (!f.portraitW || !f.portraitH) return { w: 640, h: 640 };
  const k = Math.sqrt((640 * 640) / (f.portraitW * f.portraitH));
  return { w: snap32(f.portraitW * k), h: snap32(f.portraitH * k) };
}

export function avatarRequest(f: AvatarForm, speechModel?: string): Omit<Request, "model"> {
  const { w, h } = sizeOf(f);
  return {
    images: [f.portrait],
    ...(f.source === "audio"
      ? { audio: f.audio }
      : {
          text: f.text.trim(),
          ...(f.voice ? { voice: f.voice } : {}),
          ...(speechModel ? { speechModel } : {}),
          ...(f.instruct.trim() ? { instruct: f.instruct.trim() } : {}),
        }),
    ...(f.prompt.trim() ? { prompt: f.prompt.trim() } : {}),
    width: w,
    height: h,
    ...(f.seed != null ? { seed: f.seed } : {}),
  };
}

export function avatarFrom(j: Job, prev: AvatarForm): AvatarForm {
  const r = j.request;
  const w = r.width || 0;
  const h = r.height || 0;
  const preset = sizes.find((s) => s.w === w && s.h === h);
  return {
    ...prev,
    portrait: r.images?.[0] ?? "",
    portraitW: w,
    portraitH: h,
    source: r.audio && !r.text ? "audio" : "text",
    text: r.text ?? "",
    voice: r.voice ?? "",
    instruct: r.instruct ?? "",
    audio: r.audio ?? "",
    audioName: r.audio ? (r.audio.split("/").pop() ?? "audio") : "",
    prompt: r.prompt ?? "",
    size: preset?.id ?? "match",
    seed: r.seed ?? null,
  };
}

/** What to expect of a model, honestly. */
export function modelHint(m?: Model): string {
  if (!m) return "";
  if (/longcat/i.test(m.id)) return `Experimental: on this Mac about 10 min per second of video, ~37 GB of memory, and in the test it did not keep the face (a different person came out). Use LTX-2.5 for real takes.${/bf16/i.test(m.id) ? " Full precision needs ~50 GB of memory at its peak." : ""}`;
  if (/ltx/i.test(m.id)) return "LTX-2.5: clips of about 10–20 s; the fastest. Mostly the face and lips move.";
  return m.about;
}

/** Roughly how long making it takes, for LongCat (minutes per second of video). */
export function waitHint(m: Model | undefined, seconds: number): string {
  if (!m || !seconds) return "";
  if (/longcat/i.test(m.id)) {
    // measured on a 64 GB Mac Studio: ~1000 s for 1.1 s of speech
    return `≈ ${Math.max(5, Math.round(seconds * 10))} min with LongCat`;
  }
  // LTX: ~2 min for a 4.5 s clip at 384×512
  return `≈ ${Math.max(1, Math.round(seconds / 2))} min`;
}
