// The queues of the chats (lib/chatQueue.ts), one per conversation, kept in localStorage (this browser) and in
// step with other tabs (storage events), so a reload finds them and two tabs do not send one message twice.

import { create } from "zustand";
import { emptyQueue, prune, queuedCount, type ChatQueue } from "@/lib/chatQueue";
import { local } from "@/lib/storage";

/** How a queued message is sent: as the conversation sends when the user queued it. */
export interface SendConfig {
  /** One model for a single-model chat (undefined: each column's own). */
  model?: string;
  /** The system prompt choice (Settings.system). */
  system: string;
  /** null: the model's default (Auto); 0: greedy. */
  temperature: number | null;
  /** null: Auto. */
  maxTokens: number | null;
  think: boolean;
}

export type Queue = ChatQueue<SendConfig>;

const KEY = "chat.queue";
const read = () => local.get<Record<string, Queue>>(KEY, {});

interface QueueState {
  queues: Record<string, Queue>;
  /** Changes one conversation's queue (read fresh from storage first: another tab may have changed it). */
  update: (conv: string, f: (q: Queue) => Queue) => Queue;
  /** Forgets a conversation's queue (deleted). */
  clear: (conv: string) => void;
}

export const useQueue = create<QueueState>((set) => ({
  queues: prune(read()),
  update: (conv, f) => {
    const all = read();
    const next = f(all[conv] ?? emptyQueue<SendConfig>());
    const queues = prune({ ...all, [conv]: next });
    local.set(KEY, queues);
    set({ queues });
    return next;
  },
  clear: (conv) => {
    const { [conv]: _, ...rest } = read();
    local.set(KEY, rest);
    set({ queues: rest });
  },
}));

// another tab changed a queue
if (typeof window !== "undefined")
  window.addEventListener("storage", (e) => {
    if (e.key === "mlxlab." + KEY) useQueue.setState({ queues: prune(read()) });
  });

/** The messages queued in a conversation. */
export const useQueued = (conv: string | undefined) => useQueue((s) => (conv ? queuedCount(s.queues[conv]) : 0));

let n = 0;
export const queueId = () => `q${Date.now().toString(36)}${(n++).toString(36)}${Math.random().toString(36).slice(2, 6)}`;

// the queues found on load: each is checked once against its turns (lib/chatQueue.ts restore)
const unchecked = new Set(Object.keys(useQueue.getState().queues));
/** True the first time for a queue that was there when the page loaded. */
export function fromLoad(conv: string): boolean {
  return unchecked.delete(conv);
}
