// A follow-up turn (docs/chat-tools.md): to each column's last turn, with its history, as the composer sends it.

import { api } from "@/api/client";
import type { Job, Message, Zone } from "@/api/types";
import { isActive } from "@/store/app";
import { historyAnswer, turnsOf, type Column, type Threads } from "./threads";
import { resultMessages } from "./tools";

/** The sampling fields of a chat request: unset ones take the model's defaults (Model.sampling). */
export interface ChatOpts {
  temperature?: number;
  greedy?: boolean;
  maxTokens?: number;
  think?: boolean;
}

export interface FollowUp {
  columns: Column[];
  threads: Threads;
  zone: Zone;
  text: string;
  /** One model for all columns (a single-model chat whose model was changed), else each column's own. */
  model?: string;
  /** The system prompt for a turn continuing `leaf` (plain chats; a chat with tools keeps its history's). */
  system: (leaf: Job) => string | undefined;
  opts: ChatOpts;
  /** Each job as soon as it is made. */
  put: (j: Job) => void;
}

/** Sends `text` as the next turn of every column; returns the jobs made. */
export async function followUp({ columns, threads, zone, text, model, system, opts, put }: FollowUp): Promise<Job[]> {
  const made: Job[] = [];
  for (const col of columns) {
    const leaf = col.leaf;
    if (!leaf) continue;
    // with tools, the history the worker gave back (tool turns included) goes back unchanged: the worker
    // refuses an edited one; the zone comes with the parent
    const back = zone && !isActive(leaf) && leaf.state !== "failed" ? resultMessages(leaf) : undefined;
    const answered = !!back || !!historyAnswer(leaf);
    const history = answered ? [...turnsOf(leaf), { Role: "assistant" as const, Content: historyAnswer(leaf) }] : turnsOf(leaf).slice(0, -1);
    // an unanswered (failed) turn is asked again in its place
    const parent = answered ? leaf.id : (threads.parent.get(leaf.id) ?? leaf.id);
    const sys = system(leaf);
    const messages: Message[] = back
      ? [...back, { Role: "user", Content: text }]
      : [...(sys ? [{ Role: "system" as const, Content: sys }] : []), ...history, { Role: "user", Content: text }];
    const j = await api.submit({ model: model ?? col.model, messages, ...opts, parent });
    put(j);
    made.push(j);
  }
  return made;
}
