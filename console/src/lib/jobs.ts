// The queue as the overview shows it. Pure: tested with `pnpm test` (src/lib/jobs.test.ts).
import type { Job, Update } from "../api/types.ts";

export const isActive = (j: Pick<Job, "state">) => j.state === "queued" || j.state === "running";

/** Queued and running jobs can be cancelled. */
export const canCancel = isActive;

/** Running first, then queued (oldest first: next in line on top), then the rest newest first. */
export function sortJobs(jobs: Job[]): Job[] {
  const rank = (j: Job) => (j.state === "running" ? 0 : j.state === "queued" ? 1 : 2);
  const t = (j: Job) => Date.parse(j.created) || 0;
  return [...jobs].sort((a, b) => rank(a) - rank(b) || (rank(a) < 2 ? t(a) - t(b) : t(b) - t(a)));
}

/** Applies one /api/events message to the jobs by id (an event only changes a job we have). */
export function applyUpdate(jobs: Record<string, Job>, u: Update): Record<string, Job> {
  if (u.job) return { ...jobs, [u.jobId]: u.job };
  if (u.event) {
    const j = jobs[u.jobId];
    if (!j) return jobs;
    const ev = u.event;
    if (ev.type === "status") return { ...jobs, [u.jobId]: { ...j, status: ev.text } };
    if (ev.type === "progress") return { ...jobs, [u.jobId]: { ...j, step: ev.step ?? 0, total: ev.total ?? 0 } };
    return jobs;
  }
  if (!(u.jobId in jobs)) return jobs;
  const { [u.jobId]: _, ...rest } = jobs;
  return rest;
}

/** A job's short description: its title, the last user message of a chat, the prompt — or its id. */
export function jobTitle(j: Job): string {
  const r = j.request;
  const last = r?.messages?.findLast((m) => m.Role === "user")?.Content;
  const t = (r?.title || last || r?.prompt || r?.text || j.name || "").replace(/\s+/g, " ").trim();
  return t.length > 120 ? t.slice(0, 119) + "…" : t || j.id;
}

/** Progress 0..1, or undefined when the job does not know. */
export function progress(j: Job): number | undefined {
  return j.total ? Math.min(1, (j.step ?? 0) / j.total) : undefined;
}

/** Counts for the overview's tiles. */
export function queueStats(jobs: Job[]): { running: number; queued: number; failed: number; done: number } {
  const s = { running: 0, queued: 0, failed: 0, done: 0 };
  for (const j of jobs) {
    if (j.state === "running") s.running++;
    else if (j.state === "queued") s.queued++;
    else if (j.state === "failed") s.failed++;
    else if (j.state === "done") s.done++;
  }
  return s;
}
