import type { Client, ClientCreated, Connector, ConnectorInput, ErrorBody, Info, Job, LinkRequest, LoginResponse, ModelsResponse, Worker, WorkerCreated } from "./types";

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

/** Not signed in, or the device was revoked (401 {"error":"signin"}). */
export class SignInError extends ApiError {
  constructor(message = "Sign in to continue") {
    super(401, message, "signin");
    this.name = "SignInError";
  }
}

/** Too many sign-in attempts (429). */
export class RateLimitError extends ApiError {
  retryAfter?: number;
  constructor(message: string, retryAfter?: number) {
    super(429, message, "ratelimit");
    this.name = "RateLimitError";
    this.retryAfter = retryAfter;
  }
}

/**
 * The server does not have this API (yet): an endpoint of the server itself answered 404/405/501, or the server
 * passed it on to the workers (503 offline, or their 404) because it does not know it.
 */
export class NotAvailableError extends ApiError {
  constructor(status: number) {
    super(status, "Not available on this server yet", "unavailable");
    this.name = "NotAvailableError";
  }
}

export const isNotAvailable = (e: unknown): e is NotAvailableError => e instanceof NotAvailableError;

// The session (store/session.ts) hears when the server says we are no longer signed in.
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
    // workers answer some errors in plain text
  }
  const err = body.error;
  const code = typeof err === "string" ? err : err?.code;
  const message = body.message || (typeof err === "string" ? err : err?.message) || text || r.statusText;
  if (r.status === 401) return new SignInError(body.message);
  if (r.status === 429) {
    const after = Number(r.headers.get("Retry-After"));
    return new RateLimitError(message, isFinite(after) && after > 0 ? after : undefined);
  }
  return new ApiError(r.status, message, code);
}

interface CallOptions extends RequestInit {
  /** false: a 401 is the answer (sign-in), not a lost session. */
  session?: boolean;
  /** An endpoint of the server itself (clients, connectors): a 404/405/501 or a forward to the workers means "not there". */
  serverOnly?: boolean;
}

async function call<T>(path: string, { session = true, serverOnly = false, ...init }: CallOptions = {}): Promise<T> {
  const r = await fetch(path, {
    credentials: "same-origin",
    cache: "no-store",
    ...init,
    headers: { "Content-Type": "application/json", ...init.headers },
  });
  if (!r.ok) {
    const e = await failure(r);
    if (session && e instanceof SignInError) unauthorized();
    if (serverOnly && (r.status === 404 || r.status === 405 || r.status === 501 || (r.status === 503 && e.code === "offline"))) {
      // a 404 for one item ("not found" of a known id) is not "the API is missing"
      if (!(r.status === 404 && e.code === "not found" && /\/[^/]+\/[^/]+\/[^/]+$/.test(path))) throw new NotAvailableError(r.status);
    }
    throw e;
  }
  return (r.status === 204 || r.headers.get("Content-Length") === "0" ? undefined : await r.json()) as T;
}

const post = <T>(path: string, body?: unknown, opts: CallOptions = {}) =>
  call<T>(path, { method: "POST", body: body === undefined ? undefined : JSON.stringify(body), ...opts });

const enc = encodeURIComponent;

/** The server's API (docs/architecture.md, docs/api.md) as the console uses it. */
export const api = {
  info: () => call<Info>("/api/info", { session: false }),
  login: (password: string, device: string, user: string) => post<LoginResponse>("/api/login", { password, device, user }, { session: false }),
  logout: () => post<void>("/api/logout", undefined, { session: false }),

  workers: () => call<Worker[]>("/api/workers"),
  createWorker: (name: string) => post<WorkerCreated>("/api/workers", { name }),
  revokeWorker: (id: string) => call<void>(`/api/workers/${enc(id)}`, { method: "DELETE" }),
  makePrimary: (id: string) => call<unknown>(`/api/workers/${enc(id)}/primary`, { method: "PUT" }),
  autoPrimary: (id: string) => call<unknown>(`/api/workers/${enc(id)}/primary`, { method: "DELETE" }),

  models: () => call<ModelsResponse>("/api/models"),
  jobs: () => call<Job[]>("/api/jobs"),
  cancel: (id: string) => post<void>(`/api/jobs/${enc(id)}/cancel`),

  clients: () => call<Client[]>("/api/clients", { serverOnly: true }),
  createClient: (name: string) => post<ClientCreated>("/api/clients", { name }, { serverOnly: true }),
  revokeClient: (id: string) => call<void>(`/api/clients/${enc(id)}`, { method: "DELETE", serverOnly: true }),
  /** An app's link request by its PIN (any spelling: the server keeps the digits). */
  linkRequest: (code: string) => call<LinkRequest>(`/api/link/${enc(code)}`, { serverOnly: true }),
  approveLink: (code: string) => post<{ client: Client }>(`/api/link/${enc(code)}`, { approve: true }, { serverOnly: true }),
  denyLink: (code: string) => post<void>(`/api/link/${enc(code)}`, { approve: false }, { serverOnly: true }),

  connectors: () => call<Connector[]>("/api/connectors", { serverOnly: true }),
  createConnector: (c: ConnectorInput) => post<Connector>("/api/connectors", c, { serverOnly: true }),
  patchConnector: (id: string, c: ConnectorInput) => call<Connector>(`/api/connectors/${enc(id)}`, { method: "PATCH", body: JSON.stringify(c), serverOnly: true }),
  deleteConnector: (id: string) => call<void>(`/api/connectors/${enc(id)}`, { method: "DELETE", serverOnly: true }),
};

/** A sentence for an error, for a toast or an empty state. */
export function errorText(e: unknown): string {
  if (e instanceof NotAvailableError) return "Not available on this server yet.";
  if (e instanceof ApiError && e.status === 503 && e.code === "offline") return "No worker is connected. Connect one on the Workers page.";
  if (e instanceof ApiError) return e.message;
  if (e instanceof TypeError) return "Can't reach the server.";
  return e instanceof Error ? e.message : String(e);
}
