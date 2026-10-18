import { t } from "@/lib/i18n";
import { ArrowDown, ArrowUp, CirclePause, GripVertical, Pencil, SendHorizontal, X } from "lucide-react";
import { useLayoutEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { edit, move, moveTo, pauseText, remove, type Queued } from "@/lib/chatQueue";
import { cn } from "@/lib/utils";
import { useQueue } from "./queueStore";

/** The messages queued in a conversation, under the running answer; and the bar when the queue paused. */
export function QueueView({
  conv,
  editing,
  setEditing,
  onSendNow,
  onContinue,
}: {
  conv: string;
  /** The message being edited inline. */
  editing: string | null;
  setEditing: (id: string | null) => void;
  onSendNow: (id: string) => void;
  onContinue: () => void;
}) {
  const q = useQueue((s) => s.queues[conv]);
  const update = useQueue((s) => s.update);
  const clear = useQueue((s) => s.clear);
  const [drag, setDrag] = useState<{ id: string; over: number } | null>(null);
  if (!q || (!q.items.length && !q.inflight)) return null;
  const items = q.items;
  const change = (f: Parameters<typeof update>[1]) => update(conv, f);

  return (
    <div className="flex flex-col gap-2" aria-label={t("Queued messages")}>
      {q.paused && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2 rounded-xl border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-sm">
          <CirclePause className="size-4 shrink-0 text-amber-600 dark:text-amber-400" />
          <span className="min-w-0 flex-1">
            <span className="font-medium">{t("Queue paused")}</span>
            <span className="text-muted-foreground"> — {pauseText(q.paused)}</span>
          </span>
          <span className="flex gap-1.5">
            <Button size="sm" variant="ghost" className="h-7 text-muted-foreground" onClick={() => clear(conv)}>
              {t("Discard all")}
            </Button>
            <Button size="sm" className="h-7" onClick={onContinue}>
              {t("Continue")}
            </Button>
          </span>
        </div>
      )}
      <div className="flex items-center justify-end gap-2 pr-1 text-[11px] font-medium tracking-wide text-muted-foreground uppercase">
        {q.inflight ? t("Sending …") : t("{n} queued", { n: items.length })}
        {!q.paused && !q.inflight && <span className="font-normal tracking-normal normal-case">· {t("sent when the answer is done")}</span>}
      </div>
      <ol className="flex flex-col gap-1.5">
        {items.map((it, i) => (
          <li
            key={it.id}
            onDragOver={(e) => {
              if (!drag) return;
              e.preventDefault();
              const r = e.currentTarget.getBoundingClientRect();
              const over = e.clientY < r.top + r.height / 2 ? i : i + 1;
              if (over !== drag.over) setDrag({ ...drag, over });
            }}
            onDrop={(e) => {
              e.preventDefault();
              if (!drag) return;
              const from = items.findIndex((x) => x.id === drag.id);
              const to = drag.over > from ? drag.over - 1 : drag.over;
              change((x) => moveTo(x, drag.id, to));
              setDrag(null);
            }}
            className={cn(
              "relative flex justify-end",
              drag && drag.over === i && drag.id !== it.id && "before:absolute before:-top-1 before:right-0 before:left-[15%] before:h-0.5 before:rounded before:bg-primary",
              drag && drag.over === i + 1 && i === items.length - 1 && "after:absolute after:-bottom-1 after:right-0 after:left-[15%] after:h-0.5 after:rounded after:bg-primary",
            )}
          >
            <Item
              n={i + 1}
              item={it}
              first={i === 0}
              last={i === items.length - 1}
              dragging={drag?.id === it.id}
              editing={editing === it.id}
              onEdit={() => setEditing(it.id)}
              onSave={(text) => {
                change((x) => edit(x, it.id, text));
                setEditing(null);
              }}
              onCancel={() => setEditing(null)}
              onRemove={() => change((x) => remove(x, it.id))}
              onMove={(d) => change((x) => move(x, it.id, d))}
              onSendNow={() => onSendNow(it.id)}
              onDragStart={() => setDrag({ id: it.id, over: i })}
              onDragEnd={() => setDrag(null)}
            />
          </li>
        ))}
      </ol>
    </div>
  );
}

