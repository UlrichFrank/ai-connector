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

export type ConnectorState = "connected" | "connecting" | "error" | "disabled";

export interface ConnectorModels {
  classify?: string;
  embed?: string;
  chat?: string;
}

/** One of the AIs a connector offers vboard's chat (the first is the default and must be a model). */
export interface ConnectorAgent {
  /** Stable (vboard remembers the pick by it); derived from the model when blank. */
  id: string;
  model: string;
  /** "" = the model's name. */
  name?: string;
  /** Set by the server: claude-code/* is an agent (it works on the board through vboard's MCP). */
  kind?: "model" | "agent";
  /** A Claude Code agent's effort level (low, medium, high, xhigh, max; absent: the worker's default). */
  effort?: string;
}

/** An agent as the connector offers it to vboard now. */
export interface AdvertisedAgent {
  id: string;
  name: string;
  kind: "model" | "agent";
  worker?: string;
  model: string;
}

/** An outbound connection to an application (GET /api/connectors; never the token). */
export interface Connector {
  id: string;
  kind: "vboard";
  name: string;
  url: string;
  board?: string;
  enabled: boolean;
  state: ConnectorState;
  /** Why it is in state error; while connecting, why it waits or retries ("" none). */
  error?: string;
  /** Connected since (RFC 3339; absent while not connected). */
  since?: string;
  /** Who the token belongs to, as the application says. */
  user?: string;
  /** "" = the server's default model. */
  models?: ConnectorModels;
  /** The chosen agents in their order ([] = the default: the chat model alone). */
  agents?: ConnectorAgent[];
  /** What vboard was told of (while connected). */
  advertised?: AdvertisedAgent[];
  /** The image and video models offered to vboard's board, in order (catalogue ids; [] = none). */
  generators?: string[];
  /** The user of the device that made it. */
  owner?: string;
  created?: string;
}

export interface ConnectorInput {
  kind?: "vboard";
  name?: string;
  url?: string;
  token?: string;
  board?: string;
  enabled?: boolean;
  models?: ConnectorModels;
  agents?: ConnectorAgent[];
  generators?: string[];
}

export interface ErrorBody {
  error: string | { message?: string; type?: string; code?: string };
  message?: string;
}
