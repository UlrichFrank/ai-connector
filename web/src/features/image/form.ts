import type { Job, Model, Request } from "@/api/types";
import { dims, seedOf, stepsOf } from "@/components/media/media";

/** The quantisation the image models load with: 8-bit, 4-bit, or full bf16 (-1). */
export type Quantize = 8 | 4 | -1;

/** What the composer holds (remembered per browser). Blank numbers mean "the model's default" (seed: random). */
export interface ImageForm {
  prompt: string;
  width: number;
  height: number;
  steps: number | null;
  guidance: number | null;
  seed: number | null;
  quantize: Quantize;
  /** Reference images (/files/ urls): with them the model edits instead of drawing from nothing. */
  refs: string[];
}

export const maxRefs = 4;

export const defaultForm: ImageForm = { prompt: "", width: 1024, height: 1024, steps: null, guidance: null, seed: null, quantize: 8, refs: [] };

/** Multiples of 16 between 256 and 2048 (what FLUX takes). */
export const snap = (v: number) => Math.max(256, Math.min(2048, Math.round((v || 0) / 16) * 16));

/** The settings an image was made with, to make it again (same seed: the same picture). */
export function formFrom(j: Job): ImageForm {
  const { w, h } = dims(j);
  const q = (j.stats?.quantize ?? j.request.quantize) as number | undefined;
  return {
    prompt: j.request.prompt ?? "",
    width: w,
    height: h,
    steps: stepsOf(j) ?? null,
    guidance: j.request.guidance ?? null,
    seed: seedOf(j) ?? null,
    quantize: q === 4 ? 4 : q === 0 || q === -1 ? -1 : 8,
    refs: j.request.images ?? [],
  };
}

/** The request for the server. */
export function requestOf(f: ImageForm): Omit<Request, "model"> {
  return {
    prompt: f.prompt.trim(),
    width: snap(f.width),
    height: snap(f.height),
    quantize: f.quantize,
    ...(f.steps ? { steps: f.steps } : {}),
    ...(f.guidance != null ? { guidance: f.guidance } : {}),
    ...(f.seed != null ? { seed: f.seed } : {}),
    ...(f.refs.length ? { images: f.refs } : {}),
  };
}

/**
 * Why the picked models cannot take the request: an image model with tasks does only those ("generate": text to
 * image, no references; "edit": needs references, Qwen-Image-Edit). Models without tasks (FLUX.2 klein) do both.
 */
export function refsProblem(models: Model[], editing: boolean): string | undefined {
  const task = editing ? "edit" : "generate";
  const no = models.filter((m) => m.tasks?.length && !m.tasks.includes(task));
  if (!no.length) return undefined;
  const names = no.map((m) => m.name).join(", ");
  return editing ? `${names} makes pictures from text only: remove the reference images or pick another model.` : `${names} edits pictures: add a reference image first.`;
}

export const randomSeed = () => Math.floor(Math.random() * 1_000_000_000);
