// What a worker is and where a model can run, as words. Pure: tested with `pnpm test` (src/lib/specs.test.ts).
import type { ModelWorker, Worker, WorkerSpecs } from "../api/types.ts";

export type Platform = "mac" | "linux" | "windows" | "unknown";

/** The worker's platform; a worker from before `os` was reported is a Mac (it said macOS). */
export function platformOf(s: WorkerSpecs | undefined): Platform {
  const os = (s?.os ?? "").toLowerCase();
  if (os === "darwin" || os === "macos" || (!os && s?.macOS)) return "mac";
  if (os === "linux") return "linux";
  if (os === "windows") return "windows";
  return "unknown";
}

/** "macOS 27.0.1", "Linux 6.8 (Ubuntu 24.04)", "Windows 11" — or "" when it did not say. */
export function osLabel(s: WorkerSpecs | undefined): string {
  if (!s) return "";
  const p = platformOf(s);
  const version = (s.osVersion || (p === "mac" ? s.macOS : "") || "").trim();
  const name = p === "mac" ? "macOS" : p === "linux" ? "Linux" : p === "windows" ? "Windows" : (s.os ?? "").trim();
  if (!name) return version;
  // some systems report a name that already includes the OS ("Windows 11 Pro", "Ubuntu 24.04")
  if (version.toLowerCase().startsWith(name.toLowerCase())) return version;
  return version ? `${name} ${version}` : name;
}

/** "arm64", "x86-64". */
export function archLabel(arch: string | undefined): string {
  const a = (arch ?? "").toLowerCase();
  if (a === "amd64" || a === "x86_64" || a === "x64") return "x86-64";
  if (a === "aarch64") return "arm64";
  return a;
}

/** The pieces known, in order: "Apple M2 Ultra", "192 GB RAM", "76-core GPU", "macOS 27.0.1", "arm64". */
export function specParts(s: WorkerSpecs | undefined): string[] {
  if (!s) return [];
  const gpu = s.gpu ? (s.vramGB ? `${s.gpu} (${s.vramGB} GB)` : s.gpu) : s.vramGB ? `${s.vramGB} GB VRAM` : "";
  return [s.chip ?? "", s.ramGB ? `${s.ramGB} GB RAM` : "", gpu, s.gpuCores ? `${s.gpuCores}-core GPU` : "", osLabel(s), archLabel(s.arch)].filter(Boolean);
}

/** One line for a worker's specs, "" when it never said (not connected yet, or an older worker). */
export function specLine(w: Pick<Worker, "specs">): string {
  return specParts(w.specs).join(" · ");
}

/** "loaded", "on disk", "downloads first", "too little memory". */
export function whereText(w: ModelWorker): string {
  if (!w.fits) return "too little memory";
  if (w.loaded) return "loaded";
  return w.here ? "on disk" : "downloads first";
}

/** The workers a model can run on, best first: loaded, on disk, download; those without the memory last. */
export function rankWorkers(ws: ModelWorker[]): ModelWorker[] {
  const score = (w: ModelWorker) => (w.fits ? 0 : 4) + (w.loaded ? 0 : w.here ? 1 : 2);
  return [...ws].sort((a, b) => score(a) - score(b));
}

/** What a worker is doing: "idle", "running · 2 queued", "3 queued". */
export function loadText(w: Pick<Worker, "connected" | "state">): string {
  if (!w.connected) return "offline";
  const st = w.state;
  if (!st) return "connected";
  const parts: string[] = [];
  if (st.running) parts.push("running");
  if (st.queued) parts.push(`${st.queued} queued`);
  return parts.length ? parts.join(" · ") : "idle";
}

/** The models a worker holds in memory (backend → model id), as ids. */
export function loadedModels(w: Pick<Worker, "state">): string[] {
  return Object.values(w.state?.loaded ?? {}).filter(Boolean);
}
