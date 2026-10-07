// Queued chat messages (features/chat): what the user typed while an answer was running, sent one after another
// as the turns end. Pure: the chat page keeps one queue per conversation (in localStorage) and asks `nextStep`
// whenever the jobs change. No imports (node --test runs it as is).

/** A message waiting for its turn. */
export interface Queued {
  id: string;
  text: string;
}

/** Why a queue stopped sending: never into a broken state silently. */
export type PauseReason = "failed" | "canceled" | "denied" | "gone" | "error" | "interrupted";

export interface ChatQueue<C = unknown> {
  items: Queued[];
  /** The turns (job ids) the next message waits for: the answers running when it was queued, then those it sent. */
  after: string[];
  /** Ended turns whose failure the user let pass (Continue, Send now): they do not pause the queue. */
  ack: string[];
  paused?: PauseReason;
  /** A message being sent right now (claimed, so another tab does not send it too). */
  inflight?: Queued & { since: number };
  /** How the conversation sends (model, system prompt, options): as it was when the user last queued. */
  config?: C;
}

/** What the queue needs to know of a turn (a job). */
export interface TurnState {
  id: string;
  state: "queued" | "running" | "done" | "failed" | "canceled";
  tools?: { state: string }[];
}

export const emptyQueue = <C = unknown>(): ChatQueue<C> => ({ items: [], after: [], ack: [] });

const isRunning = (t: TurnState) => t.state === "queued" || t.state === "running";

/** What went wrong in an ended turn (a denied or timed-out approval counts), or undefined when it went well. */
export function trouble(t: TurnState): PauseReason | undefined {
  if (t.state === "failed") return "failed";
  if (t.state === "canceled") return "canceled";
  if (t.tools?.some((c) => c.state === "denied")) return "denied";
  return undefined;
}

// ---------- editing ----------

/** Adds a message at the end. Into an empty queue it waits for `running` (the turns running now). */
export function enqueue<C>(q: ChatQueue<C>, item: Queued, running: string[], config?: C): ChatQueue<C> {
  const text = item.text.trim();
  if (!text) return q;
  const fresh = !q.items.length && !q.inflight;
  return {
    ...q,
    items: [...q.items, { ...item, text }],
    after: fresh ? running : q.after,
    ack: fresh ? [] : q.ack,
    config: config ?? q.config,
  };
}

export function remove<C>(q: ChatQueue<C>, id: string): ChatQueue<C> {
  return { ...q, items: q.items.filter((i) => i.id !== id) };
}

/** Changes a message's text; an emptied message is removed. */
export function edit<C>(q: ChatQueue<C>, id: string, text: string): ChatQueue<C> {
  const t = text.trim();
  if (!t) return remove(q, id);
  return { ...q, items: q.items.map((i) => (i.id === id ? { ...i, text: t } : i)) };
}

/** Moves a message to `index` (clamped). */
export function moveTo<C>(q: ChatQueue<C>, id: string, index: number): ChatQueue<C> {
  const from = q.items.findIndex((i) => i.id === id);
  if (from < 0) return q;
  const to = Math.max(0, Math.min(q.items.length - 1, index));
  if (to === from) return q;
  const items = [...q.items];
  const [it] = items.splice(from, 1);
  items.splice(to, 0, it);
  return { ...q, items };
}

/** Moves a message up (-1) or down (+1). */
export function move<C>(q: ChatQueue<C>, id: string, delta: number): ChatQueue<C> {
  const from = q.items.findIndex((i) => i.id === id);
  return from < 0 ? q : moveTo(q, id, from + delta);
}

/** Stops sending. */
export function pause<C>(q: ChatQueue<C>, reason: PauseReason): ChatQueue<C> {
  return { ...q, paused: reason };
}

/** Continue: sends on from the turns shown now (`leaves`), whatever became of them. */
export function resume<C>(q: ChatQueue<C>, leaves: string[]): ChatQueue<C> {
  const { paused: _, ...rest } = q;
  return { ...rest, after: leaves, ack: leaves };
}

/**
 * Send now: this message goes next; the turns in `stopping` (being canceled for it) and `leaves` may end as they
 * like — their end does not pause the queue.
 */
