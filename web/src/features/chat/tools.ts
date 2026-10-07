// Chat with tools (docs/chat-tools.md): the calls of a turn, where they sit in the answer, and approvals.
//
// The shared job stream (store/app.ts) keeps only what it knows (text, stats, result); the `tool` events are
// heard here, on a stream of our own that runs while the app is live. Each call remembers where in the streamed
// text it happened (`at`), so the answer can show it between the words before and after it. A turn that ended is
// laid out from its `result.messages` (the history with the tool turns), which also works after a reload.

import { create } from "zustand";
import { toast } from "sonner";
import type { Job, Message, ToolEvent, Update, Zone } from "@/api/types";
import { navigate } from "@/lib/route";
import { roundStarts } from "@/lib/thinkSteps";
import { isActive, onUpdate, useApp } from "@/store/app";

/** A call as the page knows it. */
export interface Call extends ToolEvent {
  /** Where in the job's text the call happened (characters), when seen live. */
  at?: number;
  /** When it entered its current state (ms), when seen live. */
  since?: number;
  /** What the user decided here (the card says so afterwards). */
  decided?: "allow" | "deny";
  /** It waited for approval (seen live). */
  asked?: boolean;
}

export interface JobCalls {
  order: string[];
  byId: Record<string, Call>;
  /** The text length as this stream counts it (to place the calls). */
  len: number;
}

interface ToolsState {
  jobs: Record<string, JobCalls>;
  hear: (jobId: string, ev: ToolEvent) => void;
  text: (jobId: string, chunk: string) => void;
  reset: (jobId: string, len: number) => void;
  decide: (jobId: string, call: string, d: "allow" | "deny") => void;
}

export const useTools = create<ToolsState>((set, get) => ({
  jobs: {},
  hear: (jobId, ev) => {
    const cur = get().jobs[jobId] ?? start(jobId);
    const prev = cur.byId[ev.id];
    const at = prev?.at ?? cur.len;
    const since = prev && prev.state === ev.state ? prev.since : Date.now();
    const call: Call = { ...prev, ...ev, at, since, asked: prev?.asked || ev.state === "approval" };
    set({
      jobs: {
        ...get().jobs,
        [jobId]: { ...cur, order: prev ? cur.order : [...cur.order, ev.id], byId: { ...cur.byId, [ev.id]: call } },
      },
    });
    if (ev.state === "approval" && prev?.state !== "approval") askAttention(jobId, ev);
  },
  text: (jobId, chunk) => {
    const cur = get().jobs[jobId];
    if (cur) return set({ jobs: { ...get().jobs, [jobId]: { ...cur, len: cur.len + chunk.length } } });
    // first heard mid-job: the shared stream's text, with this chunk counted once (it may have it already)
    const main = useApp.getState().jobs[jobId]?.text ?? "";
    const len = main.endsWith(chunk) ? main.length : main.length + chunk.length;
    set({ jobs: { ...get().jobs, [jobId]: { order: [], byId: {}, len } } });
  },
  reset: (jobId, len) => {
    const cur = get().jobs[jobId] ?? { order: [], byId: {}, len: -1 };
    if (cur.len !== len) set({ jobs: { ...get().jobs, [jobId]: { ...cur, len } } });
  },
  decide: (jobId, call, d) => {
    const cur = get().jobs[jobId];
    const c = cur?.byId[call];
    if (!cur || !c) return;
    set({ jobs: { ...get().jobs, [jobId]: { ...cur, byId: { ...cur.byId, [call]: { ...c, decided: d } } } } });
  },
}));

/** A job first heard of mid-stream: its text so far is what the shared stream has. */
const start = (jobId: string): JobCalls => ({ order: [], byId: {}, len: (useApp.getState().jobs[jobId]?.text ?? "").length });

function askAttention(jobId: string, ev: ToolEvent) {
  if (location.hash.startsWith("#/chat") && document.visibilityState === "visible") return;
  toast("A tool waits for your OK", {
    description: `${callTitle(ev).text} — approve or deny it in the chat.`,
    duration: 15_000,
    action: { label: "Open", onClick: () => navigate("chat", jobId) },
  });
}

// ---------- our stream of tool events ----------

/** Jobs placed by this stream: chats with tools. */
const tracked = (id: string) => !!useTools.getState().jobs[id] || !!useApp.getState().jobs[id]?.request.zone;

/** Hears the shared event stream (store/app.ts): tool events and the text around them. */
function hear(u: Update) {
  const ev = u.event;
  if (ev?.type === "tool" && ev.data && typeof ev.data === "object") useTools.getState().hear(u.jobId, ev.data as unknown as ToolEvent);
  else if (ev?.type === "text" && ev.text && tracked(u.jobId)) useTools.getState().text(u.jobId, ev.text);
  // a whole job (it started, it ended): its text is the truth to count on from
  else if (u.job && (useTools.getState().jobs[u.jobId] || (u.job.kind === "chat" && u.job.request.zone)))
    useTools.getState().reset(u.jobId, (u.job.text ?? "").length);
}

