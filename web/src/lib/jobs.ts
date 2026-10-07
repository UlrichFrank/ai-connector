import { toast } from "sonner";
import { api, errorText, NoWorkerError } from "@/api/client";
import type { Job, Request } from "@/api/types";
import { isActive, useApp } from "@/store/app";
import { local } from "./storage";

/** Submits one request per model ("Create 2 versions"); puts the jobs into the store; returns them. */
export async function submitEach(models: string[], req: Omit<Request, "model">): Promise<Job[]> {
  const out: Job[] = [];
  for (const model of models) {
    try {
      const j = await api.submit({ ...req, model });
      useApp.getState().put(j);
      out.push(j);
    } catch (e) {
      startFailed(e);
      break;
    }
  }
  return out;
}

/**
 * A job could not start: a toast that says why. No Mac with the memory for the model (409 no-worker) gets its own
 * title and stays until read.
 */
export function startFailed(e: unknown, title = "Could not start") {
  if (e instanceof NoWorkerError) toast.error("No Mac can run this", { description: errorText(e), duration: 12000 });
  else toast.error(title, { description: errorText(e) });
}

/** Resolves when the job has ended (done, failed or canceled); the event stream keeps the store current. */
export function waitJob(id: string): Promise<Job> {
  return new Promise((resolve) => {
    const check = (jobs: Record<string, Job>) => {
      const j = jobs[id];
      if (j && !isActive(j)) {
        stop();
        resolve(j);
        return true;
      }
      return false;
    };
    const stop = useApp.subscribe((s) => check(s.jobs));
    check(useApp.getState().jobs);
  });
}

/** The answer of a chat model without its thinking (<think>…</think>) and surrounding quotes. */
export function answerText(j: Job): string {
  let t = j.text ?? "";
  const close = t.indexOf("</think>");
  if (close >= 0) t = t.slice(close + 8);
  return t.trim().replace(/^["“]|["”]$/g, "").trim();
}

/**
 * Asks a chat model once (a job like any other: it shows in Activity) and returns its answer. Used by ✨ Enhance,
 * "Song from an idea" and AI covers.
 */
export async function runChat(model: string, system: string, user: string, opts: { temperature?: number; maxTokens?: number } = {}) {
  const j = await api.submit({
    model,
    temperature: opts.temperature ?? 0.7,
    maxTokens: opts.maxTokens ?? 900,
    messages: [
      { Role: "system", Content: system },
      { Role: "user", Content: user },
    ],
  });
  useApp.getState().put(j);
  const done = await waitJob(j.id);
  if (done.state !== "done") throw new Error(done.problem?.title ?? done.error ?? done.state);
  return answerText(done);
}

export function setHelperModel(id: string) {
  local.set("helperModel", id);
}

/** The chat model used for helpers (Enhance, song writing): the remembered one, else a downloaded one. */
export function helperModel(): string | undefined {
  const models = useApp.getState().models.filter((m) => m.kind === "chat");
  const pick = local.get<string | undefined>("helperModel", undefined);
  return models.find((m) => m.id === pick)?.id ?? models.find((m) => m.here)?.id ?? models[0]?.id;
}
