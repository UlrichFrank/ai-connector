// The UI language: German by default (the house language), English one tap away (DE/EN in the header), kept per
// browser under "home.lang" like the house's other web UIs. Strings are written in English in the code and looked up
// in the German dictionary (i18n-de.ts); what has no entry stays English. Data from elsewhere (worker, model and
// client names, server messages) is never translated.
//
// Not a React context: lib code (format.ts, specs.ts, …) and the stores use t() too. Outside a browser (the unit
// tests under node) the language is English, so the tests keep checking the English texts.
import { useSyncExternalStore } from "react";
import { de } from "./i18n-de.ts";

export type Lang = "de" | "en";
export const LANGS: Lang[] = ["de", "en"];

const KEY = "home.lang";

function initial(): Lang {
  if (typeof window === "undefined") return "en";
  try {
    const v = localStorage.getItem(KEY);
    if (v === "de" || v === "en") return v;
  } catch {
    // storage blocked: the default
  }
  return "de";
}

let lang: Lang = initial();
const listeners = new Set<() => void>();
if (typeof document !== "undefined") document.documentElement.lang = lang;

export function getLang(): Lang {
  return lang;
}

export function setLang(l: Lang) {
  if (l === lang) return;
  lang = l;
  try {
    localStorage.setItem(KEY, l);
  } catch {
    // a convenience only
  }
  document.documentElement.lang = l;
  listeners.forEach((f) => f());
}

/** The current language; the component renders again when it changes. */
export function useLang(): Lang {
  return useSyncExternalStore(
    (f) => (listeners.add(f), () => listeners.delete(f)),
    () => lang,
    () => lang,
  );
}

/** Intl's locale for the language: German with a decimal comma, British English (24 h, day first). */
export function locale(): string {
  return lang === "de" ? "de-DE" : "en-GB";
}

/** A UI string, written in English, in the current language; {name} placeholders filled from vars. */
export function t(text: string, vars?: Record<string, string | number>): string {
  const s = (lang === "de" && de[text]) || text;
  return vars ? s.replace(/\{(\w+)\}/g, (m: string, k: string) => (k in vars ? String(vars[k]) : m)) : s;
}

/** One of two forms by count: plural(n, "{n} worker", "{n} workers"). */
export function plural(n: number, one: string, many: string): string {
  return t(n === 1 ? one : many, { n });
}

/** A number in the current locale (1,5 in German). */
export function num(n: number, decimals?: number): string {
  return new Intl.NumberFormat(locale(), decimals === undefined ? undefined : { minimumFractionDigits: decimals, maximumFractionDigits: decimals }).format(n);
}

/** A date and time in the current locale, 24 h. */
export function dateTime(d: Date | string | number): string {
  return new Date(d).toLocaleString(locale(), { dateStyle: "medium", timeStyle: "short", hour12: false });
}
