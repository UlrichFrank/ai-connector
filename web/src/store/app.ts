import { create } from "zustand";
import { api, signedOut } from "@/api/client";
import { connectedMacs, manyMacs } from "@/lib/macs";
import type { ConnectedMac, EngineEvent, Example, Info, Job, Kind, Model, Prompt, Role, ToolEvent, Update, WorkerState } from "@/api/types";

/** Everything the event stream says, for pages that need more than the jobs (the chat places tool cards in its text). */
const listeners = new Set<(u: Update) => void>();
export function onUpdate(f: (u: Update) => void): () => void {
  listeners.add(f);
  return () => listeners.delete(f);
}

/**
 * The live stream of jobs (/api/events):
 * connecting at first; live while it runs; offline when it broke and we try again (the server is away);
 * waiting while the hub is there but the Mac is not — the stream comes back when the worker does.
 */
export type Connection = "connecting" | "live" | "offline" | "waiting";

interface AppState {
  /** Who serves us (null until started). */
  role: Role | null;
  connection: Connection;
  /** The Mac, as the hub sees it; null against `mlxlab serve` (it is the Mac). */
  worker: WorkerState | null;
  /** The hub: every Mac on the tunnel now (one, usually; several when more are connected). */
  macs: ConnectedMac[];
  /** The hub's mlxlab version, if /api/info says. */
  hubVersion?: string;
  models: Model[];
  loaded: Record<string, string>;
  examples: Partial<Record<Kind, Example[]>>;
  /** Every job by id; the server sends each change over /api/events. */
  jobs: Record<string, Job>;
  prompts: Prompt[];

  /** Opens the streams (after sign-in). */
  start: (info: Info) => void;
  /** Closes them and forgets everything (sign-out). */
  stop: () => void;
  refreshJobs: () => Promise<void>;
  refreshModels: () => Promise<void>;
  refreshPrompts: () => Promise<void>;
  /** Puts a job the API returned into the store (the event for it may come later). */
  put: (j: Job) => void;
  drop: (id: string) => void;
}

/** Applies one engine event to a job (the server sends the whole job only when its state changes). */
function apply(j: Job, ev: EngineEvent): Job {
  switch (ev.type) {
    case "status":
      return { ...j, status: ev.text };
    case "text":
      return { ...j, text: (j.text ?? "") + (ev.text ?? "") };
    case "progress":
      return { ...j, step: ev.step ?? 0, total: ev.total ?? 0 };
    case "file":
      return { ...j, files: [...(j.files ?? []), ev.file!], preview: "" };
    case "preview":
      return { ...j, preview: ev.file };
    case "log":
      return { ...j, logs: [...(j.logs ?? []), ev.text ?? ""] };
    case "stats":
      return { ...j, stats: { ...j.stats, ...ev.data } };
    case "result":
      return { ...j, result: ev.data };
    case "tool": {
      // the latest state of each call, by id (the worker keeps the same list on the job)
      const t = ev.data as unknown as ToolEvent;
      const rest = (j.tools ?? []).filter((x) => x.id !== t.id);
      const at = (j.tools ?? []).findIndex((x) => x.id === t.id);
      const tools = at < 0 ? [...rest, t] : [...rest.slice(0, at), t, ...rest.slice(at)];
      return { ...j, tools };
    }
  }
  return j;
}

const empty = {
  role: null,
  connection: "connecting" as Connection,
  worker: null,
  macs: [],
  hubVersion: undefined,
  models: [],
  loaded: {},
  examples: {},
  jobs: {},
  prompts: [],
} satisfies Partial<AppState>;

/** A delay that grows with each failed try: 1 s, 2 s, 4 s … 15 s. */
const backoff = (tries: number) => Math.min(1000 * 2 ** tries, 15000);
/** While the Mac is away we only look again this rarely (the worker event usually comes first). */
const recheck = 30000;

/** The open streams of one sign-in. */
interface Run {
  stopped: boolean;
  events: EventSource | null;
  workerEvents: EventSource | null;
  eventsTimer?: ReturnType<typeof setTimeout>;
  workerTimer?: ReturnType<typeof setTimeout>;
  eventsTries: number;
  /** A live stream just ended: one quiet try before saying "offline" (the hub ends it when the set of Macs changes). */
  quiet: boolean;
  workerTries: number;
}

let run: Run | null = null;