export function sendNow<C>(q: ChatQueue<C>, id: string, stopping: string[], leaves: string[]): ChatQueue<C> {
  const moved = moveTo(q, id, 0);
  const ids = [...new Set([...stopping, ...leaves])];
  const { paused: _, ...rest } = moved;
  return { ...rest, after: ids, ack: ids };
}

/** Takes the first message to send it (it stays known as `inflight` until `sent` or `unsent`). */
export function claim<C>(q: ChatQueue<C>, now: number): [Queued | undefined, ChatQueue<C>] {
  const [first, ...items] = q.items;
  if (!first || q.inflight) return [undefined, q];
  return [first, { ...q, items, inflight: { ...first, since: now } }];
}

/** The claimed message went out as the turns `made`: the queue waits for them next. */
export function sent<C>(q: ChatQueue<C>, made: string[]): ChatQueue<C> {
  const { inflight: _, ...rest } = q;
  return { ...rest, after: made, ack: [] };
}

/** The claimed message could not be sent: it goes back to the front and the queue pauses. */
export function unsent<C>(q: ChatQueue<C>, reason: PauseReason = "error"): ChatQueue<C> {
  const { inflight, ...rest } = q;
  if (!inflight) return pause(q, reason);
  const { since: _, ...item } = inflight;
  return { ...rest, items: [item, ...rest.items.filter((i) => i.id !== item.id)], paused: reason };
}

// ---------- running ----------

export type Step = { do: "idle" } | { do: "wait" } | { do: "pause"; reason: PauseReason } | { do: "send"; item: Queued };

export interface Now {
  /** A turn of the conversation is queued or running. */
  active: boolean;
  /** Tool calls waiting for the user's OK. */
  pending: number;
  /** The turns the next message would continue (the last of each column). */
  leaves: TurnState[];
  /** The turns of `after`, as known (undefined: not known yet). */
  after: (TurnState | undefined)[];
}

/** What to do now. */
export function nextStep(q: ChatQueue<unknown>, now: Now): Step {
  if (!q.items.length || q.paused) return { do: "idle" };
  if (q.inflight) return { do: "wait" };
  if (now.active || now.pending > 0) return { do: "wait" };
  // the turns it sent are not in the stream yet
  if (now.after.some((t) => !t)) return { do: "wait" };
  if (now.after.some((t) => t && isRunning(t))) return { do: "wait" };
  if (!now.leaves.length) return { do: "pause", reason: "gone" };
  for (const t of [...now.leaves, ...(now.after as TurnState[])]) {
    if (q.ack.includes(t.id)) continue;
    const r = trouble(t);
    if (r) return { do: "pause", reason: r };
  }
  return { do: "send", item: q.items[0] };
}

/**
 * A queue found on load: it resumes only if the turns it waits for still run or ended well; otherwise it pauses
 * (a message that was being sent when the page went away: back to the front, paused — it may have gone out).
 */
export function restore<C>(q: ChatQueue<C>, lookup: (id: string) => TurnState | undefined): ChatQueue<C> {
  if (q.inflight) return unsent(q, "interrupted");
  if (!q.items.length || q.paused) return q;
  if (!q.after.length) return pause(q, "gone");
  for (const id of q.after) {
    const t = lookup(id);
    if (!t) return pause(q, "gone");
    if (isRunning(t) || q.ack.includes(id)) continue;
    const r = trouble(t);
    if (r) return pause(q, r);
  }
  return q;
}

/** "Queue paused — …" */
export function pauseText(r: PauseReason): string {
  switch (r) {
    case "failed":
      return "the last answer failed";
    case "canceled":
      return "the last answer was stopped";
    case "denied":
      return "a tool call was denied";
    case "gone":
      return "the answer it waited for is gone";
    case "error":
      return "a message could not be sent";
    case "interrupted":
      return "sending was interrupted (it may have gone out)";
  }
}

/** Drops the queues of conversations not in `keep` and empty ones. */
export function prune<C>(all: Record<string, ChatQueue<C>>, keep?: (id: string) => boolean): Record<string, ChatQueue<C>> {
  const out: Record<string, ChatQueue<C>> = {};
  for (const [id, q] of Object.entries(all)) if ((q.items.length || q.inflight) && (!keep || keep(id))) out[id] = q;
  return out;
}

/** The messages waiting (the one being sent counts). */
export const queuedCount = (q: ChatQueue<unknown> | undefined) => (q ? q.items.length + (q.inflight ? 1 : 0) : 0);
