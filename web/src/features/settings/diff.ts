/** One line of a line diff: kept, only in the default (removed) or only in the edit (added). */
export interface DiffLine {
  op: "same" | "del" | "add";
  text: string;
}

/**
 * A line diff of two texts (longest common subsequence; prompts are short). Long single-paragraph prompts are split
 * into sentences first, so a changed sentence shows instead of one changed line.
 */
export function diffLines(a: string, b: string): DiffLine[] {
  const split = (s: string) => s.split(/\n|(?<=[.!?:])\s+(?=[A-Z"“(])/).filter((l, i, all) => l !== "" || i < all.length - 1);
  const x = split(a);
  const y = split(b);
  const n = x.length;
  const m = y.length;
  const L: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) for (let j = m - 1; j >= 0; j--) L[i][j] = x[i] === y[j] ? L[i + 1][j + 1] + 1 : Math.max(L[i + 1][j], L[i][j + 1]);
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (x[i] === y[j]) out.push({ op: "same", text: x[i++] }), j++;
    else if (L[i + 1][j] >= L[i][j + 1]) out.push({ op: "del", text: x[i++] });
    else out.push({ op: "add", text: y[j++] });
  }
  while (i < n) out.push({ op: "del", text: x[i++] });
  while (j < m) out.push({ op: "add", text: y[j++] });
  return out;
}
