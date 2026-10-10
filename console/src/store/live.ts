import { create } from "zustand";
import { api, signedOut } from "@/api/client";
import type { ConnectedWorker, Info, Job, Model, Update, Worker, WorkerEvent } from "@/api/types";
import { applyUpdate } from "@/lib/jobs";

/**
 * The job stream (/api/events): connecting at first; live while it runs; waiting while no worker is connected
 * (the server has nothing to stream then); offline when it broke and is tried again.
 */
export type Connection = "connecting" | "live" | "waiting" | "offline";

interface LiveState {
  connection: Connection;
  /** The workers on the tunnel now (GET /api/worker/events). */
  connected: ConnectedWorker[];
  /** Every enrolled worker, with specs and state (GET /api/workers). */
  workers: Worker[];
  workersError: unknown;
  jobs: Record<string, Job>;
  models: Model[];
  modelsError: unknown;
  modelsLoaded: boolean;

  start: (info: Info) => void;
  stop: () => void;
  refreshWorkers: () => Promise<void>;
  refreshJobs: () => Promise<void>;
  refreshModels: () => Promise<void>;
}

const empty = {
  connection: "connecting" as Connection,
  connected: [] as ConnectedWorker[],
  workers: [] as Worker[],
  workersError: null as unknown,
  jobs: {} as Record<string, Job>,
  models: [] as Model[],
  modelsError: null as unknown,
  modelsLoaded: false,
};

/** The workers on the tunnel, from a worker event (or /api/info): the list, else the one the old fields describe. */
export function connectedOf(w: WorkerEvent | undefined | null, list?: ConnectedWorker[]): ConnectedWorker[] {
  if (Array.isArray(list)) return list.filter((m) => m && m.id);
  if (!w) return [];
  if (Array.isArray(w.workers)) return w.workers.filter((m) => m && m.id);
  return w.connected ? [{ id: w.id ?? "", name: w.name ?? "", since: w.since }] : [];
}

const backoff = (tries: number) => Math.min(1000 * 2 ** tries, 15000);

interface Run {
  stopped: boolean;
  events: EventSource | null;
  workerEvents: EventSource | null;
  eventsTimer?: ReturnType<typeof setTimeout>;
  workerTimer?: ReturnType<typeof setTimeout>;
  poll?: ReturnType<typeof setInterval>;
  eventsTries: number;
  workerTries: number;
}

let run: Run | null = null;

export const useLive = create<LiveState>((set, get) => ({
  ...empty,

  start: (info) => {
    if (run) return;
    const r: Run = { stopped: false, events: null, workerEvents: null, eventsTries: 0, workerTries: 0 };
    run = r;
    // the server streams jobs only while a worker is connected; `mlxlab serve` is the worker itself
    const hub = info.role === "hub";
    set({ connected: connectedOf(info.worker, info.workers) });

    const connectEvents = () => {
      clearTimeout(r.eventsTimer);
      if (r.stopped || r.events) return;
      if (hub && !get().connected.length) {
        set({ connection: "waiting" });
        return; // the worker stream starts it when one connects
      }
      const es = new EventSource("/api/events");
      r.events = es;
      es.onopen = () => {
        r.eventsTries = 0;
        set({ connection: "live" });
        get().refreshJobs();
      };
      es.onmessage = (e: MessageEvent<string>) => {
        const u = JSON.parse(e.data) as Update;
        const prev = get().jobs[u.jobId];
        set({ jobs: applyUpdate(get().jobs, u) });
        // a job that started or ended changes what the workers hold in memory
        if (u.job && prev?.state !== u.job.state) {
          get().refreshWorkers();
          if (u.job.state !== "queued") get().refreshModels();
        }
      };
      es.onerror = async () => {
        es.close();
        if (r.events === es) r.events = null;
        if (r.stopped) return;
        set({ connection: !hub || get().connected.length ? "offline" : "waiting" });
        const i = await api.info().catch(() => null);
        if (r.stopped) return;
        if (i && i.role === "hub" && !i.signedIn) return signedOut();
        if (i) set({ connected: connectedOf(i.worker, i.workers) });
        // a live stream that ended (a worker came or went): again at once; else with backoff
        r.eventsTimer = setTimeout(connectEvents, r.eventsTries === 0 ? 250 : backoff(r.eventsTries));
        r.eventsTries++;
      };
    };

    const connectWorker = () => {
      clearTimeout(r.workerTimer);
      if (r.stopped || r.workerEvents) return;
      const es = new EventSource("/api/worker/events");
      r.workerEvents = es;
      es.onopen = () => (r.workerTries = 0);
      es.onmessage = (e: MessageEvent<string>) => {
        const w = JSON.parse(e.data) as WorkerEvent;
        const before = get().connected.map((m) => m.id).join();
        const connected = connectedOf(w);
        set({ connected });
        if (before !== connected.map((m) => m.id).join()) {
          get().refreshWorkers();
          get().refreshJobs();
          get().refreshModels();
        }
        if (connected.length && !r.events) {
          r.eventsTries = 0;
          connectEvents();
        } else if (!connected.length) {
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
        const i = await api.info().catch(() => null);
        if (r.stopped) return;
        if (i && i.role === "hub" && !i.signedIn) return signedOut();
        r.workerTimer = setTimeout(connectWorker, backoff(r.workerTries++));
      };
    };

    if (hub) connectWorker();
    connectEvents();
    get().refreshWorkers();
    get().refreshJobs();
    get().refreshModels();
    // specs and load change without an event: look again now and then while the page is visible
    r.poll = setInterval(() => {
      if (document.visibilityState === "visible") get().refreshWorkers();
    }, 15000);
  },

  stop: () => {
    if (!run) return;
    run.stopped = true;
    run.events?.close();
    run.workerEvents?.close();
    clearTimeout(run.eventsTimer);
    clearTimeout(run.workerTimer);
    clearInterval(run.poll);
    run = null;
    set(empty);
  },

  refreshWorkers: async () => {
    try {
      set({ workers: await api.workers(), workersError: null });
    } catch (e) {
      set({ workersError: e });
    }
  },

  refreshJobs: async () => {
    const list = await api.jobs().catch(() => null);
    if (list) set({ jobs: Object.fromEntries(list.map((j) => [j.id, j])) });
  },

  refreshModels: async () => {
    try {
      const r = await api.models();
      set({ models: r.models ?? [], modelsError: null, modelsLoaded: true });
    } catch (e) {
      set({ modelsError: e, modelsLoaded: true });
    }
  },
}));
