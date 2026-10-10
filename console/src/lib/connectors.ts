// The connector form (docs/api.md, "Connectors"). Pure: tested with `pnpm test` (src/lib/connectors.test.ts).
import type { Connector, ConnectorAgent, ConnectorInput, ConnectorState } from "../api/types.ts";

/** An agent of the form: the model, a name ("" the model's), its id (kept from the server, else derived). */
export interface AgentRow {
  id: string;
  model: string;
  name: string;
}

export interface ConnectorForm {
  name: string;
  url: string;
  /** Blank while editing: the stored token stays. */
  token: string;
  board: string;
  classify: string;
  embed: string;
  chat: string;
  /** The agents vboard's chat offers, in order ([] = the chat model alone). */
  agents: AgentRow[];
}

export const emptyForm: ConnectorForm = { name: "", url: "", token: "", board: "", classify: "", embed: "", chat: "", agents: [] };

export function formOf(c: Connector): ConnectorForm {
  return {
    name: c.name,
    url: c.url,
    token: "",
    board: c.board ?? "",
    classify: c.models?.classify ?? "",
    embed: c.models?.embed ?? "",
    chat: c.models?.chat ?? "",
    agents: (c.agents ?? []).map((a) => ({ id: a.id, model: a.model, name: a.name ?? "" })),
  };
}

// ---------- agents ----------

/** At most this many (vboard keeps 20). */
export const maxAgents = 20;

/** A Claude Code model is an agent (it works on the board itself, through vboard's MCP); anything else a model. */
export function agentKind(model: string): "model" | "agent" {
  return model.trim().toLowerCase().startsWith("claude-code/") ? "agent" : "model";
}

/** The id the server derives from a model id (claude-code/haiku → claude-code-haiku). */
export function agentId(model: string): string {
  return model
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9._-]/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 80);
}

/** The list with the model added at the end (an id not used yet; nothing when the model is there already). */
export function addAgent(list: AgentRow[], model: string): AgentRow[] {
  if (!model || list.some((a) => a.model === model) || list.length >= maxAgents) return list;
  const base = agentId(model) || "agent";
  let id = base;
  for (let n = 2; list.some((a) => a.id === id); n++) id = `${base}-${n}`;
  return [...list, { id, model, name: "" }];
}

/** The list with row i moved by d (-1 up, +1 down). */
export function moveAgent(list: AgentRow[], i: number, d: number): AgentRow[] {
  const j = i + d;
  if (i < 0 || i >= list.length || j < 0 || j >= list.length) return list;
  const out = [...list];
  [out[i], out[j]] = [out[j], out[i]];
  return out;
}

export function removeAgent(list: AgentRow[], i: number): AgentRow[] {
  return list.filter((_, k) => k !== i);
}

/** What is wrong with the list ("" fine), as the server checks it. */
export function agentsProblem(list: AgentRow[]): string {
  if (list.length > maxAgents) return `At most ${maxAgents} agents`;
  if (list.length && agentKind(list[0].model) !== "model")
    return "The first is the default, which older vboards and search ask with vboard's tools: it must be a chat model, not Claude Code";
  const ids = new Set<string>();
  for (const a of list) {
    if (!a.model.trim()) return "Every agent needs a model";
    const id = a.id.trim() || agentId(a.model);
    if (ids.has(id)) return `The id “${id}” is there twice`;
    ids.add(id);
  }
  return "";
}

const agentsBody = (list: AgentRow[]): ConnectorAgent[] => list.map((a) => ({ id: a.id.trim() || agentId(a.model), model: a.model.trim(), name: a.name.trim() }));

/** "vboard.example.com/" → "https://vboard.example.com"; keeps an explicit http:// (a LAN vboard). */
export function normalizeUrl(u: string): string {
  let s = u.trim();
  if (!s) return "";
  if (!/^[a-z][a-z0-9+.-]*:\/\//i.test(s)) s = "https://" + s;
  return s.replace(/\/+$/, "");
}

/** The name the server gives a connector without one (also what vboard shows as the local AI's name). */
export const defaultName = "ai-connector";

/**
 * What is wrong with the form, by field ({} when it can be saved). A new connector needs a token; the name may be blank
 * (the server's default). vboard's token prefix is configurable (vbp_ by default) and board tokens exist too, so any
 * token without spaces is taken.
 */
export function validate(f: ConnectorForm, editing: boolean): Partial<Record<keyof ConnectorForm, string>> {
  const e: Partial<Record<keyof ConnectorForm, string>> = {};
  const url = normalizeUrl(f.url);
  if (!url) e.url = "The vboard address is needed";
  else {
    try {
      const p = new URL(url);
      if (p.protocol !== "https:" && p.protocol !== "http:") e.url = "An http(s) address, like https://vboard.example.com";
    } catch {
      e.url = "Not an address, like https://vboard.example.com";
    }
  }
  const t = f.token.trim();
  if (!t && !editing) e.token = "A vboard token (vbp_…) is needed";
  else if (/\s/.test(t)) e.token = "A token has no spaces";
  const a = agentsProblem(f.agents);
  if (a) e.agents = a;
  return e;
}

const models = (f: ConnectorForm) => {
  const m: { classify?: string; embed?: string; chat?: string } = {};
  if (f.classify) m.classify = f.classify;
  if (f.embed) m.embed = f.embed;
  if (f.chat) m.chat = f.chat;
  return m;
};

/** POST /api/connectors (a blank name: the server's default). */
export function createBody(f: ConnectorForm): ConnectorInput {
  const body: ConnectorInput = { kind: "vboard", url: normalizeUrl(f.url), token: f.token.trim() };
  if (f.name.trim()) body.name = f.name.trim();
  if (f.board.trim()) body.board = f.board.trim();
  const m = models(f);
  if (Object.keys(m).length) body.models = m;
  if (f.agents.length) body.agents = agentsBody(f.agents);
  return body;
}

/**
 * PATCH /api/connectors/{id}: only what changed (a blank token keeps the stored one; "" clears board or a model; a blank
 * name is the server's default).
 */
export function patchBody(c: Connector, f: ConnectorForm): ConnectorInput {
  const p: ConnectorInput = {};
  if ((f.name.trim() || defaultName) !== c.name) p.name = f.name.trim();
  if (normalizeUrl(f.url) !== c.url) p.url = normalizeUrl(f.url);
  if (f.token.trim()) p.token = f.token.trim();
  if (f.board.trim() !== (c.board ?? "")) p.board = f.board.trim();
  const before = c.models ?? {};
  if ((before.classify ?? "") !== f.classify || (before.embed ?? "") !== f.embed || (before.chat ?? "") !== f.chat)
    p.models = { classify: f.classify, embed: f.embed, chat: f.chat };
  const agents = agentsBody(f.agents);
  const was = (c.agents ?? []).map((a) => ({ id: a.id, model: a.model, name: a.name ?? "" }));
  if (JSON.stringify(agents) !== JSON.stringify(was)) p.agents = agents; // [] = back to the default
  return p;
}

/** The state as a word and a tone for its dot. */
export function stateLabel(s: ConnectorState | string, enabled = true): { text: string; tone: "ok" | "busy" | "bad" | "off" } {
  if (!enabled || s === "disabled") return { text: "disabled", tone: "off" };
  if (s === "connected") return { text: "connected", tone: "ok" };
  if (s === "connecting") return { text: "connecting", tone: "busy" };
  if (s === "error") return { text: "error", tone: "bad" };
  return { text: s || "unknown", tone: "off" };
}
