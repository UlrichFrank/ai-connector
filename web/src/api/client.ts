import type { CommunityLookup, ConnectedMac, Device, ErrorBody, Info, Invite, Job, LoginResponse, ModelsResponse, Prompt, Request, Role, Style, StyleSettings, StylesResponse, ToolsResponse, Voice, VoicesResponse, Worker, WorkerCreated } from "./types";

/** A request the server refused. `code` is the `error` field of a JSON answer ("signin", "offline", …), if any. */
export class ApiError extends Error {
  status: number;
  code?: string;
  constructor(status: number, message: string, code?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }
}

/** The hub has no Mac to forward to (503 {"error":"offline"}): the request can be tried again once it is back. */
export class OfflineError extends ApiError {
  constructor(message = "The Mac is offline") {
    super(503, message, "offline");
    this.name = "OfflineError";
  }
}

/** Not signed in, or the device was revoked (401 {"error":"signin"}). */
export class SignInError extends ApiError {
  constructor(message = "Sign in to continue") {
    super(401, message, "signin");
    this.name = "SignInError";
  }
}

/** Too many sign-in attempts (429). `retryAfter` in seconds, when the hub says. */
export class RateLimitError extends ApiError {
  retryAfter?: number;
  constructor(message: string, retryAfter?: number) {
    super(429, message, "ratelimit");
    this.name = "RateLimitError";
    this.retryAfter = retryAfter;
  }
}

/**
 * The hub has Macs, but none with enough memory for the model (409 {"error":"no-worker"}). `message` names the model and
 * what it needs; `errorText` adds what to do.
 */
export class NoWorkerError extends ApiError {
  constructor(message = "No connected Mac has enough memory for this model") {
    super(409, message, "no-worker");
    this.name = "NoWorkerError";
  }
}

export const isOffline = (e: unknown): e is OfflineError => e instanceof OfflineError;

// Whoever keeps the session (store/session.ts) hears when the server says we are no longer signed in.
let unauthorized = () => {};
export function onUnauthorized(f: () => void) {
  unauthorized = f;
}
/** Tells the session that the sign-in is gone (a 401, or /api/info says so). */
export function signedOut() {
  unauthorized();
}

async function failure(r: Response): Promise<ApiError> {
  const text = (await r.text().catch(() => "")).trim();
  let body: Partial<ErrorBody> = {};
  try {
    body = JSON.parse(text) ?? {};
  } catch {
    // the worker answers most errors in plain text
  }
  const message = body.message || body.error || text || r.statusText;
  if (r.status === 503 && body.error === "offline") return new OfflineError(body.message);
  if (r.status === 409 && body.error === "no-worker") return new NoWorkerError(body.message);
  if (r.status === 401) return new SignInError(body.message);
  if (r.status === 429) {
    const after = Number(r.headers.get("Retry-After"));
    return new RateLimitError(message, isFinite(after) && after > 0 ? after : undefined);
  }
  return new ApiError(r.status, message, body.error);
}

interface CallOptions extends RequestInit {
  /** false: a 401 is the answer (sign-in), not a lost session. */
  session?: boolean;
}

async function call<T>(path: string, { session = true, ...init }: CallOptions = {}): Promise<T> {
  const r = await fetch(path, {
    credentials: "same-origin",
    ...init,
    headers: init.body instanceof FormData ? init.headers : { "Content-Type": "application/json", ...init.headers },
  });
  if (!r.ok) {
    const e = await failure(r);
    if (session && e instanceof SignInError) unauthorized();
    throw e;
  }
  return (r.status === 204 || r.headers.get("Content-Length") === "0" ? undefined : await r.json()) as T;
}

const post = <T>(path: string, body?: unknown, opts: CallOptions = {}) =>
  call<T>(path, { method: "POST", body: body === undefined ? undefined : JSON.stringify(body), ...opts });

/** The hub's /api/info, or the worker's (which may not say its role or sign-in: it has none). */
async function info(): Promise<Info> {
  const i = await call<Partial<Info> & { workers?: ConnectedMac[] }>("/api/info", { session: false, cache: "no-store" });
  // the hub lists every connected Mac beside the primary's old fields
  const worker = i.worker && Array.isArray(i.workers) ? { ...i.worker, workers: i.workers } : i.worker;
  const role: Role = i.role === "hub" ? "hub" : "worker";
  return { app: i.app ?? "mlxlab", name: i.name ?? "mlxlab", role, signedIn: role === "worker" || !!i.signedIn, worker, user: i.user ?? "", owner: !!i.owner, canInvite: !!i.canInvite, version: i.version };
}

