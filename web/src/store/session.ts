import { create } from "zustand";
import { api, onUnauthorized } from "@/api/client";
import type { Info } from "@/api/types";
import { useApp } from "./app";

/**
 * Where the app is before it can show its pages:
 * loading /api/info; signin on a hub without a session; ready once signed in (or against `mlxlab serve`);
 * unreachable when /api/info does not answer at all.
 */
export type Phase = "loading" | "signin" | "ready" | "unreachable";

interface SessionState {
  phase: Phase;
  info: Info | null;
  /** Why we are on the sign-in page again ("You were signed out …"). */
  note: string;

  boot: () => Promise<void>;
  /** Throws the ApiError of a wrong password or too many attempts. */
  signIn: (password: string, device: string, user: string) => Promise<void>;
  signOut: () => Promise<void>;
  /** The server no longer knows us (a 401, or the device was revoked elsewhere). */
  expire: () => void;
  /** Reads /api/info again (who this device is, whether it may invite: an owner may have changed that). */
  refresh: () => Promise<void>;
  /** This device's user changed (an owner renamed it on the Devices page). */
  setUser: (user: string) => void;
}

let booting: Promise<void> | null = null;
let retry: ReturnType<typeof setTimeout> | undefined;

export const useSession = create<SessionState>((set, get) => {
  const ready = (info: Info) => {
    set({ phase: "ready", info, note: "" });
    useApp.getState().start(info);
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
          if (info.role === "hub" && !info.signedIn) set({ phase: "signin", info });
          else ready(info);
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
      if (!info.signedIn)
        throw new Error("Signed in, but this browser did not keep the session. Is the hub served over HTTPS?");
      ready(info);
    },

    signOut: async () => {
      await api.logout().catch(() => undefined);
      useApp.getState().stop();
      set({ phase: "signin", note: "" });
    },

    expire: () => {
      if (get().phase !== "ready") return;
      useApp.getState().stop();
      set({ phase: "signin", note: "You were signed out. Sign in again to continue." });
    },

    refresh: async () => {
      const info = await api.info().catch(() => null);
      if (info?.signedIn && get().phase === "ready") set({ info });
    },

    setUser: (user) => {
      const info = get().info;
      if (info) set({ info: { ...info, user } });
    },
  };
});

onUnauthorized(() => useSession.getState().expire());