let started = false;
/** Starts listening for tool events (once; the chat page calls it). */
export function listenForTools() {
  if (started) return;
  started = true;
  onUpdate(hear);
}

/** POST /api/jobs/{id}/approve. With `always`, the tool is also allowed always (it runs without asking from now on). */
export async function approve(jobId: string, call: string, allow: boolean, always = false) {
  const r = await fetch(`/api/jobs/${encodeURIComponent(jobId)}/approve`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(always && allow ? { call, allow, always } : { call, allow }),
  });
  if (r.status === 409) throw new ApprovalGone();
  if (!r.ok) throw new Error((await r.text().catch(() => "")).trim() || r.statusText);
  useTools.getState().decide(jobId, call, allow ? "allow" : "deny");
}

/** 409: nothing waits for an answer any more (decided elsewhere, or timed out). */
export class ApprovalGone extends Error {
  constructor() {
    super("Already decided, or it timed out.");
    this.name = "ApprovalGone";
  }
}

/** No answer within this long = denied (the worker's rule). */
export const approvalTimeout = 5 * 60_000;

// ---------- a turn, laid out ----------

export type Segment = { kind: "text"; key: string; text: string } | { kind: "tool"; key: string; call: Call };

/** The history a job sent and got back: `result.messages` once it ended. */
export function resultMessages(j: Job): Message[] | undefined {
  const ms = j.result?.messages;
  return Array.isArray(ms) && ms.length ? (ms as Message[]) : undefined;
}

/** The messages this turn added (after its question). */
function turnMessages(j: Job): Message[] {
  const ms = resultMessages(j) ?? [];
  let i = ms.length - 1;
  while (i >= 0 && ms[i].Role !== "user") i--;
  return ms.slice(i + 1);
}

/** Calls the server keeps on the job (if it does): `job.tools`, or a list in `stats.toolCalls`. */
function keptCalls(j: Job): ToolEvent[] {
  if (Array.isArray(j.tools)) return j.tools;
  const t = j.stats?.toolCalls;
  return Array.isArray(t) ? (t as ToolEvent[]) : [];
}

const parseArgs = (a: unknown): unknown => {
  if (typeof a !== "string") return a;
  try {
    return JSON.parse(a);
  } catch {
    return a;
  }
};

/** The calls of a job (what is known of them), in order. */
export function callsOf(j: Job, live?: JobCalls): Call[] {
  const known = new Map<string, Call>();
  const order: string[] = [];
  const add = (c: Call) => {
    const prev = known.get(c.id);
    if (!prev) order.push(c.id);
    known.set(c.id, { ...prev, ...c, at: prev?.at ?? c.at, since: prev?.since ?? c.since, decided: prev?.decided ?? c.decided, asked: prev?.asked || c.asked });
  };
  for (const c of keptCalls(j)) add(c);
  for (const id of live?.order ?? []) add(live!.byId[id]);
  return order.map((id) => known.get(id)!);
}

/**
 * The answer in pieces: text, a call, text … Live calls sit where they happened in the text; a turn that ended
 * is laid out from its history (its calls matched by id, else in order, to what the events said about them).
 */