export const useApp = create<AppState>((set, get) => ({
  ...empty,

  start: (info) => {
    if (run) return;
    const r: Run = { stopped: false, events: null, workerEvents: null, eventsTries: 0, workerTries: 0, quiet: false };
    run = r;
    const hub = info.role === "hub";
    const w0 = hub ? (info.worker ?? null) : null;
    set({ role: info.role, worker: w0, macs: connectedMacs(w0), hubVersion: info.version });

    const onUpdate = (e: MessageEvent<string>) => {
      const u = JSON.parse(e.data) as Update;
      listeners.forEach((f) => f(u));
      const jobs = get().jobs;
      if (u.job) {
        const prev = jobs[u.jobId];
        set({ jobs: { ...jobs, [u.jobId]: u.job } });
        // a job that started or ended changes what is in memory
        if (prev?.state !== u.job.state) get().refreshModels();
      } else if (u.event) {
        const j = jobs[u.jobId];
        if (j) set({ jobs: { ...jobs, [u.jobId]: apply(j, u.event) } });
      } else {
        const { [u.jobId]: _, ...rest } = jobs;
        set({ jobs: rest });
      }
    };

    const connectEvents = () => {
      clearTimeout(r.eventsTimer);
      if (r.stopped || r.events) return;
      if (get().connection !== "offline" && !r.quiet) set({ connection: "connecting" });
      const es = new EventSource("/api/events");
      r.events = es;
      es.onopen = () => {
        r.eventsTries = 0;
        r.quiet = false;
        set({ connection: "live" });
        // anything missed while away comes with the full lists
        get().refreshJobs();
        get().refreshModels();
        get().refreshPrompts();
      };
      es.onmessage = onUpdate;
      es.onerror = () => {
        es.close();
        if (r.events === es) r.events = null;
        if (r.stopped) return;
        // a stream that was live: the hub may have ended it because a Mac came or went — again at once, no
        // "offline" flash; the lists load again when it opens
        if (get().connection === "live" && !r.quiet) {
          r.quiet = true;
          r.eventsTimer = setTimeout(connectEvents, 250);
          return;
        }
        r.quiet = false;
        set({ connection: "offline" });
        diagnose();
      };
    };

    /** Why did /api/events fail? Signed out: back to sign-in. The Mac away: wait for it. Otherwise: again soon. */
    const diagnose = async () => {
      const i = await api.info().catch(() => null);
      if (r.stopped) return;
      if (i && !i.signedIn) return signedOut();
      if (i?.role === "hub") {
        if (i.worker) set({ worker: i.worker, macs: connectedMacs(i.worker) });
        if (!i.worker?.connected) {
          set({ connection: "waiting" });
          r.eventsTimer = setTimeout(diagnose, recheck);
          return;
        }
      }
      r.eventsTimer = setTimeout(connectEvents, backoff(r.eventsTries++));
    };

    const connectWorker = () => {
      clearTimeout(r.workerTimer);
      if (r.stopped || r.workerEvents) return;
      const es = new EventSource("/api/worker/events");
      r.workerEvents = es;
      es.onopen = () => (r.workerTries = 0);
      es.onmessage = (e: MessageEvent<string>) => {
        const w = JSON.parse(e.data) as WorkerState;
        const before = get().macs.map((m) => m.id).join();
        const macs = connectedMacs(w);
        set({ worker: w, macs });
        // another Mac came or went while the stream runs: its jobs and models come or go with it
        if (w.connected && before !== macs.map((m) => m.id).join()) {
          get().refreshJobs();
          get().refreshModels();
        }
        if (w.connected) {
          // the Mac is back: the job stream with it, at once
          if (!r.events) {
            r.eventsTries = 0;
            connectEvents();
          }
        } else {
          r.events?.close();
          r.events = null;
          clearTimeout(r.eventsTimer);
          set({ connection: "waiting" });
        }
      };
      es.onerror = async () => {
        es.close();
        if (r.workerEvents === es) r.workerEvents = null;
        if (r.stopped) return;
        // an EventSource does not tell the status; /api/info does tell whether we are still signed in
        const i = await api.info().catch(() => null);
        if (r.stopped) return;
        if (i && !i.signedIn) return signedOut();
        r.workerTimer = setTimeout(connectWorker, backoff(r.workerTries++));
      };
    };

    if (hub) connectWorker();
    if (hub && !info.worker?.connected) set({ connection: "waiting" });
    else connectEvents();
  },

  stop: () => {
    if (!run) return;
    run.stopped = true;
    run.events?.close();
    run.workerEvents?.close();
    clearTimeout(run.eventsTimer);
    clearTimeout(run.workerTimer);
    run = null;
    set(empty);
  },

  refreshJobs: async () => {
    const list = await api.jobs().catch(() => null);
    if (list) set({ jobs: Object.fromEntries(list.map((j) => [j.id, j])) });
  },

  refreshModels: async () => {
    const r = await api.models().catch(() => null);
    if (r) set({ models: r.models, loaded: r.loaded, examples: r.examples ?? {} });
  },

  refreshPrompts: async () => {
    const p = await api.prompts().catch(() => null);
    if (p) set({ prompts: p });
  },

  put: (j) => set({ jobs: { ...get().jobs, [j.id]: j } }),
  drop: (id) => {
    const { [id]: _, ...rest } = get().jobs;
    set({ jobs: rest });
  },
}));

// ---------- selectors ----------

/** More than one Mac is known (connected, or in the jobs): jobs then say which Mac they are on. */
export const useManyMacs = () => useApp((s) => manyMacs(s.macs, s.jobs));

export const isActive = (j: Job) => j.state === "queued" || j.state === "running";

/** Jobs of a kind, newest first. */
export function jobsOf(jobs: Record<string, Job>, kind: Kind): Job[] {
  return Object.values(jobs)
    .filter((j) => j.kind === kind)
    .sort((a, b) => Date.parse(b.created) - Date.parse(a.created)); // parsed: the server writes local offsets
}
