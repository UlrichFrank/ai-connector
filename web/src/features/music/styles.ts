// Trained styles (GET /api/styles): the list the Styles page and the Create panel's style picker share.
import { useEffect } from "react";
import { create } from "zustand";
import { api } from "@/api/client";
import type { CommunityStyle, Style, StyleSettings, StylesResponse } from "@/api/types";

interface Styles {
  styles: Style[];
  models: StylesResponse["models"];
  defaults: StyleSettings | null;
  /** The curated community LoRAs (Hugging Face). */
  community: CommunityStyle[];
  loaded: boolean;
  error: string;
  refresh: () => Promise<void>;
  /** Puts one style (an answer of a write) into the list. */
  put: (s: Style) => void;
  drop: (id: string) => void;
}

export const useStyles = create<Styles>((set, get) => ({
  styles: [],
  models: [],
  defaults: null,
  community: [],
  loaded: false,
  error: "",
  refresh: async () => {
    try {
      const r = await api.styles();
      set({ styles: r.styles, models: r.models, defaults: r.defaults, community: r.community ?? [], loaded: true, error: "" });
    } catch (e) {
      set({ loaded: true, error: e instanceof Error ? e.message : String(e) });
    }
  },
  put: (s) => {
    const list = get().styles;
    // the hub adds the Mac to listed styles; an answer of a write comes from that Mac without it
    const old = list.find((x) => x.id === s.id);
    const v = { ...s, worker: s.worker ?? old?.worker };
    set({ styles: old ? list.map((x) => (x.id === s.id ? v : x)) : [...list, v] });
  },
  drop: (id) => set({ styles: get().styles.filter((x) => x.id !== id) }),
}));

export const isBusy = (s: Style) => s.status === "training" || s.status === "queued";

/** Keeps the list current while mounted: every few seconds while a style trains, else now and then. */
export function useStylesLive() {
  const busy = useStyles((s) => s.styles.some(isBusy));
  const refresh = useStyles((s) => s.refresh);
  useEffect(() => {
    void refresh();
    const t = setInterval(() => void refresh(), busy ? 3000 : 20000);
    return () => clearInterval(t);
  }, [busy, refresh]);
}

/** "about 40 min", "about 2 h 10 min". */
export function about(seconds: number): string {
  if (!seconds || !isFinite(seconds)) return "";
  const m = Math.max(1, Math.round(seconds / 60));
  if (m < 60) return `about ${m} min`;
  return `about ${Math.floor(m / 60)} h${m % 60 ? ` ${m % 60} min` : ""}`;
}

/** "4:05" for track lengths, "1:02:05" past an hour. */
export function clock(seconds: number): string {
  const s = Math.round(seconds);
  const h = Math.floor(s / 3600);
  const mm = String(Math.floor((s % 3600) / 60)).padStart(h ? 2 : 1, "0");
  return `${h ? h + ":" : ""}${mm}:${String(s % 60).padStart(2, "0")}`;
}

/** "licence not stated" when the repository names none. */
export const licenceText = (l?: string) => (l?.trim() ? l.trim() : "licence not stated");

/** "88 MB". */
export const megabytes = (n?: number) => (n ? (n >= 1 << 30 ? (n / (1 << 30)).toFixed(1) + " GB" : Math.max(1, Math.round(n / (1 << 20))) + " MB") : "");

export const STATUS_LABEL: Record<Style["status"], string> = {
  new: "No tracks yet",
  ready: "Ready to train",
  queued: "Waiting for the GPU",
  training: "Training",
  paused: "Paused",
  trained: "Trained",
  failed: "Failed",
};

/** A style a song can use: trained, for this model. */
export const usableFor = (styles: Style[], model: string | undefined) => styles.filter((s) => s.weights && s.baseModel === model);
