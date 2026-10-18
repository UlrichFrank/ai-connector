import type { Job } from "@/api/types";
import { locale, num, t } from "@/lib/i18n";

/** 75 → "1:15"; under a minute "42 s". */
export function duration(s: number | undefined): string {
  if (s == null || !isFinite(s)) return "";
  if (s < 60) return `${s < 10 ? num(s, 1) : Math.round(s)} s`;
  const m = Math.floor(s / 60);
  const r = Math.round(s % 60);
  return `${m}:${String(r).padStart(2, "0")}`;
}

export function bytes(n: number): string {
  if (!n) return "";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1000 && i < u.length - 1) (n /= 1000), i++;
  return `${n < 10 && i ? num(n, 1) : Math.round(n)} ${u[i]}`;
}

/** "just now", "5 min ago", "yesterday", or the date. */
export function ago(iso: string | undefined): string {
  if (!iso) return "";
  const at = new Date(iso).getTime();
  const s = (Date.now() - at) / 1000;
  if (s < 45) return t("just now");
  if (s < 3600) return t("{n} min ago", { n: Math.round(s / 60) });
  if (s < 86400) return t("{n} h ago", { n: Math.round(s / 3600) });
  if (s < 2 * 86400) return t("yesterday");
  return new Date(at).toLocaleDateString(locale(), { day: "numeric", month: "short" });
}

/** How long a job took (or is taking). */
export function took(j: Job): number | undefined {
  if (!j.started) return undefined;
  const end = j.finished ? new Date(j.finished).getTime() : Date.now();
  return (end - new Date(j.started).getTime()) / 1000;
}

/** Progress 0..1, or undefined when the job does not know. */
export function progress(j: Job): number | undefined {
  return j.total ? Math.min(1, (j.step ?? 0) / j.total) : undefined;
}

export const isMovie = (j: Job) => !!j.request.script?.length;

/** What a job is called in a sentence. */
export function noun(j: Job): string {
  if (isMovie(j)) return "movie";
  if (isTranscription(j)) return "transcript";
  return {
    image: "image",
    video: "video",
    music: "song",
    chat: "answer",
    embed: "comparison",
    classify: "result",
    speech: "speech",
    avatar: "avatar video",
    sound: "sound",
  }[j.kind];
}

/** A speech job that writes down what an audio says (qwen3-asr; used for voice samples), not one that speaks. */
export const isTranscription = (j: Job) => j.kind === "speech" && !j.request.text && !!j.request.audio;

/** A song's (or any job's) title. */
export function title(j: Job): string {
  return (
    j.request.title ||
    (j.kind === "chat" ? j.request.messages?.findLast((m) => m.Role === "user")?.Content : j.kind === "speech" || j.kind === "avatar" ? j.request.text || j.request.prompt : j.request.prompt || j.request.text) ||
    j.name ||
    j.id
  );
}
