// The uplink form (docs/api.md, "Uplinks"). Pure: tested with `pnpm test` (src/lib/uplinks.test.ts).
import type { Uplink, UplinkInput, UplinkState } from "../api/types.ts";

export interface UplinkForm {
  name: string;
  url: string;
  /** Blank while editing: the stored token stays. */
  token: string;
}

export const emptyForm: UplinkForm = { name: "", url: "", token: "" };

export function formOf(u: Uplink): UplinkForm {
  return { name: u.name, url: u.url, token: "" };
}

/** The address as typed, with https:// when it has no scheme (ws(s):// and http(s):// stay). */
export function normalizeUrl(s: string): string {
  const v = s.trim();
  if (!v) return "";
  return /^[a-z][a-z0-9+.-]*:\/\//i.test(v) ? v : `https://${v}`;
}

/** What is wrong with the form, field by field (the messages are English keys of i18n). */
export function validate(f: UplinkForm, editing: boolean): Partial<Record<keyof UplinkForm, string>> {
  const errs: Partial<Record<keyof UplinkForm, string>> = {};
  const url = normalizeUrl(f.url);
  if (!url) {
    errs.url = "The application's address is needed";
  } else {
    try {
      const u = new URL(url);
      if (!["http:", "https:", "ws:", "wss:"].includes(u.protocol) || !u.host) errs.url = "An http(s) or ws(s) address of the application's endpoint";
    } catch {
      errs.url = "An http(s) or ws(s) address of the application's endpoint";
    }
  }
  const token = f.token.trim();
  if (!token && !editing) errs.token = "The application's token is needed";
  else if (/\s/.test(token)) errs.token = "A token has no spaces";
  return errs;
}

export function createBody(f: UplinkForm): UplinkInput {
  const b: UplinkInput = { url: normalizeUrl(f.url), token: f.token.trim(), enabled: true };
  if (f.name.trim()) b.name = f.name.trim();
  return b;
}

/** Only what changed (a blank token keeps the stored one). */
export function patchBody(u: Uplink, f: UplinkForm): UplinkInput {
  const b: UplinkInput = {};
  if (f.name.trim() && f.name.trim() !== u.name) b.name = f.name.trim();
  const url = normalizeUrl(f.url);
  if (url !== u.url) b.url = url;
  if (f.token.trim()) b.token = f.token.trim();
  return b;
}

export function stateLabel(s: UplinkState | string, enabled = true): { text: string; tone: "ok" | "busy" | "bad" | "off" } {
  if (!enabled || s === "disabled") return { text: "disabled", tone: "off" };
  if (s === "connected") return { text: "connected", tone: "ok" };
  if (s === "connecting") return { text: "connecting", tone: "busy" };
  if (s === "error") return { text: "error", tone: "bad" };
  return { text: s || "unknown", tone: "off" };
}
