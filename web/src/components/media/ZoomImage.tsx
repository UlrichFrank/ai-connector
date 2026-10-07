import { useCallback, useEffect, useRef, useState } from "react";
import { cn } from "@/lib/utils";

interface Z {
  s: number;
  x: number;
  y: number;
}

const one: Z = { s: 1, x: 0, y: 0 };
const maxScale = 8;

/**
 * A picture that zooms (wheel, trackpad pinch, two fingers, double-click) and pans (drag) when zoomed. Unzoomed, a
 * horizontal swipe calls `onSwipe(-1 | 1)`. `onZoom` hears the scale (for a "100 %" button outside).
 */
export function ZoomImage({
  src,
  alt,
  onSwipe,
  onZoom,
  resetKey,
  className,
}: {
  src: string;
  alt: string;
  onSwipe?: (dir: -1 | 1) => void;
  onZoom?: (s: number) => void;
  resetKey?: number;
  /** For the picture: its largest size unzoomed (room for the controls around it). */
  className?: string;
}) {
  const stage = useRef<HTMLDivElement>(null);
  const img = useRef<HTMLImageElement>(null);
  const [z, setZState] = useState<Z>(one);
  const zr = useRef<Z>(one);
  const [dragging, setDragging] = useState(false);
  const pointers = useRef(new Map<number, { x: number; y: number }>());
  const swipe = useRef<{ x: number; y: number } | null>(null);
  const pinch = useRef<number | null>(null);

  const setZ = useCallback(
    (n: Z) => {
      // keep the picture on the stage: it may not be pulled further than its edges
      const st = stage.current?.getBoundingClientRect();
      const el = img.current;
      if (n.s <= 1.001) n = one;
      else if (st && el) {
        const mx = Math.max(0, (el.offsetWidth * n.s - st.width) / 2);
        const my = Math.max(0, (el.offsetHeight * n.s - st.height) / 2);
        n = { s: n.s, x: Math.max(-mx, Math.min(mx, n.x)), y: Math.max(-my, Math.min(my, n.y)) };
      }
      zr.current = n;
      setZState(n);
      onZoom?.(n.s);
    },
    [onZoom],
  );

  /** Zooms to `s` keeping the point under (cx, cy) where it is. */
  const zoomAt = useCallback(
    (cx: number, cy: number, s: number) => {
      const st = stage.current?.getBoundingClientRect();
      if (!st) return;
      const cur = zr.current;
      s = Math.max(1, Math.min(maxScale, s));
      const qx = cx - (st.left + st.width / 2);
      const qy = cy - (st.top + st.height / 2);
      const k = s / cur.s;
      setZ({ s, x: qx - k * (qx - cur.x), y: qy - k * (qy - cur.y) });
    },
    [setZ],
  );

  useEffect(() => setZ(one), [src, resetKey, setZ]);

  // wheel: React's listener is passive, so the page would scroll instead
  useEffect(() => {
    const el = stage.current;
    if (!el) return;
    const wheel = (e: WheelEvent) => {
      e.preventDefault();
      zoomAt(e.clientX, e.clientY, zr.current.s * Math.exp(-e.deltaY * (e.ctrlKey ? 0.01 : 0.0025)));
    };
    el.addEventListener("wheel", wheel, { passive: false });
    return () => el.removeEventListener("wheel", wheel);
  }, [zoomAt]);

  const dist = () => {
    const [a, b] = [...pointers.current.values()];
    return Math.hypot(a.x - b.x, a.y - b.y);
  };
  const mid = () => {
    const [a, b] = [...pointers.current.values()];
    return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
  };

  return (
    <div
      ref={stage}
      className={cn("relative flex size-full touch-none items-center justify-center overflow-hidden select-none", z.s > 1 ? (dragging ? "cursor-grabbing" : "cursor-grab") : "cursor-zoom-in")}
      onDoubleClick={(e) => (zr.current.s > 1 ? setZ(one) : zoomAt(e.clientX, e.clientY, 2.5))}
      onPointerDown={(e) => {
        e.currentTarget.setPointerCapture(e.pointerId);
        pointers.current.set(e.pointerId, { x: e.clientX, y: e.clientY });
        if (pointers.current.size === 2) {
          pinch.current = dist();
          swipe.current = null;
        } else {
          swipe.current = { x: e.clientX, y: e.clientY };
          setDragging(true);
        }
      }}
      onPointerMove={(e) => {
        const p = pointers.current.get(e.pointerId);
        if (!p) return;
        const dx = e.clientX - p.x;
        const dy = e.clientY - p.y;
        pointers.current.set(e.pointerId, { x: e.clientX, y: e.clientY });
        if (pointers.current.size === 2 && pinch.current) {
          const d = dist();
          const m = mid();
          zoomAt(m.x, m.y, (zr.current.s * d) / pinch.current);
          pinch.current = d;
        } else if (zr.current.s > 1) {
          setZ({ ...zr.current, x: zr.current.x + dx, y: zr.current.y + dy });
        }
      }}
      onPointerUp={(e) => {
        pointers.current.delete(e.pointerId);
        if (pointers.current.size < 2) pinch.current = null;
        setDragging(false);
        const s0 = swipe.current;
        swipe.current = null;
        if (s0 && zr.current.s <= 1 && onSwipe) {
          const dx = e.clientX - s0.x;
          const dy = e.clientY - s0.y;
          if (Math.abs(dx) > 60 && Math.abs(dy) < 80) onSwipe(dx < 0 ? 1 : -1);
        }
      }}
      onPointerCancel={(e) => {
        pointers.current.delete(e.pointerId);
        pinch.current = null;
        swipe.current = null;
        setDragging(false);
      }}
    >
      <img
        ref={img}
        src={src}
        alt={alt}
        draggable={false}
        className={cn("max-h-full max-w-full object-contain will-change-transform", className, !dragging && "transition-transform duration-150 ease-out")}
        style={{ transform: `translate(${z.x}px, ${z.y}px) scale(${z.s})` }}
      />
    </div>
  );
}
