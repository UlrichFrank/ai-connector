// Conversations, derived from chat jobs.
//
// A chat turn is a job that carries the whole history. The chat page sets `request.parent` on every turn it sends:
//   * a continuation points at the job whose answer it continues (it has more messages than that job);
//   * a regenerated answer, or another model's answer in a compare chat, points at a job with the same messages
//     (a sibling: the same place in the conversation).
// Jobs made elsewhere (the old UI, the CLI) have no parent: a job whose history ends with another job's
// question + answer continues it; first questions asked the same within two minutes form one compare chat.

import type { Job, Message, Prompt } from "@/api/types";
import { partsOf, wordsOf } from "@/lib/thinkSteps";
import { isActive } from "@/store/app";

/**
 * The answer without its thinking: every thought joined (null when there was none, or only Qwen's empty one), the
 * words joined, and whether it ends inside a thought (lib/thinkSteps.ts has the rules).
 */
export function splitThink(text: string | undefined): { think: string | null; answer: string; thinking: boolean } {
  const parts = partsOf(text);
  const thoughts = parts.filter((p) => p.kind === "think");
  const last = parts[parts.length - 1];
  return {
    think: thoughts.length ? thoughts.map((p) => p.text).join("\n\n") : null,
    answer: wordsOf(text),
    thinking: last?.kind === "think" && last.open,
  };
}

/** What a turn's answer adds to the history. */
export const historyAnswer = (j: Job) => splitThink(j.text).answer;

interface Info {
  /** The messages without the system prompt. */
  turns: Message[];
  system?: string;
  fp: string;
  prefixFp: string;
  /** The history plus this job's answer (set once the job ended: only then can it be continued). */
  answerFp?: string;
}

const key = (ms: Message[]) => JSON.stringify(ms.map((m) => [m.Role[0], m.Content.trim()]));

// jobs are replaced (not changed) on each event: a job that did not change keeps its fingerprints
const cache = new WeakMap<Job, Info>();
function info(j: Job): Info {
  let i = cache.get(j);
  if (!i) {
    const ms = j.request.messages ?? [];
    const turns = ms.filter((m) => m.Role !== "system");
    i = {
      turns,
      system: ms.find((m) => m.Role === "system")?.Content,
      fp: key(turns),
      prefixFp: key(turns.slice(0, -1)),
      answerFp: isActive(j) ? undefined : key([...turns, { Role: "assistant", Content: historyAnswer(j) }]),
    };
    cache.set(j, i);
  }
  return i;
}

export const turnsOf = (j: Job) => info(j).turns;
export const systemOf = (j: Job) => info(j).system;
export const questionOf = (j: Job) => info(j).turns.at(-1)?.Content ?? "";

export interface Conversation {
  /** The first job (its id is the conversation's address). */
  id: string;
  root: Job;
  title: string;
  /** Every job of the conversation. */
  jobs: Job[];
  /** The first answers (one per model in a compare chat, plus regenerated ones). */
  roots: Job[];
  /** The newest job's creation time. */
  updated: string;
  active: boolean;
}

export interface Threads {
  conversations: Conversation[];
  byJob: Map<string, Conversation>;
  /** The turn a job continues (undefined for a first question). */
  parent: Map<string, string | undefined>;
  /** The continuations of a job, oldest first. */
  children: Map<string, Job[]>;
}

/** The jobs Enhance, song writing and covers made are chat jobs too; they are not conversations. */
function helperTexts(prompts: Prompt[]) {
  return new Set(prompts.filter((p) => p.use !== "chat").flatMap((p) => [p.text.trim(), (p.default ?? "").trim()].filter(Boolean)));
}

/**
 * A job of a helper (lib/jobs.ts runChat): a system prompt of another use, or runChat's signature (one question
 * with a system prompt at temperature 0.7 and 900 tokens, no parent) when its prompt was edited since.
 */
function isHelper(j: Job, helpers: Set<string>) {
  const i = info(j);
  if (i.system == null) return false;
  if (helpers.has(i.system.trim())) return true;
  const r = j.request;
  return i.turns.length === 1 && !r.parent && r.temperature === 0.7 && r.maxTokens === 900;
}

