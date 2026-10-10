import { useSyncExternalStore } from "react";
import { local } from "./storage";

/** The colour scheme: as the system is (default), or chosen. Kept per browser. */
export type Theme = "system" | "light" | "dark";

const dark = matchMedia("(prefers-color-scheme: dark)");
let theme: Theme = local.get<Theme>("console.theme", "system");
const listeners = new Set<() => void>();

function apply() {
  document.documentElement.classList.toggle("dark", theme === "dark" || (theme === "system" && dark.matches));
}

export function initTheme() {
  apply();
  dark.addEventListener("change", apply);
}

export function setTheme(t: Theme) {
  theme = t;
  local.set("console.theme", t);
  apply();
  listeners.forEach((l) => l());
}

export function useTheme(): Theme {
  return useSyncExternalStore(
    (l) => (listeners.add(l), () => listeners.delete(l)),
    () => theme,
  );
}