function Item({
  n,
  item,
  first,
  last,
  dragging,
  editing,
  onEdit,
  onSave,
  onCancel,
  onRemove,
  onMove,
  onSendNow,
  onDragStart,
  onDragEnd,
}: {
  n: number;
  item: Queued;
  first: boolean;
  last: boolean;
  dragging: boolean;
  editing: boolean;
  onEdit: () => void;
  onSave: (text: string) => void;
  onCancel: () => void;
  onRemove: () => void;
  onMove: (delta: number) => void;
  onSendNow: () => void;
  onDragStart: () => void;
  onDragEnd: () => void;
}) {
  const [text, setText] = useState(item.text);
  const area = useRef<HTMLTextAreaElement>(null);
  useLayoutEffect(() => {
    if (!editing) return;
    setText(item.text);
    const t = area.current;
    if (t) {
      t.focus();
      t.setSelectionRange(t.value.length, t.value.length);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editing]);
  useLayoutEffect(() => {
    const t = area.current;
    if (!t) return;
    t.style.height = "auto";
    t.style.height = Math.min(t.scrollHeight, 240) + "px";
  }, [text, editing]);

  return (
    <div
      className={cn(
        "group/q flex w-full max-w-[85%] flex-col gap-1 rounded-2xl rounded-br-md border border-dashed border-border bg-muted/40 px-3 py-2 text-[0.9375rem] leading-relaxed text-muted-foreground transition-opacity",
        dragging && "opacity-40",
        editing && "border-solid border-ring/60 bg-card text-foreground",
      )}
    >
      <div className="flex items-start gap-2">
        <span
          draggable={!editing}
          onDragStart={(e) => {
            e.dataTransfer.effectAllowed = "move";
            e.dataTransfer.setData("text/plain", item.id);
            onDragStart();
          }}
          onDragEnd={onDragEnd}
          className={cn("mt-0.5 flex shrink-0 items-center gap-0.5", !editing && "cursor-grab active:cursor-grabbing")}
          title={t("Drag to reorder")}
        >
          <GripVertical className="size-3.5 opacity-40 max-sm:hidden" />
          <span className="grid size-5 place-items-center rounded-full bg-background text-[11px] font-medium tabular-nums ring-1 ring-border">{n}</span>
        </span>
        {editing ? (
          <textarea spellCheck={false} autoCorrect="off" autoCapitalize="off"
            ref={area}
            value={text}
            rows={1}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
                e.preventDefault();
                onSave(text);
              } else if (e.key === "Escape") {
                e.preventDefault();
                onCancel();
              }
            }}
            className="min-w-0 flex-1 resize-none bg-transparent outline-none"
            aria-label={t("Edit queued message {n}", { n })}
          />
        ) : (
          <span className="min-w-0 flex-1 whitespace-pre-wrap [overflow-wrap:anywhere]" onDoubleClick={onEdit}>
            {item.text}
          </span>
        )}
      </div>
      <div className="flex items-center justify-end gap-0.5">
        {editing ? (
          <>
            <Button size="sm" variant="ghost" className="h-7 text-muted-foreground" onClick={onCancel}>
              {t("Cancel")}
            </Button>
            <Button size="sm" className="h-7" onClick={() => onSave(text)}>
              {t("Save")}
            </Button>
          </>
        ) : (
          <>
            <span className="mr-auto pl-7 text-[11px] tracking-wide uppercase opacity-70">{t("queued")}</span>
            <Button variant="ghost" size="icon-sm" className="size-7 text-muted-foreground" title={t("Edit")} onClick={onEdit}>
              <Pencil className="size-3.5" />
            </Button>
            <Button variant="ghost" size="icon-sm" className="size-7 text-muted-foreground" title={t("Move up")} disabled={first} onClick={() => onMove(-1)}>
              <ArrowUp className="size-3.5" />
            </Button>
            <Button variant="ghost" size="icon-sm" className="size-7 text-muted-foreground" title={t("Move down")} disabled={last} onClick={() => onMove(1)}>
              <ArrowDown className="size-3.5" />
            </Button>
            <Button variant="ghost" size="sm" className="h-7 gap-1 px-2 text-xs text-muted-foreground" title={t("Stop the running answer and send this next")} onClick={onSendNow}>
              <SendHorizontal className="size-3.5" /> Send now
            </Button>
            <Button variant="ghost" size="icon-sm" className="size-7 text-muted-foreground hover:text-destructive" title={t("Remove")} onClick={onRemove}>
              <X className="size-3.5" />
            </Button>
          </>
        )}
      </div>
    </div>
  );
}
