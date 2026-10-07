import { Loader2, Pause, Play } from "lucide-react";
import { create } from "zustand";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { usePlayer } from "@/store/player";

/**
 * A short listen to a voice sample (not a job: the player bar stays for what was made). One plays at a time; it
 * pauses the player bar.
 */
interface PreviewState {
  url: string;
  state: "idle" | "loading" | "playing";
  toggle: (url: string) => void;
  stop: () => void;
}

const audio = typeof Audio !== "undefined" ? new Audio() : (null as unknown as HTMLAudioElement);

export const usePreview = create<PreviewState>((set, get) => {
  if (audio) {
    audio.addEventListener("playing", () => set({ state: "playing" }));
    audio.addEventListener("pause", () => set({ state: "idle" }));
    audio.addEventListener("ended", () => set({ state: "idle" }));
    audio.addEventListener("error", () => set({ state: "idle" }));
  }
  return {
    url: "",
    state: "idle",
    toggle: (url) => {
      const s = get();
      if (s.url === url && s.state !== "idle") return s.stop();
      usePlayer.getState().pause();
      if (audio.src !== new URL(url, location.href).href) audio.src = url;
      audio.currentTime = 0;
      set({ url, state: "loading" });
      void audio.play().catch(() => set({ state: "idle" }));
    },
    stop: () => {
      audio.pause();
      set({ state: "idle" });
    },
  };
});

/** ▶ for a sample. */
export function PreviewButton({ url, className, label }: { url: string; className?: string; label?: string }) {
  const on = usePreview((s) => (s.url === url ? s.state : "idle"));
  const toggle = usePreview((s) => s.toggle);
  return (
    <Button
      type="button"
      variant="ghost"
      size={label ? "xs" : "icon-sm"}
      title={on === "idle" ? "Listen" : "Stop"}
      className={cn("text-muted-foreground", on !== "idle" && "text-primary", className)}
      onClick={(e) => (e.stopPropagation(), toggle(url))}
    >
      {on === "loading" ? <Loader2 className="animate-spin" /> : on === "playing" ? <Pause className="fill-current" /> : <Play className="fill-current" />}
      {label}
    </Button>
  );
}
