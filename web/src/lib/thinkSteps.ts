// The steps of a chat answer (features/chat): thinking, tool calls and words, in the order they happened.
// Pure, no imports (node --test runs it as is).
//
// What the worker streams (internal/agent/loop.go): with think on, every round of a chat with tools starts with
// "<think>" (the prompt opened the thought), then the model's thought, "</think>", and what it wrote before its
// tool calls (the calls themselves are not in the text). So one turn's text can hold many <think> blocks:
//   "<think>\nA</think>\n\nLet me look.<think>\nB</think>\n\nThe answer."
// A plain chat's text may start inside the thought (no opening tag: the prompt opened it), and Qwen writes an
// empty "<think>\n\n</think>" when think is off; that is no step.

/** A piece of a text: a thought (open: it was still going when the text ended) or words. */
export type Part = { kind: "think"; text: string; open: boolean } | { kind: "text"; text: string };

const OPEN = "<think>";
const CLOSE = "</think>";

/** The length of the longest end of s that begins tag (a tag still arriving). */
function partialTag(s: string, tag: string): number {
  for (let n = Math.min(tag.length - 1, s.length); n > 0; n--) if (s.endsWith(tag.slice(0, n))) return n;
  return 0;
}

/** A text in thoughts and words. Empty closed thoughts (think off) are left out; an open one is kept (it is live). */
export function partsOf(text: string | undefined): Part[] {
  const out: Part[] = [];
  const words = (s: string) => {
    let t = s.split(CLOSE).join("");
    t = t.slice(0, t.length - partialTag(t, OPEN)).trim();
    if (t) out.push({ kind: "text", text: t });
  };
  const thought = (s: string, open: boolean) => {
    let t = s;
    if (open) t = t.slice(0, t.length - partialTag(t, CLOSE));
    t = t.split(OPEN).join("").trim();
    if (t || open) out.push({ kind: "think", text: t, open });
  };
  let rest = text ?? "";
  // the prompt opened the thought: a closing tag before any opening one ends a thought at the start
  const c0 = rest.indexOf(CLOSE);
  const o0 = rest.indexOf(OPEN);
  if (c0 >= 0 && (o0 < 0 || c0 < o0)) {
    thought(rest.slice(0, c0), false);
    rest = rest.slice(c0 + CLOSE.length);
  }
  for (;;) {
    const o = rest.indexOf(OPEN);
    if (o < 0) {
      words(rest);
      break;
    }
    words(rest.slice(0, o));
    rest = rest.slice(o + OPEN.length);
    const c = rest.indexOf(CLOSE);
    if (c < 0) {
      thought(rest, true);
      break;
    }
    thought(rest.slice(0, c), false);
    rest = rest.slice(c + CLOSE.length);
  }
  return out;
}

/** The text without its thoughts (what the answer says). */
export const wordsOf = (text: string | undefined) =>
  partsOf(text)
    .filter((p) => p.kind === "text")
    .map((p) => p.text)
    .join("\n\n");

/**
 * Where the rounds after the first begin in a turn's text with think on: each "<think>" after the opening one (a
 * round after tool calls). Nothing when the text does not start with a thought (think off: no rounds to see).
 */
export function roundStarts(text: string): number[] {
  if (!text.startsWith(OPEN)) return [];
  const out: number[] = [];
  for (let i = text.indexOf(OPEN, OPEN.length); i >= 0; i = text.indexOf(OPEN, i + OPEN.length)) out.push(i);
  return out;
}

// ---------- a turn: text pieces and tool calls → steps ----------

export type Piece<C> = { kind: "text"; key: string; text: string } | { kind: "tool"; key: string; call: C };

export type Step<C> =
  /** n: the thought's place among the turn's thoughts (0, 1, …), stable from live to ended. */
  | { kind: "think"; key: string; text: string; open: boolean; n: number }
  | { kind: "text"; key: string; text: string }
  | { kind: "tool"; key: string; call: C };

/** The steps of a turn, in order: each text piece split into its thoughts and words, the calls between them. */
export function stepsOf<C>(pieces: Piece<C>[]): Step<C>[] {
  const out: Step<C>[] = [];
  let n = 0;
  let m = 0;
  for (const p of pieces) {
    if (p.kind === "tool") {
      out.push(p);
      continue;
    }
    // keys by place among the thoughts / the words: the same live and once the turn ended (no remounting)
    partsOf(p.text).forEach((part) => {
      const key = part.kind === "think" ? `th${n}` : `tx${m++}`;
      if (part.kind === "think") out.push({ kind: "think", key, text: part.text, open: part.open, n: n++ });
      else out.push({ kind: "text", key, text: part.text });
    });
  }
  return out;
}

/** What a turn looks like, for its header and its notes. */
export function shapeOf<C>(steps: Step<C>[]) {
  const thoughts = steps.filter((s) => s.kind === "think").length;
  const tools = steps.filter((s) => s.kind === "tool").length;
  const last = steps[steps.length - 1];
  return {
    thoughts,
    tools,
    /** The words after the last call or thought: the answer proper. */
    answered: last?.kind === "text",
    /** It ended (or is) inside a thought. */
    inThought: last?.kind === "think" && last.open,
    /** Worth the "Show steps" toggle: more than one thought, or thoughts and calls. */
    multi: thoughts > 1 || (thoughts > 0 && tools > 0),
  };
}

/**
 * How long a thought took, estimated from its share of the text the model wrote: with the speed, the generation
 * time (tokens / tokens per second), else the whole time; the first thought also waited for the first token.
 */
export function thoughtSeconds(o: {
  chars: number;
  allChars: number;
  tokens?: number;
  tokensPerSecond?: number;
  total?: number;
  firstToken?: number;
  first: boolean;
}): number | undefined {
  const share = Math.min(1, (o.chars + 15) / Math.max(1, o.allChars));
  const lead = o.first ? (o.firstToken ?? 0) : 0;
  if (o.tokens && o.tokensPerSecond) return lead + (o.tokens / o.tokensPerSecond) * share;
  if (o.total == null) return undefined;
  return lead + Math.max(0, o.total - (o.firstToken ?? 0)) * share;
}

// ---------- the Think toggle ----------

/** The key a zone's Think choice is kept under ("" = a plain chat). */
export const thinkKey = (zone: string) => zone || "chat";

/**
 * Think on or off for a zone: the user's choice for it, else on for chats with tools (the steps matter there) and,
 * for a plain chat, the old one-for-all setting (off when there is none).
 */
export function thinkFor(zone: string, chosen: Record<string, boolean>, legacy?: boolean): boolean {
  const c = chosen[thinkKey(zone)];
  if (typeof c === "boolean") return c;
  return zone ? true : !!legacy;
}
