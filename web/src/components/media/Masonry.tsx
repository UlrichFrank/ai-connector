import { useLayoutEffect, useRef, useState, type ReactNode } from "react";

/**
 * Tiles of their own shape in columns, in order (each goes into the shortest column). Every tile is placed
 * absolutely in one container, so a new one at the front moves the others without remounting them.
 */
export function Masonry<T>({
  items,
  keyOf,
  aspectOf,
  minHeightOf,
  render,
  column = 260,
  phoneColumn = 150,
  gap = 12,
}: {
  items: T[];
  keyOf: (t: T) => string;
  /** width / height */
  aspectOf: (t: T) => number;
  /** A tile that needs room whatever its shape (a failed job's message). */
  minHeightOf?: (t: T) => number;
  render: (t: T) => ReactNode;
  /** The narrowest a column may be (on a phone: `phoneColumn`). */
  column?: number;
  phoneColumn?: number;
  gap?: number;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    setWidth(el.clientWidth);
    const ro = new ResizeObserver(() => setWidth(el.clientWidth));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const min = width < 640 ? Math.min(column, phoneColumn) : column;
  const cols = Math.max(1, Math.floor((width + gap) / (min + gap)));
  const w = cols > 0 && width ? (width - gap * (cols - 1)) / cols : 0;
  const heights = new Array<number>(cols).fill(0);
  const placed = items.map((t) => {
    const c = heights.indexOf(Math.min(...heights));
    const h = Math.max(w / Math.min(3, Math.max(0.33, aspectOf(t) || 1)), minHeightOf?.(t) ?? 0);
    const pos = { x: c * (w + gap), y: heights[c], h };
    heights[c] += h + gap;
    return { t, ...pos };
  });

  return (
    <div ref={ref} className="relative w-full" style={{ height: Math.max(0, Math.max(...heights) - gap) }}>
      {w > 0 &&
        placed.map(({ t, x, y, h }) => (
          <div
            key={keyOf(t)}
            className="absolute top-0 left-0 transition-transform duration-300 ease-out"
            style={{ width: w, height: h, transform: `translate(${x}px, ${y}px)` }}
          >
            {render(t)}
          </div>
        ))}
    </div>
  );
}
