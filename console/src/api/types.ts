// The server's JSON (docs/architecture.md, docs/api.md): only what the console reads.

export type Kind = "chat" | "embed" | "classify" | "image" | "video" | "music" | "speech" | "avatar" | "sound";
export type JobState = "queued" | "running" | "done" | "failed" | "canceled";

/** A job of any worker (GET /api/jobs on the server merges them). */
export interface Job {
  id: string;
  model: string;
  name?: string;
  kind: Kind;
  state: JobState;
  created: string;
  started?: string;
  finished?: string;
  status?: string;
  step?: number;
  total?: number;
  error?: string;
  /** The worker that holds the job. */
  worker?: { id: string; name: string };
  /** The client token's name when an application submitted it (docs/api.md). */
  client?: string;
  request?: { title?: string; prompt?: string; text?: string; messages?: { Role: string; Content: string }[] };
}

export interface EngineEvent {
  type: string;
  text?: string;
  step?: number;
  total?: number;
}

/** One message of /api/events: the whole job when its state changes, an event while it runs, neither when deleted. */
export interface Update {
  jobId: string;
  job?: Job;
  event?: EngineEvent;
}

export interface ModelWorker {
  id: string;
  name: string;
  /** On its disk (else it downloads first). */
  here: boolean;
  /** In its memory now. */
  loaded: boolean;
  /** Its memory is enough. */
  fits: boolean;
}

export interface Model {
  id: string;
  name: string;
  kind: Kind;
  backend: string;
  about?: string;
  size?: number;
  here?: boolean;
  gated?: boolean;
  licence?: string;
  nonCommercial?: boolean;
  /** A music, sound or speech model: what it takes (the transcriber has no maxChars). */
  audio?: { maxSeconds?: number; maxChars?: number };
  workers?: ModelWorker[];
}

export interface ModelsResponse {
  models: Model[];
  loaded?: Record<string, string>;
  kinds?: Kind[];
}

/** What a worker is (its /api/info, through the server). */
export interface WorkerSpecs {
  chip?: string;
  ramGB?: number;
  gpuCores?: number;
  /** Older Macs: the macOS version. */
  macOS?: string;
  /** "darwin", "linux", "windows". */
  os?: string;
  osVersion?: string;
  arch?: string;
  gpu?: string;
  vramGB?: number;
  version?: string;
}

export interface WorkerLoad {
  running?: string | null;
  queued?: number;
  loaded?: Record<string, string>;
}

/** A worker enrolled with its own token (GET /api/workers). */
export interface Worker {
  id: string;
  name: string;
  created?: string;
  lastSeen?: string;
  connected: boolean;
  since?: string;
  specs?: WorkerSpecs;
  state?: WorkerLoad;
  primary?: boolean;
  pinned?: boolean;
  route?: string;
  legacy?: boolean;
}

export interface WorkerCreated {
  worker: Worker;
  token: string;
}

/** A worker on the tunnel now (GET /api/worker/events `workers`). */
export interface ConnectedWorker {
  id: string;
  name: string;
  since?: string;
}

/** GET /api/worker/events: the primary in the old fields, and every connected worker. */
export interface WorkerEvent {
  connected: boolean;
  id?: string;
  name?: string;
  since?: string;
  workers?: ConnectedWorker[];
}

/** GET /api/info (no sign-in needed). */
export interface Info {
  role?: string;
  signedIn?: boolean;
  user?: string;
  owner?: boolean;
  version?: string;
  worker?: WorkerEvent;
  workers?: ConnectedWorker[];
}

export interface Device {
  id: string;
  name: string;
  user?: string;
}

export interface LoginResponse {
  token: string;
  device: Device;
}

/** An application's API token (GET /api/clients; the token itself is shown once, on creation). */
export interface Client {
  id: string;
  name: string;
  created: string;
  lastSeen?: string;
  /** The person it acts for (who made it): its Claude Code jobs run on their workers. Absent on older tokens. */
  owner?: string;
}

export interface ClientCreated {
  client: Client;
  token: string;
}

/** An app waiting for its client token (GET /api/link/{code}): approving it makes a client that acts for you. */
export interface LinkRequest {
  name: string;
  /** The PIN as the app shows it, "123 456". */
  userCode: string;
  expires: string;
}

export type UplinkState = "connected" | "connecting" | "error" | "disabled";

/**
 * An outbound connection to an application that cannot reach this server (GET /api/uplinks; never the token): the
 * server dials it, and its requests come back through it as the uplink's client.
 */
export interface Uplink {
  id: string;
  name: string;
  url: string;
  enabled: boolean;
  /** The user of the device that made it: the uplink's client acts for them. */
  owner: string;
  /** The id of the client it acts as (listed under Clients). */
  client: string;
  created: string;
  state: UplinkState;
  /** Why it is in state error; while connecting, why it retries ("" none). */
  error?: string;
  /** Connected since (RFC 3339; absent while not connected). */
  since?: string;
}

export interface UplinkInput {
  name?: string;
  url?: string;
  token?: string;
  enabled?: boolean;
}

export interface ErrorBody {
  error: string | { message?: string; type?: string; code?: string };
  message?: string;
}
