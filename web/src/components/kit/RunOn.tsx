import { useEffect, useState } from "react";
import type { Model } from "@/api/types";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { whereText } from "@/lib/macs";
import { local } from "@/lib/storage";
import { useApp } from "@/store/app";
import { Field } from "./Layout";

const AUTO = "auto";

/**
 * The Mac to run on ("Run on" in Advanced): Auto lets the hub pick (loaded > on disk > download, idle first, most
 * RAM); a Mac pins it. Remembered per page; a remembered Mac that is not connected now counts as Auto.
 * `worker` goes into the request as is (undefined = Auto).
 */
export function useRunOn(page: string) {
  const macs = useApp((s) => s.macs);
  const key = `runOn.${page}`;
  const [chosen, setChosen] = useState<string>(() => local.get<string>(key, ""));
  useEffect(() => local.set(key, chosen), [key, chosen]);
  const worker = chosen && macs.some((m) => m.id === chosen) ? chosen : undefined;
  return { worker, set: setChosen, shown: macs.length > 1 };
}

/** The Mac a page's next job is pinned to (`worker` of the request), or undefined for Auto. */
export function runOnWorker(page: string): string | undefined {
  const id = local.get<string>(`runOn.${page}`, "");
  return id && useApp.getState().macs.some((m) => m.id === id) ? id : undefined;
}

/**
 * The "Run on" select of a page's Advanced section; nothing with one Mac. `model`: the chosen model, to say how each
 * Mac holds it. Submit with `worker: runOnWorker(page)`.
 */
export function RunOn({ page, model }: { page: string; model?: Model }) {
  const choice = useRunOn(page);
  const macs = useApp((s) => s.macs);
  if (!choice.shown) return null;
  const about = (id: string) => {
    const w = model?.workers?.find((x) => x.id === id);
    return w ? whereText(w) : "";
  };
  const fits = (id: string) => model?.workers?.find((x) => x.id === id)?.fits !== false;
  return (
    <Field label="Run on" hint={choice.worker ? "Only this Mac; it waits for that Mac if it is busy." : "The hub picks the Mac that has the model ready and is free."}>
      <Select value={choice.worker ?? AUTO} onValueChange={(v) => choice.set(v === AUTO ? "" : v)}>
        <SelectTrigger className="w-full" data-run-on="">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={AUTO}>Auto (best Mac)</SelectItem>
          {macs.map((m) => (
            <SelectItem key={m.id} value={m.id} disabled={!fits(m.id)}>
              <span>{m.name || "Mac"}</span>
              {about(m.id) && <span className="text-muted-foreground">· {about(m.id)}</span>}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </Field>
  );
}
