// The connector form (docs/api.md, "Connectors"). Pure: tested with `pnpm test` (src/lib/connectors.test.ts).
import type { Connector, ConnectorInput, ConnectorState } from "../api/types.ts";

export interface ConnectorForm {
  name: string;
  url: string;
  /** Blank while editing: the stored token stays. */
  token: string;
  board: string;
  classify: string;
  embed: string;
  chat: string;
}

export const emptyForm: ConnectorForm = { name: "", url: "", token: "", board: "", classify: "", embed: "", chat: "" };

export function formOf(c: Connector): ConnectorForm {
  return { name: c.name, url: c.url, token: "", board: c.board ?? "", classify: c.models?.classify ?? "", embed: c.models?.embed ?? "", chat: c.models?.chat ?? "" };
}

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
