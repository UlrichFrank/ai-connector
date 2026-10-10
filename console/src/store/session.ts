import { create } from "zustand";
import { api, onUnauthorized } from "@/api/client";
import type { Info } from "@/api/types";
import { useLive } from "./live";

/** loading /api/info; signin without a session; ready once signed in; unreachable when /api/info does not answer. */
export type Phase = "loading" | "signin" | "ready" | "unreachable";

interface SessionState {
  phase: Phase;
  info: Info | null;
  /** Why we are on the sign-in page again. */
  note: string;
  boot: () => Promise<void>;
  /** Throws the ApiError of a wrong password or too many attempts. */
  signIn: (password: string, device: string, user: string) => Promise<void>;
  signOut: () => Promise<void>;
  /** The server no longer knows us (a 401, or the device was revoked elsewhere). */
  expire: () => void;
}

let booting: Promise<void> | null = null;
let retry: ReturnType<typeof setTimeout> | undefined;

/** `mlxlab serve` (a worker alone) has no sign-in: it counts as signed in. */
const signedIn = (i: Info) => i.role !== "hub" || !!i.signedIn;

export const useSession = create<SessionState>((set, get) => {
  const ready = (info: Info) => {
    set({ phase: "ready", info, note: "" });
    useLive.getState().start(info);
  };
  return {
    phase: "loading",
    info: null,
    note: "",

    boot: () =>
      (booting ??= (async () => {
        clearTimeout(retry);
        try {
          const info = await api.info();
          if (signedIn(info)) ready(info);
          else set({ phase: "signin", info });
        } catch {
          set({ phase: "unreachable" });
          retry = setTimeout(() => get().boot(), 5000);
        } finally {
          booting = null;
        }
      })()),

    signIn: async (password, device, user) => {
      await api.login(password, device, user);
      const info = await api.info();
      if (!signedIn(info)) throw new Error("Signed in, but this browser did not keep the session. Is the server served over HTTPS?");
      ready(info);
    },

    signOut: async () => {
      await api.logout().catch(() => undefined);
      useLive.getState().stop();
      set({ phase: "signin", note: "" });
    },

    expire: () => {
      if (get().phase !== "ready") return;
      useLive.getState().stop();
      set({ phase: "signin", note: "You were signed out. Sign in again to continue." });
    },
  };
});

onUnauthorized(() => useSession.getState().expire());
