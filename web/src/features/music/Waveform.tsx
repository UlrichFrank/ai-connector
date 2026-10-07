import { Play, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import type { Job } from "@/api/types";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { usePlayer } from "@/store/player";
import { clock, mmss } from "./songs";
import { playRange, useStudio } from "./state";

// ---------- peaks: decoded once per file, kept for the session ----------

interface Peaks {
  peaks: Float32Array;
  duration: number;
}

const N = 1600;
const cache = new Map<string, Promise<Peaks | null>>();

function loadPeaks(file: string): Promise<Peaks | null> {
  let p = cache.get(file);
  if (!p) {
    p = (async () => {
      try {
        const buf = await (await fetch(file, { credentials: "same-origin" })).arrayBuffer();
        const ctx = new AudioContext();
        const audio = await ctx.decodeAudioData(buf);
        void ctx.close();
        const chans = Array.from({ length: audio.numberOfChannels }, (_, i) => audio.getChannelData(i));
        const len = chans[0].length;
        const step = Math.max(1, Math.floor(len / N));
        const peaks = new Float32Array(N);
        let max = 0;
        for (let i = 0; i < N; i++) {
          let m = 0;
          const end = Math.min(len, (i + 1) * step);
          for (const ch of chans) for (let k = i * step; k < end; k += 4) m = Math.max(m, Math.abs(ch[k]));
          peaks[i] = m;
          max = Math.max(max, m);
        }
        // normalised, a little compressed so quiet parts stay visible
        if (max > 0) for (let i = 0; i < N; i++) peaks[i] = Math.pow(peaks[i] / max, 0.8);
        return { peaks, duration: audio.duration };
      } catch {
        cache.delete(file); // try again next time
        return null;
      }
    })();
    cache.set(file, p);
  }
  return p;
}

function usePeaks(file: string) {
  const [state, setState] = useState<{ file: string; peaks: Peaks | null; done: boolean }>({ file, peaks: null, done: false });
  useEffect(() => {
    let live = true;
    loadPeaks(file).then((peaks) => live && setState({ file, peaks, done: true }));
    return () => {
      live = false;
    };
  }, [file]);
  return state.file === file ? state : { file, peaks: null, done: false };
}

function draw(canvas: HTMLCanvasElement, peaks: Float32Array | null) {
  const r = canvas.getBoundingClientRect();
  const dpr = window.devicePixelRatio || 1;
  canvas.width = Math.max(1, Math.round(r.width * dpr));
  canvas.height = Math.max(1, Math.round(r.height * dpr));
  const g = canvas.getContext("2d");
  if (!g) return;
  g.clearRect(0, 0, canvas.width, canvas.height);
  g.fillStyle = getComputedStyle(canvas).color;
  const bar = 2 * dpr;
  const gap = 1 * dpr;
  const n = Math.floor(canvas.width / (bar + gap));
  const mid = canvas.height / 2;
  for (let i = 0; i < n; i++) {
    let v = 0.04;
    if (peaks?.length) {
      const a = Math.floor((i / n) * peaks.length);
      const b = Math.max(a + 1, Math.floor(((i + 1) / n) * peaks.length));
      for (let k = a; k < b; k++) v = Math.max(v, peaks[k]);
    }
    const h = Math.max(dpr, v * mid * 0.92);
    g.beginPath();
    g.roundRect(i * (bar + gap), mid - h, bar, h * 2, bar / 2);
    g.fill();
  }
}

/** One layer of bars in the colour of its CSS class. */
function Bars({ peaks, className }: { peaks: Float32Array | null; className: string }) {
  const ref = useRef<HTMLCanvasElement>(null);
  useEffect(() => {
    const c = ref.current;
    if (!c) return;
    draw(c, peaks);
    const ro = new ResizeObserver(() => draw(c, peaks));
    ro.observe(c);
    // the theme can change (dark/light follow the system)
    const mq = matchMedia("(prefers-color-scheme: dark)");
    const redraw = () => requestAnimationFrame(() => draw(c, peaks));
    mq.addEventListener("change", redraw);
    return () => (ro.disconnect(), mq.removeEventListener("change", redraw));
  }, [peaks]);
  return <canvas ref={ref} className={cn("absolute inset-0 size-full", className)} />;
}

// ---------- the waveform ----------

/**
 * A song's waveform: the played part in colour with a playhead; click to play from there, drag to select a part (for
 * Replace section and Add instrument).
 */
export function Waveform({ job: j, file }: { job: Job; file: string }) {
  const { peaks, done } = usePeaks(file);
  const len = peaks?.duration || Number(j.stats?.duration) || j.request.duration || 0;
  const sel = useStudio((s) => (s.sel?.song === j.id ? s.sel : null));
  const setSel = useStudio((s) => s.setSel);
  const box = useRef<HTMLDivElement>(null);
  const drag = useRef<{ x: number; at: number; moved: boolean } | null>(null);
  const [hover, setHover] = useState<number | null>(null);
  const [live, setLive] = useState<{ start: number; end: number } | null>(null);

  const frac = (clientX: number) => {
    const r = box.current!.getBoundingClientRect();
    return Math.min(1, Math.max(0, (clientX - r.left) / r.width));
  };
  const shown = live ?? sel;

  return (
    <div className="flex flex-col gap-2">
      <div
        ref={box}
        className="relative h-28 cursor-text touch-none rounded-xl bg-muted/40 select-none max-sm:h-20"
        onPointerDown={(e) => {
          if (e.button !== 0 || !len) return;
          e.currentTarget.setPointerCapture(e.pointerId);
          drag.current = { x: e.clientX, at: frac(e.clientX), moved: false };
        }}
        onPointerMove={(e) => {
          setHover(frac(e.clientX));
          const d = drag.current;
          if (!d) return;
          if (!d.moved && Math.abs(e.clientX - d.x) < 4) return;
          d.moved = true;
          const f = frac(e.clientX);
          setLive({ start: Math.min(d.at, f) * len, end: Math.max(d.at, f) * len });
        }}
        onPointerUp={(e) => {
          const d = drag.current;
          drag.current = null;
          if (!d) return;
          if (d.moved && live && live.end - live.start > 0.2) setSel({ song: j.id, start: +live.start.toFixed(1), end: +live.end.toFixed(1) });
          else if (!d.moved) {
            const t = frac(e.clientX) * len;
            const p = usePlayer.getState();
            if (p.file === file) {
              p.seek(t);
              if (p.paused) p.play(j, file);
            } else p.play(j, file, t);
          }
          setLive(null);
        }}
        onPointerCancel={() => ((drag.current = null), setLive(null))}
        onPointerLeave={() => setHover(null)}
      >
        <div className="absolute inset-x-3 inset-y-2">
          <Bars peaks={peaks?.peaks ?? null} className="text-muted-foreground/45" />
          <Played file={file} len={len}>
            <Bars peaks={peaks?.peaks ?? null} className="text-primary" />
          </Played>
          {shown && (
            <div
              className="pointer-events-none absolute inset-y-[-4px] rounded-md border-x-2 border-primary bg-primary/15"
              style={{ left: `${(shown.start / len) * 100}%`, width: `${((shown.end - shown.start) / len) * 100}%` }}
            />
          )}
          <Playhead file={file} len={len} />
          {hover != null && len > 0 && (
            <div className="pointer-events-none absolute inset-y-0 w-px bg-foreground/40" style={{ left: `${hover * 100}%` }}>
              <span className="absolute -top-1 left-1 rounded bg-popover px-1 py-px text-[10px] text-popover-foreground shadow tabular-nums">{mmss(hover * len)}</span>
            </div>
          )}
        </div>
        {!done && <div className="absolute inset-0 grid place-items-center text-xs text-muted-foreground">reading the waveform …</div>}
      </div>
      <Ruler len={len} />
      <div className="flex flex-wrap items-center gap-2">
        <Time file={file} len={len} />
        <div className="flex-1" />
        {sel ? (
          <div className="flex items-center gap-1.5 text-xs">
            <span className="rounded-md bg-primary/15 px-2 py-1 text-primary tabular-nums">
              {clock(sel.start)} – {clock(sel.end)} <span className="opacity-70">· {(sel.end - sel.start).toFixed(1)} s</span>
            </span>
            <Button size="sm" variant="outline" onClick={() => playRange(j, file, sel.start, sel.end)}>
              <Play className="fill-current" /> Play part
            </Button>
            <Button size="icon-sm" variant="ghost" title="Clear the selection" onClick={() => setSel(null)}>
              <X />
            </Button>
          </div>
        ) : (
          <span className="text-xs text-muted-foreground max-sm:hidden">Click to play from there · drag to select a part</span>
        )}
      </div>
    </div>
  );
}

function usePos(file: string) {
  return usePlayer((s) => (s.file === file ? s.time : -1));
}

function Played({ file, len, children }: { file: string; len: number; children: React.ReactNode }) {
  const t = usePos(file);
  const w = t >= 0 && len ? Math.min(100, (t / len) * 100) : 0;
  return (
    <div className="absolute inset-0" style={{ clipPath: `inset(0 ${100 - w}% 0 0)` }}>
      {children}
    </div>
  );
}

function Playhead({ file, len }: { file: string; len: number }) {
  const t = usePos(file);
  if (t < 0 || !len) return null;
  return <div className="pointer-events-none absolute -inset-y-1 w-0.5 rounded-full bg-primary shadow-[0_0_8px] shadow-primary" style={{ left: `${Math.min(100, (t / len) * 100)}%` }} />;
}

function Time({ file, len }: { file: string; len: number }) {
  const t = usePos(file);
  return (
    <span className="text-xs text-muted-foreground tabular-nums">
      <span className={cn(t >= 0 && "text-foreground")}>{mmss(Math.max(0, t))}</span> / {mmss(len)}
    </span>
  );
}

function Ruler({ len }: { len: number }) {
  if (!len) return null;
  const step = [5, 10, 15, 30, 60, 120].find((s) => len / s <= 8) ?? 300;
  const ticks: number[] = [];
  for (let t = 0; t <= len - step * 0.4; t += step) ticks.push(t);
  return (
    <div className="relative mx-3 h-3 text-[10px] text-muted-foreground tabular-nums">
      {ticks.map((t) => (
        <span key={t} className="absolute -translate-x-1/2 first:translate-x-0" style={{ left: `${(t / len) * 100}%` }}>
          {mmss(t)}
        </span>
      ))}
      <span className="absolute right-0">{mmss(len)}</span>
    </div>
  );
}