/** The mlxlab HTTP API: the hub's own endpoints (docs/architecture.md) and the worker's (internal/web/web.go). */
export const api = {
  // hub
  info,
  login: (password: string, device: string, user: string) =>
    post<LoginResponse>("/api/login", { password, device, user }, { session: false }),
  logout: () => post<void>("/api/logout", undefined, { session: false }),
  devices: () => call<Device[]>("/api/devices"),
  revoke: (id: string) => call<void>(`/api/devices/${encodeURIComponent(id)}`, { method: "DELETE" }),
  /** Owner devices only: the switch "may sign in other iPhones", and the user name. Answers with the changed device. */
  patchDevice: (id: string, change: { canInvite?: boolean; user?: string }) =>
    call<Device>(`/api/devices/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify(change) }),
  /** A QR code that signs in one iPhone, once (owner devices, or devices that may invite; else 403). */
  createInvite: () => post<Invite>("/api/invites"),
  /** The Macs enrolled with their own token (docs/architecture.md, "Connecting a Mac from the web UI"). */
  workers: () => call<Worker[]>("/api/workers"),
  /** Enrolls a Mac: its token comes back this once. */
  createWorker: (name: string) => post<WorkerCreated>("/api/workers", { name }),
  /** Revokes a Mac's token; it is disconnected if it is on the tunnel. */
  revokeWorker: (id: string) => call<void>(`/api/workers/${encodeURIComponent(id)}`, { method: "DELETE" }),
  /** Pins the Mac that serves the per-Mac libraries (prompts, tools, approvals). */
  makePrimary: (id: string) => call<unknown>(`/api/workers/${encodeURIComponent(id)}/primary`, { method: "PUT" }),
  /** Unpins it: the primary is again the connected Mac with the most RAM. */
  autoPrimary: (id: string) => call<unknown>(`/api/workers/${encodeURIComponent(id)}/primary`, { method: "DELETE" }),

  // worker
  models: () => call<ModelsResponse>("/api/models"),
  jobs: () => call<Job[]>("/api/jobs"),
  job: (id: string) => call<Job>(`/api/jobs/${id}`),
  submit: (r: Request) => post<Job>("/api/jobs", r),
  cancel: (id: string) => post<void>(`/api/jobs/${id}/cancel`),
  retry: (id: string) => post<Job>(`/api/jobs/${id}/retry`),
  /** Deletes a job and the files it made (files another job still uses stay). */
  remove: (id: string) => call<void>(`/api/jobs/${id}`, { method: "DELETE" }),
  rename: (id: string, title: string) => post<Job>(`/api/jobs/${id}/title`, { title }),
  move: (id: string, folder: string) => post<void>(`/api/jobs/${id}/folder`, { folder }),
  renameFolder: (from: string, to: string) => post<void>("/api/folders/rename", { from, to }),
  star: (id: string, starred: boolean) => post<Job>(`/api/jobs/${id}/star`, { starred }),
  setCover: (id: string, image: string) => post<Job>(`/api/jobs/${id}/cover`, { image }),
  unload: () => post<void>("/api/unload"),
  // the server wraps the list: {"prompts": [...]}
  prompts: async () => (await call<{ prompts: Prompt[] }>("/api/prompts")).prompts ?? [],
  savePrompt: (p: Partial<Prompt>) => post<Prompt>("/api/prompts", p),
  deletePrompt: (id: string) => call<void>(`/api/prompts/${id}`, { method: "DELETE" }),
  /** Uploads a file; returns its /files/ url. With `import`, an audio file becomes a song in the library. */
  upload: async (file: File, opts: { import?: boolean; folder?: string } = {}) => {
    const fd = new FormData();
    fd.append("file", file);
    if (opts.import) fd.append("import", "1");
    if (opts.folder) fd.append("folder", opts.folder);
    return call<{ url: string; job?: Job }>("/api/upload", { method: "POST", body: fd });
  },

  // the voice library (docs/voice-avatar.md)
  voices: async (): Promise<VoicesResponse> => {
    // a bare list, or {voices, presets}
    const r = await call<Voice[] | VoicesResponse>("/api/voices");
    return Array.isArray(r) ? { voices: r, presets: {} } : { voices: r.voices ?? [], presets: r.presets ?? {} };
  },
  /** A cloned voice: a sample of the speaker (converted and trimmed by the server); without a transcript it is transcribed. */
  cloneVoice: (name: string, sample: Blob, transcript?: string, filename = "sample.webm") => {
    const fd = new FormData();
    fd.append("name", name);
    fd.append("sample", sample, filename);
    if (transcript?.trim()) fd.append("transcript", transcript.trim());
    return call<Voice>("/api/voices", { method: "POST", body: fd });
  },
  /** A designed voice, from a description. */
  designVoice: (v: { name: string; description: string; language?: string }) => post<Voice>("/api/voices", v),
  updateVoice: (id: string, p: { name?: string; transcript?: string; description?: string }) =>
    call<Voice>(`/api/voices/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify(p) }),
  deleteVoice: (id: string) => call<void>(`/api/voices/${encodeURIComponent(id)}`, { method: "DELETE" }),

  // trained styles of the song models (docs/architecture.md, "Train your own style")
  styles: async (): Promise<StylesResponse> => {
    const r = await call<Partial<StylesResponse>>("/api/styles");
    return { styles: r.styles ?? [], models: r.models ?? [], defaults: r.defaults as StyleSettings, community: r.community ?? [] };
  },
  /** What a LoRA repository on Hugging Face says about itself (owner/name[@rev] or a link), before it is added. */
  lookupCommunityStyle: (repo: string, worker?: string) =>
    call<CommunityLookup>(`/api/community-styles?repo=${encodeURIComponent(repo)}${worker ? `&worker=${encodeURIComponent(worker)}` : ""}`),
  /** A community style: a LoRA from Hugging Face (pinned); trigger and base model when its repository names none. */
  addCommunityStyle: (body: { repo: string; rev?: string; name?: string; trigger?: string; baseModel?: string }, worker?: string) =>
    post<Style>(`/api/styles${worker ? `?worker=${encodeURIComponent(worker)}` : ""}`, body),
  style: (id: string) => call<Style>(`/api/styles/${encodeURIComponent(id)}`),
  /** A new style on a Mac (`worker`, with several; else the primary). */
  createStyle: (name: string, baseModel: string, worker?: string) =>
    post<Style>(`/api/styles${worker ? `?worker=${encodeURIComponent(worker)}` : ""}`, { name, baseModel }),
  updateStyle: (id: string, p: { name?: string; description?: string; strength?: number; settings?: StyleSettings }) =>
    call<Style>(`/api/styles/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify(p) }),
  deleteStyle: (id: string) => call<void>(`/api/styles/${encodeURIComponent(id)}`, { method: "DELETE" }),
  /** Tracks you own: converted on the Mac (48 kHz stereo), cut into segments when trained. */
  addStyleTracks: (id: string, files: File[], vocals = false) => {
    const fd = new FormData();
    for (const f of files) fd.append("file", f);
    if (vocals) fd.append("vocals", "1");
    return call<Style>(`/api/styles/${encodeURIComponent(id)}/tracks`, { method: "POST", body: fd });
  },
  deleteStyleTrack: (id: string, track: string) => call<Style>(`/api/styles/${encodeURIComponent(id)}/tracks/${encodeURIComponent(track)}`, { method: "DELETE" }),
  /** Submits the training job (it resumes from the last checkpoint unless `fresh`). */
  trainStyle: (id: string, opts: { steps?: number; settings?: StyleSettings; fresh?: boolean } = {}) =>
    post<{ style: Style; job: Job }>(`/api/styles/${encodeURIComponent(id)}/train`, opts),

  // chat with tools (docs/chat-tools.md)
  /** The home tools by MCP server, and the tools allowed always. */
  tools: () => call<ToolsResponse>("/api/tools"),
  /** The tools allowed always (they run without an approval card). */
  approvals: async () => (await call<{ always: string[] | null }>("/api/approvals")).always ?? [],
  allowAlways: async (tool: string) => (await call<{ always: string[] | null }>(`/api/approvals/${encodeURIComponent(tool)}`, { method: "PUT" })).always ?? [],
  askAgain: async (tool: string) => (await call<{ always: string[] | null }>(`/api/approvals/${encodeURIComponent(tool)}`, { method: "DELETE" })).always ?? [],
  /** A chat as plain text (from its first turn to the newest of the branch `id` is on). */
  transcript: async (id: string, opts: { thinking?: boolean; full?: boolean } = {}) => {
    const q = new URLSearchParams();
    if (opts.thinking) q.set("thinking", "1");
    if (opts.full) q.set("full", "1");
    const r = await fetch(`/api/jobs/${encodeURIComponent(id)}/transcript${q.size ? "?" + q : ""}`, { credentials: "same-origin", cache: "no-store" });
    if (!r.ok) {
      const e = await failure(r);
      if (e instanceof SignInError) unauthorized();
      throw e;
    }
    return r.text();
  },
};

/** A sentence for an error, for a toast or an empty state. */
export function errorText(e: unknown): string {
  if (e instanceof OfflineError) return "The Mac is offline. Try again when it is back.";
  if (e instanceof NoWorkerError) return `${e.message.replace(/[.\s]+$/, "")} — connect a bigger Mac or pick a smaller model.`;
  if (e instanceof ApiError) return e.message;
  if (e instanceof TypeError) return "Can't reach the server.";
  return e instanceof Error ? e.message : String(e);
}