export function segmentsOf(j: Job, live?: JobCalls): Segment[] {
  const calls = callsOf(j, live);
  const turn = isActive(j) ? [] : turnMessages(j);
  const fromHistory = turn.some((m) => m.ToolCalls?.length);
  // an ended turn's history is exact; live positions are only needed while it streams
  if (fromHistory) {
    const out: Segment[] = [];
    const byId = new Map(calls.map((c) => [c.id, c]));
    const results = new Map<string, string>();
    for (const m of turn) if (m.Role === "tool" && m.ToolCallID) results.set(m.ToolCallID, m.Content);
    let n = 0;
    const used = new Set<string>();
    turn.forEach((m, i) => {
      if (m.Role === "assistant") {
        if (m.Content?.trim()) out.push({ kind: "text", key: `t${i}`, text: m.Content });
        m.ToolCalls?.forEach((tc, k) => {
          const id = tc.ID ?? "";
          // by id; else the next unmatched call of that name
          const ev = (id && byId.get(id)) || calls.find((c) => !used.has(c.id) && c.name === tc.Name);
          n++;
          if (ev) used.add(ev.id);
          const result = (id && results.get(id)) ?? turn.filter((x) => x.Role === "tool")[n - 1]?.Content;
          const held = !!result && /^\[result held back/i.test(result);
          const call: Call = ev ?? {
            id: id || `h${i}-${k}`,
            name: tc.Name,
            zone: zoneOfTool(tc.Name, j.request.zone ?? ""),
            arguments: parseArgs(tc.Arguments),
            state: held ? "held" : "done",
            result,
          };
          out.push({ kind: "tool", key: `c${call.id}`, call: { ...call, result: call.result ?? result } });
        });
      }
    });
    // calls the history does not show (blocked ones, say) go after the call they followed live
    calls.forEach((c, i) => {
      if (used.has(c.id)) return;
      const before = calls.slice(0, i).reverse().find((p) => used.has(p.id));
      const k = before ? out.findIndex((s) => s.kind === "tool" && s.call.id === before.id) + 1 : out.length && out[out.length - 1].kind === "text" ? out.length - 1 : 0;
      out.splice(k, 0, { kind: "tool", key: `c${c.id}`, call: c });
      used.add(c.id);
    });
    return out;
  }

  const text = j.text ?? "";
  if (!calls.length) return text ? [{ kind: "text", key: "t0", text }] : [];
  const out: Segment[] = [];
  let from = 0;
  const sorted = placeCalls(text, calls);
  for (const c of sorted) {
    const at = Math.min(Math.max(from, c.at ?? text.length), text.length);
    const piece = text.slice(from, at);
    if (piece.trim()) out.push({ kind: "text", key: `t${from}`, text: piece });
    out.push({ kind: "tool", key: `c${c.id}`, call: c });
    from = at;
  }
  const tail = text.slice(from);
  if (tail.trim() || isActive(j)) out.push({ kind: "text", key: `t${from}`, text: tail });
  return out;
}

/**
 * The calls in text order. A call heard before the page listened has no place: with think on, every round after a
 * call starts with "<think>" (lib/thinkSteps.ts roundStarts), so it goes where the round after it began; else at
 * the end.
 */
function placeCalls(text: string, calls: Call[]): Call[] {
  const starts = roundStarts(text);
  let r = 0;
  const placed = calls.map((c) => {
    if (c.at != null) {
      while (r < starts.length && starts[r] <= c.at) r++;
      return c;
    }
    return { ...c, at: starts[r++] };
  });
  return placed.sort((a, b) => (a.at ?? text.length) - (b.at ?? text.length));
}

/** Files the turn made: those of its calls, then the job's own. */
export function filesOf(j: Job, segments: Segment[]): string[] {
  const all = [...segments.flatMap((s) => (s.kind === "tool" ? (s.call.files ?? []) : [])), ...(j.files ?? [])];
  return [...new Set(all)];
}

/** The calls of the open conversation that wait for the user. */
export function pendingOf(jobs: Job[], tools: Record<string, JobCalls>): number {
  let n = 0;
  for (const j of jobs) {
    if (!isActive(j)) continue;
    for (const c of callsOf(j, tools[j.id])) if (c.state === "approval") n++;
  }
  return n;
}

// ---------- words ----------

/** A tool's zone when the event does not say (the history): web tools by name, files tools, else home. */
export function zoneOfTool(name: string, chat: Zone): Zone {
  if (/^web_/.test(name)) return "web";
  if (/^make_file|^files?_/.test(name)) return "files";
  return chat === "home" ? "home" : chat || "home";
}

const str = (v: unknown) => (typeof v === "string" ? v : v == null ? "" : JSON.stringify(v));
export const argsOf = (c: { arguments?: unknown }): Record<string, unknown> => {
  const a = parseArgs(c.arguments);
  return a && typeof a === "object" && !Array.isArray(a) ? (a as Record<string, unknown>) : a == null || a === "" ? {} : { value: a };
};

export function domainOf(url: string): string {
  try {
    return new URL(url).hostname.replace(/^www\./, "");
  } catch {
    return url.replace(/^https?:\/\//, "").split("/")[0];
  }
}

/** "vboard.search_cards", "vboard__search_cards" → ["vboard", "search_cards"]. */
export function serverAndTool(name: string): [string | undefined, string] {
  const m = name.match(/^([^.:/]+?)(?:__|\.|:|\/)(.+)$/);
  return m ? [m[1], m[2]] : [undefined, name];
}

/** "create_card" → "Create card". */
export const humanize = (s: string) => {
  const t = s.replace(/[_-]+/g, " ").replace(/([a-z])([A-Z])/g, "$1 $2").trim().toLowerCase();
  return t.charAt(0).toUpperCase() + t.slice(1);
};

const quote = (s: string) => `“${s.length > 70 ? s.slice(0, 68).trimEnd() + " …" : s}”`;

/** A human title: "Searched the web for “…”", "Read example.com", "vboard · search_cards", "Made report.pdf". */
export function callTitle(c: Pick<ToolEvent, "name" | "arguments" | "state">): { text: string; detail?: string } {
  const a = argsOf(c);
  const doing = c.state === "running" || c.state === "approval";
  const failed = c.state === "blocked" || c.state === "denied" || c.state === "error";
  switch (c.name) {
    case "web_search": {
      const q = str(a.query ?? a.q ?? a.value);
      const verb = doing ? "Searching" : failed ? "Search" : "Searched";
      return { text: `${verb} the web${q ? " for " + quote(q) : ""}` };
    }
    case "web_fetch": {
      const url = str(a.url ?? a.value);
      return { text: `${doing ? "Reading" : "Read"} ${url ? domainOf(url) : "a page"}`, detail: url || undefined };
    }
    case "make_file": {
      const name = str(a.name ?? a.filename ?? a.file ?? a.path).split("/").pop() || "a file";
      return { text: `${doing ? "Making" : failed ? "Make" : "Made"} ${name}` };
    }
  }
  const [server, tool] = serverAndTool(c.name);
  return { text: server ? `${server} · ${tool}` : tool };
}
