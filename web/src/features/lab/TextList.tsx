import { X } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";

interface Item {
  id: number;
  text: string;
}

let next = 1;
const items = (texts: string[]): Item[] => texts.map((text) => ({ id: next++, text }));

/** Lines of pasted or loaded text, trimmed, without empty ones. */
export const lines = (s: string) =>
  s
    .split(/\r?\n/)
    .map((l) => l.trim())
    .filter(Boolean);

/**
 * The texts of a lab page: a numbered list that is edited in place (the numbers are the ones in the results). Kept
 * per page in this browser, so a reload does not lose them.
 */
export function useTextList(key: string, initial: string[] = []) {
  const [list, setList] = useState<Item[]>(() => items(local.get<string[]>(key, initial)));
  const texts = list.map((i) => i.text.trim()).filter(Boolean);
  useEffect(() => local.set(key, list.map((i) => i.text)), [key, list]);
  const set = useCallback((t: string[]) => setList(items(t)), []);
  return { list, setList, texts, set };
}

/**
 * Edits the list: Enter makes a new text below, Backspace in an empty one removes it, pasting several lines makes
 * a text of each. The last, empty row adds one.
 */
export function TextList({ state, placeholder = "Add a text …", max = 64 }: { state: ReturnType<typeof useTextList>; placeholder?: string; max?: number }) {
  const { list, setList } = state;
  const refs = useRef(new Map<number | "new", HTMLInputElement>());
  const [focus, setFocus] = useState<{ id: number | "new"; at?: number } | null>(null);

  useEffect(() => {
    if (!focus) return;
    const el = refs.current.get(focus.id);
    if (el) {
      el.focus();
      if (focus.at != null) el.setSelectionRange(focus.at, focus.at);
    }
    setFocus(null);
  }, [focus]);

  const update = (id: number, text: string) => setList((l) => l.map((i) => (i.id === id ? { ...i, text } : i)));
  const remove = (id: number) => setList((l) => l.filter((i) => i.id !== id));
  const insertAfter = (id: number | "new", texts: string[]) => {
    const add = items(texts);
    setList((l) => {
      const at = id === "new" ? l.length : l.findIndex((i) => i.id === id) + 1;
      return [...l.slice(0, at), ...add, ...l.slice(at)].slice(0, max);
    });
    return add;
  };

  const onPaste = (id: number | "new", e: React.ClipboardEvent<HTMLInputElement>) => {
    const pasted = lines(e.clipboardData.getData("text"));
    if (pasted.length < 2) return;
    e.preventDefault();
    if (id !== "new") {
      // the first line goes into this row (if it is empty), the rest below
      const row = list.find((i) => i.id === id);
      if (row && !row.text.trim()) {
        update(id, pasted[0]);
        pasted.shift();
      }
    }
    const added = insertAfter(id, pasted);
    setFocus({ id: id === "new" ? "new" : (added.at(-1)?.id ?? id) });
  };

  const onKey = (it: Item, i: number, e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter") {
      e.preventDefault();
      if (!it.text.trim()) return;
      if (i === list.length - 1) setFocus({ id: "new" });
      else setFocus({ id: list[i + 1].id });
    } else if (e.key === "Backspace" && !it.text) {
      e.preventDefault();
      remove(it.id);
      const prev = list[i - 1];
      setFocus(prev ? { id: prev.id, at: prev.text.length } : list[i + 1] ? { id: list[i + 1].id } : { id: "new" });
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      setFocus({ id: list[i + 1]?.id ?? "new" });
    } else if (e.key === "ArrowUp" && i > 0) {
      e.preventDefault();
      setFocus({ id: list[i - 1].id });
    }
  };

  const full = list.length >= max;
  return (
    <ol className="flex flex-col overflow-hidden rounded-lg border bg-background/40 dark:bg-input/20">
      {list.map((it, i) => (
        <li key={it.id} className="group flex items-center border-b last:border-b-0 focus-within:bg-accent/40">
          <Num n={i + 1} />
          <input
            ref={(el) => void (el ? refs.current.set(it.id, el) : refs.current.delete(it.id))}
            value={it.text}
            onChange={(e) => update(it.id, e.target.value)}
            onKeyDown={(e) => onKey(it, i, e)}
            onPaste={(e) => onPaste(it.id, e)}
            aria-label={`Text ${i + 1}`}
            className="h-9 min-w-0 flex-1 bg-transparent pr-1 text-sm outline-none"
          />
          <button
            type="button"
            onClick={() => remove(it.id)}
            aria-label={`Remove text ${i + 1}`}
            className="mr-1 grid size-7 shrink-0 place-items-center rounded-md text-muted-foreground opacity-60 hover:bg-accent hover:text-foreground hover:opacity-100 focus-visible:opacity-100 sm:opacity-0 sm:group-hover:opacity-100"
          >
            <X className="size-3.5" />
          </button>
        </li>
      ))}
      {!full && (
        <li className="flex items-center focus-within:bg-accent/40">
          <Num n={list.length + 1} quiet />
          <input
            ref={(el) => void (el ? refs.current.set("new", el) : refs.current.delete("new"))}
            value=""
            placeholder={placeholder}
            onChange={(e) => {
              const [added] = insertAfter("new", [e.target.value]);
              setFocus({ id: added.id });
            }}
            onPaste={(e) => onPaste("new", e)}
            aria-label="Add a text"
            className="h-9 min-w-0 flex-1 bg-transparent pr-3 text-sm outline-none placeholder:text-muted-foreground/70"
          />
        </li>
      )}
    </ol>
  );
}

function Num({ n, quiet }: { n: number; quiet?: boolean }) {
  return <span className={cn("w-9 shrink-0 text-center text-xs tabular-nums", quiet ? "text-muted-foreground/50" : "text-muted-foreground")}>{n}</span>;
}
