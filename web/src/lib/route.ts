import { useSyncExternalStore } from "react";

/** The page and its argument from the URL hash: "#/music/<song id>" → { page: "music", arg: "<song id>" }. */
export interface Route {
  page: string;
  arg: string;
}

function read(): Route {
  const [page = "", ...rest] = location.hash.replace(/^#\/?/, "").split("/");
  return { page: decodeURIComponent(page) || "image", arg: decodeURIComponent(rest.join("/")) };
}

let current = read();
const listeners = new Set<() => void>();
window.addEventListener("hashchange", () => {
  current = read();
  listeners.forEach((l) => l());
});

export function useRoute(): Route {
  return useSyncExternalStore(
    (l) => (listeners.add(l), () => listeners.delete(l)),
    () => current,
  );
}

export function navigate(page: string, arg = "") {
  location.hash = "/" + page + (arg ? "/" + encodeURIComponent(arg) : "");
}
