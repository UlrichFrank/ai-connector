import { useEffect, useState } from "react";
import { cn } from "@/lib/utils";

/** The modules of a QR code (true = dark), row by row. */
type Matrix = boolean[][];

/**
 * Encodes text as a QR code (error correction M, the smallest version that fits). qrcode-generator is loaded only
 * here, when the first code is shown, so it stays out of the main bundle.
 */
async function encode(text: string): Promise<Matrix> {
  const { default: qrcode } = await import("qrcode-generator");
  const qr = qrcode(0, "M");
  qr.addData(text, "Byte");
  qr.make();
  const n = qr.getModuleCount();
  return Array.from({ length: n }, (_, r) => Array.from({ length: n }, (_, c) => qr.isDark(r, c)));
}

/** The dark modules as one SVG path: a run of dark modules in a row is one rectangle. */
function path(m: Matrix, quiet: number): string {
  let d = "";
  m.forEach((row, r) => {
    for (let c = 0; c < row.length; c++) {
      if (!row[c]) continue;
      const start = c;
      while (c + 1 < row.length && row[c + 1]) c++;
      d += `M${start + quiet} ${r + quiet}h${c - start + 1}v1h${start - c - 1}z`;
    }
  });
  return d;
}

/**
 * A QR code of `value`, drawn as crisp SVG at any size: always black on white with the quiet zone around it (cameras
 * read that best, in dark mode too). `blurred` hides a code that no longer works.
 */
export function QrCode({ value, label, blurred, className }: { value: string; label: string; blurred?: boolean; className?: string }) {
  const [matrix, setMatrix] = useState<{ value: string; m: Matrix } | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let live = true;
    encode(value).then(
      (m) => live && setMatrix({ value, m }),
      () => live && setFailed(true),
    );
    return () => {
      live = false;
    };
  }, [value]);

  const quiet = 4;
  const m = matrix?.value === value ? matrix.m : null;
  const size = m ? m.length + 2 * quiet : 0;
  return (
    <div className={cn("relative aspect-square overflow-hidden rounded-xl bg-white shadow-sm ring-1 ring-black/10", className)}>
      {m ? (
        <svg
          viewBox={`0 0 ${size} ${size}`}
          role="img"
          aria-label={label}
          shapeRendering="crispEdges"
          className={cn("size-full transition-[filter,opacity] duration-300", blurred && "opacity-40 blur-[6px]")}
          data-qr={value}
        >
          <path d={path(m, quiet)} fill="#000" />
        </svg>
      ) : (
        <div className="grid size-full place-items-center p-6 text-center text-xs text-neutral-500">{failed ? "Could not draw the QR code. Use the link below." : null}</div>
      )}
    </div>
  );
}
