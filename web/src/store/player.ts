import { create } from "zustand";
import type { Job } from "@/api/types";

/**
 * The one audio player of the app: it keeps playing while you move between pages (the bar at the bottom).
 * Pages call play(job) / toggle(job); `playing` is the job whose file is loaded.
 */
interface PlayerState {
  job: Job | null;
  file: string;
  paused: boolean;
  time: number;
  duration: number;
  play: (job: Job, file?: string, at?: number) => void;
  toggle: (job: Job, file?: string) => void;
  pause: () => void;
  seek: (t: number) => void;
  stop: () => void;
}

const audio = typeof Audio !== "undefined" ? new Audio() : (null as unknown as HTMLAudioElement);

export const usePlayer = create<PlayerState>((set, get) => {
  if (audio) {
    audio.preload = "auto";
    audio.addEventListener("timeupdate", () => set({ time: audio.currentTime }));
    audio.addEventListener("durationchange", () => set({ duration: isFinite(audio.duration) ? audio.duration : 0 }));
    audio.addEventListener("play", () => set({ paused: false }));
    audio.addEventListener("pause", () => set({ paused: true }));
    audio.addEventListener("ended", () => set({ paused: true }));
  }
  return {
    job: null,
    file: "",
    paused: true,
    time: 0,
    duration: 0,
    play: (job, file = job.files?.[0] ?? "", at) => {
      if (!file) return;
      if (get().file !== file) {
        audio.src = file;
        set({ job, file, time: 0, duration: 0 });
      } else set({ job });
      if (at != null) audio.currentTime = at;
      void audio.play();
    },
    toggle: (job, file = job.files?.[0] ?? "") => {
      const s = get();
      if (s.file === file && !s.paused) audio.pause();
      else s.play(job, file);
    },
    pause: () => audio.pause(),
    seek: (t) => {
      audio.currentTime = t;
      set({ time: t });
    },
    stop: () => {
      audio.pause();
      audio.removeAttribute("src");
      set({ job: null, file: "", paused: true, time: 0, duration: 0 });
    },
  };
});

/** Is this file the one playing right now? */
export const usePlaying = (file?: string) => usePlayer((s) => !!file && s.file === file && !s.paused);
