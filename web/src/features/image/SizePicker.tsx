import { ArrowLeftRight } from "lucide-react";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { snap } from "./form";

const presets = [
  { id: "square", label: "Square", w: 1024, h: 1024 },
  { id: "portrait", label: "Portrait", w: 896, h: 1152 },
  { id: "landscape", label: "Landscape", w: 1152, h: 896 },
  { id: "wide", label: "Wide", w: 1344, h: 768 },
] as const;

/** The shape of the picture: four common ones, or any width × height (multiples of 16). */
export function SizePicker({ width, height, onChange }: { width: number; height: number; onChange: (w: number, h: number) => void }) {
  const match = presets.find((p) => p.w === width && p.h === height);
  const [customOpen, setCustomOpen] = useState(false);
  const custom = !match || customOpen;

  return (
    <div className="flex flex-col gap-2">
      <div role="radiogroup" className="grid grid-cols-5 gap-1 rounded-lg bg-muted/60 p-1">
        {presets.map((p) => (
          <Segment key={p.id} on={!custom && match?.id === p.id} label={p.label} onClick={() => (setCustomOpen(false), onChange(p.w, p.h))}>
            <Shape w={p.w} h={p.h} />
          </Segment>
        ))}
        <Segment on={custom} label="Custom" onClick={() => setCustomOpen(true)}>
          <span className="grid h-4 place-items-center text-[10px] leading-none font-semibold tracking-tight">W×H</span>
        </Segment>
      </div>
      {custom ? (
        <div className="flex items-center gap-1.5">
          <Dim value={width} onChange={(w) => onChange(w, height)} label="Width" />
          <Button variant="ghost" size="icon-sm" title="Swap width and height" className="text-muted-foreground" onClick={() => onChange(height, width)}>
            <ArrowLeftRight />
          </Button>
          <Dim value={height} onChange={(h) => onChange(width, h)} label="Height" />
          <span className="ml-1 text-xs whitespace-nowrap text-muted-foreground">{ratio(width, height)}</span>
        </div>
      ) : (
        <div className="px-1 text-xs text-muted-foreground tabular-nums">
          {width} × {height} · {ratio(width, height)}
        </div>
      )}
    </div>
  );
}

function Segment({ on, label, onClick, children }: { on: boolean; label: string; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={on}
      onClick={onClick}
      className={cn(
        "flex min-w-0 flex-col items-center gap-1 rounded-md px-1 pt-2 pb-1.5 text-[11px] font-medium text-muted-foreground transition-colors hover:text-foreground",
        on && "bg-background text-foreground shadow-sm ring-1 ring-foreground/10",
      )}
    >
      <span className="grid h-4 place-items-center">{children}</span>
      <span className="w-full truncate text-center">{label}</span>
    </button>
  );
}

/** A small outline of the aspect ratio. */
function Shape({ w, h }: { w: number; h: number }) {
  const k = 16 / Math.max(w, h);
  return <span className="block rounded-[3px] border-[1.5px] border-current" style={{ width: Math.round(w * k), height: Math.round(h * k) }} />;
}

/** A number field that settles on a multiple of 16 when you leave it. */
function Dim({ value, onChange, label }: { value: number; onChange: (v: number) => void; label: string }) {
  const [text, setText] = useState<string | null>(null);
  return (
    <Input
      aria-label={label}
      inputMode="numeric"
      className="h-8 w-20 text-center tabular-nums"
      value={text ?? String(value)}
      onChange={(e) => {
        setText(e.target.value);
        const n = parseInt(e.target.value, 10);
        if (n >= 256 && n <= 2048) onChange(n);
      }}
      onBlur={() => {
        onChange(snap(parseInt(text ?? String(value), 10)));
        setText(null);
      }}
      onKeyDown={(e) => e.key === "Enter" && (e.target as HTMLInputElement).blur()}
    />
  );
}

const common = [
  [16, 9],
  [9, 16],
  [21, 9],
  [3, 2],
  [2, 3],
  [4, 3],
  [3, 4],
] as const;

function gcd(a: number, b: number): number {
  return b ? gcd(b, a % b) : a;
}

/** "16:9", or "1.75:1" when it is not a small ratio. */
function ratio(w: number, h: number): string {
  const g = gcd(w, h) || 1;
  const a = w / g;
  const b = h / g;
  if (a <= 10 && b <= 10 && !(a === 7 && b === 4) && !(a === 4 && b === 7)) return `${a}:${b}`;
  const near = common.find(([x, y]) => Math.abs(w / h / (x / y) - 1) < 0.02);
  if (near) return `≈ ${near[0]}:${near[1]}`;
  if (a <= 21 && b <= 21) return `${a}:${b}`;
  return w >= h ? `${(w / h).toFixed(2)}:1` : `1:${(h / w).toFixed(2)}`;
}
