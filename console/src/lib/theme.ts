import { useSyncExternalStore } from "react";

/** Light or dark, as the system is (no manual switch, like the house's other web UIs). index.html sets it before
 * the first paint; this follows changes live: html.dark and html[data-theme] both. */
export type Theme = "light" | "dark";

const dark = matchMedia("(prefers-color-scheme: dark)");
const listeners = new Set<() => void>();

function apply() {
  document.documentElement.classList.toggle("dark", dark.matches);
  document.documentElement.dataset.theme = dark.matches ? "dark" : "light";
  listeners.forEach((l) => l());
}

export function initTheme() {
  apply();
  dark.addEventListener("change", apply);
}

export function useTheme(): Theme {
  return useSyncExternalStore(
    (l) => (listeners.add(l), () => listeners.delete(l)),
    () => (dark.matches ? "dark" : "light"),
  );
}