export function deriveThreads(all: Record<string, Job>, prompts: Prompt[], links: Record<string, string>): Threads {
  const helpers = helperTexts(prompts);
  const jobs = Object.values(all)
    .filter((j) => j.kind === "chat" && info(j).turns.at(-1)?.Role === "user" && !isHelper(j, helpers))
    .sort((a, b) => a.created.localeCompare(b.created) || a.id.localeCompare(b.id));
  const byId = new Map(jobs.map((j) => [j.id, j]));

  // the job whose answer each job's history ends with (fallback for jobs without a parent)
  const byAnswer = new Map<string, Job>();
  for (const j of jobs) {
    const a = info(j).answerFp;
    if (a) byAnswer.set(a, j); // the newest wins
  }

  // sibling-of and child-of
  const sibling = new Map<string, string>();
  const parentOf = new Map<string, string | undefined>();
  for (const j of jobs) {
    const p = j.request.parent;
    const me = info(j);
    const pj = p ? byId.get(p) : undefined;
    if (pj && pj.id !== j.id) {
      const pt = info(pj).turns.length;
      if (pt === me.turns.length) sibling.set(j.id, pj.id);
      else if (pt < me.turns.length) parentOf.set(j.id, pj.id);
      continue;
    }
    if (p && me.turns.length === 1) {
      sibling.set(j.id, p); // its anchor is gone: it still joins the others that point there
      continue;
    }
    if (me.turns.length >= 3) {
      const a = byAnswer.get(me.prefixFp);
      if (a && a.created <= j.created && a.id !== j.id) parentOf.set(j.id, a.id);
    }
  }
  // a sibling continues what its sibling continues
  const resolve = (id: string, seen = new Set<string>()): string | undefined => {
    if (parentOf.has(id)) return parentOf.get(id);
    const s = sibling.get(id);
    if (!s || seen.has(s) || !byId.has(s)) return undefined;
    seen.add(id);
    return resolve(s, seen);
  };
  const parent = new Map<string, string | undefined>();
  for (const j of jobs) parent.set(j.id, resolve(j.id));

  // first questions → conversations (union-find)
  const up = new Map<string, string>();
  const find = (x: string): string => {
    let r = x;
    while (up.has(r) && up.get(r) !== r) r = up.get(r)!;
    up.set(x, r);
    return r;
  };
  const union = (a: string, b: string) => {
    const ra = find(a);
    const rb = find(b);
    if (ra !== rb) up.set(rb, ra);
  };
  const roots = jobs.filter((j) => !parent.get(j.id));
  const recent = new Map<string, Job>();
  for (const r of roots) {
    find(r.id);
    const s = sibling.get(r.id);
    if (s) union(s, r.id);
    if (links[r.id]) union(links[r.id], r.id);
    const fp = info(r).fp + "\u0000" + (info(r).system ?? "") + "\u0000" + (r.request.zone ?? "");
    const last = recent.get(fp);
    if (last && Date.parse(r.created) - Date.parse(last.created) < 120_000) union(last.id, r.id);
    recent.set(fp, r);
  }

  const children = new Map<string, Job[]>();
  const convOf = new Map<string, string>(); // job → group key
  for (const j of jobs) {
    const p = parent.get(j.id);
    if (p) {
      const c = children.get(p);
      if (c) c.push(j);
      else children.set(p, [j]);
      convOf.set(j.id, convOf.get(p) ?? find(p));
    } else convOf.set(j.id, find(j.id));
  }

  const groups = new Map<string, Job[]>();
  for (const j of jobs) {
    const g = convOf.get(j.id)!;
    const list = groups.get(g);
    if (list) list.push(j);
    else groups.set(g, [j]);
  }
  const conversations: Conversation[] = [];
  const byJob = new Map<string, Conversation>();
  for (const list of groups.values()) {
    const rs = list.filter((j) => !parent.get(j.id));
    const root = rs[0] ?? list[0];
    const c: Conversation = {
      id: root.id,
      root,
      title: root.request.title || firstLine(info(root).turns[0]?.Content ?? "") || "New chat",
      jobs: list,
      roots: rs,
      updated: list.reduce((m, j) => (j.created > m ? j.created : m), ""),
      active: list.some(isActive),
    };
    conversations.push(c);
    for (const j of list) byJob.set(j.id, c);
  }
  conversations.sort((a, b) => b.updated.localeCompare(a.updated));
  return { conversations, byJob, parent, children };
}

function firstLine(s: string) {
  const t = s.trim().split("\n")[0].trim();
  return t.length > 80 ? t.slice(0, 78).trimEnd() + " …" : t;
}

// ---------- columns: one per model of a compare chat ----------

export interface Turn {
  question: string;
  /** The job that answered, or the answer as the history has it (turns from before the first job). */
  job?: Job;
  answer?: string;
  /** Where the job sits: the answers there are its variants (regenerated). */
  slot?: string;
  variants?: Job[];
}

export interface Column {
  key: string;
  model: string;
  turns: Turn[];
  /** The last job of the chain shown. */
  leaf?: Job;
}

/** The variant shown at a place: the one picked, else the newest. */
function pickOf(variants: Job[], slot: string, picks: Record<string, string>) {
  return variants.find((v) => v.id === picks[slot]) ?? variants[variants.length - 1];
}

export function columnsOf(c: Conversation, t: Threads, picks: Record<string, string>): Column[] {
  const models: string[] = [];
  for (const r of c.roots) if (!models.includes(r.model)) models.push(r.model);
  return models.map((model) => {
    const slot = `root:${c.id}:${model}`;
    const variants = c.roots.filter((r) => r.model === model);
    let job: Job | undefined = pickOf(variants, slot, picks);
    const turns: Turn[] = [];
    // history before the first job (a conversation continued from elsewhere)
    const hist = info(job).turns.slice(0, -1);
    for (let i = 0; i + 1 < hist.length; i += 2) turns.push({ question: hist[i].Content, answer: hist[i + 1].Content });
    let leaf: Job | undefined;
    let s = slot;
    let vs = variants;
    while (job) {
      turns.push({ question: questionOf(job), job, slot: s, variants: vs });
      leaf = job;
      const kids = t.children.get(job.id);
      if (!kids?.length) break;
      s = job.id;
      vs = kids;
      job = pickOf(kids, s, picks);
    }
    return { key: model, model, turns, leaf };
  });
}

/** The picks that show a job (and the turns before it). */
export function picksFor(id: string, c: Conversation, t: Threads): Record<string, string> {
  const out: Record<string, string> = {};
  let cur = c.jobs.find((j) => j.id === id);
  while (cur) {
    const p = t.parent.get(cur.id);
    out[p ?? `root:${c.id}:${cur.model}`] = cur.id;
    cur = p ? c.jobs.find((j) => j.id === p) : undefined;
  }
  return out;
}
