import { useMemo, useState } from "react";
import { cn } from "@/lib/utils";

/**
 * How strongly a cosine similarity is drawn: 0.1 (or less) faint, 1 full violet; one hue. Unrelated texts rarely
 * score under 0.2 with these models, so the low end is cut and the curve bends a little to set close pairs apart.
 */
const strength = (v: number) => Math.max(0, Math.min(1, (v - 0.1) / 0.9)) ** 1.2;
const mix = (v: number) => Math.round(8 + strength(v) * 92);
export const cellColor = (v: number) => `color-mix(in oklab, var(--primary) ${mix(v)}%, var(--card))`;
/** Light text on the strong cells, so the values stay readable. */
const inkFor = (v: number) => (mix(v) > 58 ? "text-primary-foreground" : "text-foreground");

export const fmt = (v: number) => v.toFixed(2);

/**
 * The similarity of every pair of texts as a square matrix: rows and columns are the numbered texts, the darker
 * the cell the closer the meaning. Pointing at a cell (or a text in the key) highlights its pair.
 */
export function Heatmap({ texts, sim }: { texts: string[]; sim: number[][] }) {
  const n = texts.length;
  const [hover, setHover] = useState<{ r: number; c: number } | null>(null);
  const [focusText, setFocusText] = useState<number | null>(null);
  const inline = n <= 10;
  const on = (i: number) => hover?.r === i || hover?.c === i || focusText === i;

  const readout = useMemo(() => {
    if (!hover || hover.r === hover.c) return null;
    return { a: hover.r, b: hover.c, v: sim[hover.r][hover.c] };
  }, [hover, sim]);

  return (
    <div className="flex flex-col gap-6 xl:flex-row xl:items-start">
      <div className="flex shrink-0 flex-col gap-3" style={{ width: `min(100%, ${2 + n * 3.5}rem)` }}>
        <div
          role="grid"
          aria-label="Similarity of every pair of texts"
          className="grid gap-[2px]"
          style={{ gridTemplateColumns: `1.5rem repeat(${n}, minmax(0, 1fr))` }}
          onPointerLeave={() => setHover(null)}
        >
          <span />
          {texts.map((_, c) => (
            <span key={c} className={cn("pb-1 text-center text-[11px] tabular-nums text-muted-foreground", on(c) && "font-semibold text-foreground")}>
              {c + 1}
            </span>
          ))}
          {sim.map((row, r) => (
            <Row key={r}>
              <span className={cn("grid place-items-center text-[11px] tabular-nums text-muted-foreground", on(r) && "font-semibold text-foreground")}>{r + 1}</span>
              {row.map((v, c) => {
                const self = r === c;
                const lit = hover && (hover.r === r || hover.c === c);
                const exact = hover?.r === r && hover?.c === c;
                return (
                  <button
                    type="button"
                    role="gridcell"
                    key={c}
                    aria-label={self ? `Text ${r + 1} with itself` : `Text ${r + 1} and text ${c + 1}: ${fmt(v)}`}
                    title={self ? texts[r] : `${fmt(v)}\n${r + 1}. ${texts[r]}\n${c + 1}. ${texts[c]}`}
                    onPointerEnter={() => setHover({ r, c })}
                    onFocus={() => setHover({ r, c })}
                    onBlur={() => setHover(null)}
                    className={cn(
                      "relative grid aspect-square min-w-0 place-items-center rounded-[3px] text-[11px] font-medium tabular-nums outline-none transition-[opacity,box-shadow]",
                      self ? "bg-muted/60 text-muted-foreground/60" : inkFor(v),
                      hover && !lit && "opacity-55",
                      exact && "z-10 ring-2 ring-foreground",
                      "focus-visible:z-10 focus-visible:ring-2 focus-visible:ring-ring",
                    )}
                    style={self ? undefined : { background: cellColor(v) }}
                  >
                    {self ? "–" : inline ? fmt(v).replace(/^0/, "") : ""}
                  </button>
                );
              })}
            </Row>
          ))}
        </div>
        <Legend />
        <div aria-live="polite" className="min-h-10 rounded-md border bg-card/60 px-3 py-2 text-xs">
          {readout ? (
            <span className="flex flex-wrap items-baseline gap-x-2">
              <span className="font-semibold tabular-nums">{fmt(readout.v)}</span>
              <span className="text-muted-foreground">
                {readout.a + 1} ↔ {readout.b + 1}
              </span>
            </span>
          ) : (
            <span className="text-muted-foreground">Point at a cell to compare two texts.</span>
          )}
        </div>
      </div>
      <ol className="flex min-w-0 flex-1 flex-col gap-0.5 text-sm" aria-label="The texts">
        {texts.map((t, i) => (
          <li
            key={i}
            onPointerEnter={() => setFocusText(i)}
            onPointerLeave={() => setFocusText(null)}
            className={cn("flex gap-2.5 rounded-md px-2 py-1.5 transition-colors", on(i) && "bg-accent")}
          >
            <span className="w-5 shrink-0 text-right text-xs leading-5 tabular-nums text-muted-foreground">{i + 1}</span>
            <span className="min-w-0 break-words">{t}</span>
          </li>
        ))}
      </ol>
    </div>
  );
}

/** A row of the grid without a wrapper element (the grid places the cells). */
function Row({ children }: { children: React.ReactNode }) {
  return <>{children}</>;
}

function Legend() {
  const stops = [0, 0.25, 0.5, 0.75, 1];
  return (
    <div className="flex items-center gap-2 text-[11px] text-muted-foreground">
      <span>unrelated</span>
      <div className="flex h-2 flex-1 overflow-hidden rounded-full">
        {stops.map((s) => (
          <span key={s} className="flex-1" style={{ background: cellColor(s) }} />
        ))}
      </div>
      <span>same meaning</span>
    </div>
  );
}

/** Every pair once, most similar first. */
export function pairs(sim: number[][]) {
  const out: { a: number; b: number; v: number }[] = [];
  for (let a = 0; a < sim.length; a++) for (let b = a + 1; b < sim.length; b++) out.push({ a, b, v: sim[a][b] });
  return out.sort((x, y) => y.v - x.v);
}
