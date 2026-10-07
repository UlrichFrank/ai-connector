import type { ConnectedMac, Job, Model, ModelWorker, Worker, WorkerState } from "../api/types";

// Several Macs on one hub (docs/architecture.md, "Several Macs (workers) at once"). With one Mac nothing here shows.

/** The Macs on the tunnel now: the hub's list, else the one the older single fields describe. */
export function connectedMacs(w: WorkerState | null | undefined): ConnectedMac[] {
  if (!w) return [];
  if (Array.isArray(w.workers)) return w.workers.filter((m) => m && m.id);
  return w.connected ? [{ id: w.id ?? "", name: w.name, since: w.since }] : [];
}

/** More than one Mac is known: several connected, or jobs made on more than one. Then jobs say where they ran. */
export function manyMacs(connected: ConnectedMac[], jobs: Record<string, Job>): boolean {
  if (connected.length > 1) return true;
  let first: string | undefined = connected[0]?.id || undefined;
  for (const id in jobs) {
    const w = jobs[id].worker?.id;
    if (!w) continue;
    if (first === undefined) first = w;
    else if (w !== first) return true;
  }
  return false;
}

/** The Macs that made these jobs, by name, for a filter (only when there are several). */
export function macsOf(jobs: Job[]): { id: string; name: string; count: number }[] {
  const by = new Map<string, { id: string; name: string; count: number }>();
  for (const j of jobs) {
    if (!j.worker?.id) continue;
    const m = by.get(j.worker.id) ?? { id: j.worker.id, name: j.worker.name || "Mac", count: 0 };
    m.count++;
    by.set(j.worker.id, m);
  }
  return [...by.values()].sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" }));
}

/** "1.4.2", "v1.4.2-3-gabc", "1.5" → [1, 4, 2] …; null for "dev" and the like. */
function parts(v: string | undefined): number[] | null {
  const m = /^v?(\d+(?:\.\d+)*)/.exec((v ?? "").trim());
  return m ? m[1].split(".").map(Number) : null;
}

/** -1 when `a` is older than `b`, 1 when newer, 0 when the same or either cannot be read. */
export function compareVersions(a: string | undefined, b: string | undefined): number {
  const x = parts(a);
  const y = parts(b);
  if (!x || !y) return 0;
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    const d = (x[i] ?? 0) - (y[i] ?? 0);
    if (d) return d < 0 ? -1 : 1;
  }
  return 0;
}

/** The version a Mac should have: the hub's, else the newest any Mac has. */
export function referenceVersion(hub: string | undefined, workers: Worker[]): string | undefined {
  if (parts(hub)) return hub;
  let best: string | undefined;
  for (const w of workers) {
    const v = w.specs?.version;
    if (parts(v) && (!best || compareVersions(v, best) > 0)) best = v;
  }
  return best;
}

/** The Mac's mlxlab is older than the reference. */
export function isOutdated(version: string | undefined, reference: string | undefined): boolean {
  return compareVersions(version, reference) < 0;
}

/** "loaded", "on disk", "downloads first", "too little memory". */
export function whereText(w: ModelWorker): string {
  if (!w.fits) return "too little memory";
  if (w.loaded) return "loaded";
  return w.here ? "on disk" : "downloads first";
}

/** No connected Mac has the memory for it (an older hub, or no Mac at all, says nothing: then it may run). */
export function fitsNowhere(m: Model): boolean {
  return !!m.workers?.length && !m.workers.some((w) => w.fits);
}

/** Why a model cannot be picked: "Needs more memory than MacBook (16 GB) has", or "" when it can. */
export function noFitReason(m: Model): string {
  if (!fitsNowhere(m)) return "";
  const ws = m.workers!;
  return ws.length === 1 ? `Too big for ${ws[0].name || "the Mac"}'s memory` : "Too big for every connected Mac's memory";
}

/** The Macs a model can run on, best first: loaded, on disk, download; those without the memory last. */
export function rankWorkers(ws: ModelWorker[]): ModelWorker[] {
  const score = (w: ModelWorker) => (w.fits ? 0 : 4) + (w.loaded ? 0 : w.here ? 1 : 2);
  return [...ws].sort((a, b) => score(a) - score(b));
}

/** "64 GB · Apple M2 Ultra · 60-core GPU · macOS 27.0.1" pieces, those known. */
export function specParts(w: Worker): string[] {
  const s = w.specs;
  if (!s) return [];
  return [s.chip, s.ramGB ? `${s.ramGB} GB` : "", s.gpuCores ? `${s.gpuCores}-core GPU` : "", s.macOS ? `macOS ${s.macOS}` : ""].filter(Boolean) as string[];
}
