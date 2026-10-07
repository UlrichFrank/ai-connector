import { useMemo } from "react";

/**
 * The training loss over the steps: every step faint, a smoothed line on top (the loss of single steps jumps a lot
 * with flow matching: the noise level is random per step). The x axis runs to the planned steps.
 */
export function LossChart({ points, total }: { points: { step: number; loss: number }[]; total: number }) {
  const W = 600;
  const H = 140;
  const pad = { l: 36, r: 8, t: 8, b: 18 };
  const { raw, smooth, lo, hi, last } = useMemo(() => {
    const ys = points.map((p) => p.loss).filter((v) => isFinite(v));
    // the axis leaves out the wildest 2 % (a few huge early steps flatten everything else)
    const sorted = [...ys].sort((a, b) => a - b);
    const lo = sorted[0] ?? 0;
    // (and stays within 3× the median: the first steps of a fresh LoRA can be far off)
    const p98 = sorted[Math.max(0, Math.floor(sorted.length * 0.98) - 1)] ?? 1;
    const hi = Math.min(p98, 3 * (sorted[Math.floor(sorted.length / 2)] ?? p98));
    const a = Math.min(0.3, 10 / Math.max(10, points.length));
    const smooth: { step: number; loss: number }[] = [];
    for (const p of points) smooth.push({ step: p.step, loss: smooth.length ? a * p.loss + (1 - a) * smooth[smooth.length - 1].loss : p.loss });
    return { raw: points, smooth, lo, hi: hi > lo ? hi : lo + 1, last: smooth[smooth.length - 1]?.loss };
  }, [points]);
  const maxStep = Math.max(total, points[points.length - 1]?.step ?? 1);
  const x = (s: number) => pad.l + ((W - pad.l - pad.r) * s) / maxStep;
  const y = (v: number) => pad.t + (H - pad.t - pad.b) * (1 - (Math.min(v, hi) - lo) / (hi - lo));
  const path = (ps: { step: number; loss: number }[]) => ps.map((p, i) => `${i ? "L" : "M"}${x(p.step).toFixed(1)},${y(p.loss).toFixed(1)}`).join("");
  return (
    <figure className="flex flex-col gap-1">
      <figcaption className="flex items-baseline gap-2 text-xs text-muted-foreground">
        <span className="font-semibold tracking-wide">LOSS</span>
        {last != null && <span className="tabular-nums">now ≈ {last.toFixed(3)} (smoothed)</span>}
      </figcaption>
      <svg viewBox={`0 0 ${W} ${H}`} className="w-full text-primary" role="img" aria-label="Training loss">
        {[0, 0.5, 1].map((f) => {
          const v = lo + (hi - lo) * f;
          return (
            <g key={f} className="text-muted-foreground">
              <line x1={pad.l} x2={W - pad.r} y1={y(v)} y2={y(v)} stroke="currentColor" strokeOpacity={0.15} vectorEffect="non-scaling-stroke" />
              <text x={pad.l - 4} y={y(v) + 3} textAnchor="end" fontSize={10} fill="currentColor">
                {v.toFixed(2)}
              </text>
            </g>
          );
        })}
        <text x={W - pad.r} y={H - 4} textAnchor="end" fontSize={10} className="fill-muted-foreground">
          step {maxStep}
        </text>
        <path d={path(raw)} fill="none" stroke="currentColor" strokeOpacity={0.25} strokeWidth={1} vectorEffect="non-scaling-stroke" />
        <path d={path(smooth)} fill="none" stroke="currentColor" strokeWidth={2} vectorEffect="non-scaling-stroke" />
      </svg>
    </figure>
  );
}
