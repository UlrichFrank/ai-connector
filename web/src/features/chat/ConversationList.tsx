import { t } from "@/lib/i18n";
import { Copy, Loader2, MessageSquarePlus, MoreHorizontal, Pencil, Search, Trash2, X } from "lucide-react";
import { memo, useMemo, useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { ago } from "@/lib/format";
import { useNow } from "@/lib/now";
import { cn } from "@/lib/utils";
import { isActive, useApp } from "@/store/app";
import { copyChat } from "./copy";
import { useQueue, useQueued } from "./queueStore";
import { historyAnswer, turnsOf, type Conversation } from "./threads";
import { pendingOf, useTools } from "./tools";
import { ZoneBadge } from "./zones";

export function ConversationList({
  conversations,
  current,
  onOpen,
  onNew,
  onDeleted,
}: {
  conversations: Conversation[];
  current?: string;
  onOpen: (c: Conversation) => void;
  onNew: () => void;
  onDeleted: (c: Conversation) => void;
}) {
  const [q, setQ] = useState("");
  const [deleting, setDeleting] = useState<Conversation | null>(null);
  const now = useNow(60_000);

  const shown = useMemo(() => {
    const words = q.toLowerCase().split(/\s+/).filter(Boolean);
    if (!words.length) return conversations;
    return conversations.filter((c) => {
      const hay = (c.title + "\n" + c.jobs.map((j) => turnsOf(j).at(-1)?.Content + "\n" + historyAnswer(j)).join("\n")).toLowerCase();
      return words.every((w) => hay.includes(w));
    });
  }, [conversations, q]);

  const groups = useMemo(() => {
    const start = new Date(now);
    start.setHours(0, 0, 0, 0);
    const today = shown.filter((c) => Date.parse(c.updated) >= start.getTime());
    const earlier = shown.filter((c) => Date.parse(c.updated) < start.getTime());
    return [
      { name: t("Today"), list: today },
      { name: t("Earlier"), list: earlier },
    ].filter((g) => g.list.length);
  }, [shown, now]);

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-col gap-2 p-3">
        <Button variant="outline" className="justify-start gap-2" onClick={onNew}>
          <MessageSquarePlus /> New chat
        </Button>
        <div className="relative">
          <Search className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder={t("Search chats")} className="h-8 pr-7 pl-8 text-sm" />
          {q && (
            <button className="absolute top-1/2 right-2 -translate-y-1/2 text-muted-foreground hover:text-foreground" onClick={() => setQ("")} title={t("Clear")}>
              <X className="size-3.5" />
            </button>
          )}
        </div>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-3">
        {groups.map((g) => (
          <div key={g.name} className="mb-2">
            <div className="px-2 pt-2 pb-1 text-[11px] font-semibold tracking-wide text-muted-foreground uppercase">{g.name}</div>
            {g.list.map((c) => (
              <Row key={c.id} c={c} current={c.id === current} onOpen={onOpen} onDelete={setDeleting} />
            ))}
          </div>
        ))}
        {!groups.length && (
          <p className="px-3 py-6 text-center text-sm text-muted-foreground">{q ? t("No chat matches.") : t("Your chats appear here.")}</p>
        )}
      </div>
      <DeleteConversation c={deleting} onClose={() => setDeleting(null)} onDeleted={onDeleted} />
    </div>
  );
}

const Row = memo(function Row({
  c,
  current,
  onOpen,
  onDelete,
}: {
  c: Conversation;
  current: boolean;
  onOpen: (c: Conversation) => void;
  onDelete: (c: Conversation) => void;
}) {
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(c.title);
  const put = useApp((s) => s.put);
  const models = new Set(c.roots.map((r) => r.model)).size;
  const pending = useTools((s) => (c.active ? pendingOf(c.jobs, s.jobs) : 0));
  const queued = useQueued(c.id);

  const save = async () => {
    setEditing(false);
    const title = name.trim();
    if (!title || title === c.title) return;
    try {
      put(await api.rename(c.root.id, title));
    } catch (e) {
      toast.error(t("Could not rename"), { description: errorText(e) });
    }
  };

  if (editing)
    return (
      <form
        className="px-1 py-0.5"
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
      >
        <Input autoFocus value={name} onChange={(e) => setName(e.target.value)} onBlur={save} onKeyDown={(e) => e.key === "Escape" && setEditing(false)} className="h-8 text-sm" />
      </form>
    );

  return (
    <div
      className={cn(
        "group/row relative flex items-center rounded-lg text-sm transition-colors hover:bg-accent/70",
        current && "bg-accent text-accent-foreground hover:bg-accent",
      )}
    >
      <button className="flex min-w-0 flex-1 flex-col items-start gap-0.5 px-2.5 py-2 text-left" onClick={() => onOpen(c)}>
        <span className="flex w-full items-center gap-1.5">
          {c.active && !pending && <Loader2 className="size-3 shrink-0 animate-spin text-primary" />}
          {pending > 0 && <span className="size-2 shrink-0 animate-pulse rounded-full bg-primary" title={t("A tool waits for your OK")} />}
          <span className="truncate">{c.title}</span>
        </span>
        <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <ZoneBadge zone={c.root.request.zone} className="h-4 px-1 text-[10px]" />
          <span className="truncate">
            {pending > 0 ? <span className="text-[var(--teal-ink)]">{t("Needs your OK")}</span> : ago(c.updated)}
            {models > 1 && ` · ${models} models`}
            {queued > 0 && <span className="text-foreground/80"> · {queued} queued</span>}
          </span>
        </span>
      </button>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="ghost"
            size="icon-sm"
            className="mr-1 shrink-0 text-muted-foreground opacity-100 data-[state=open]:opacity-100 md:opacity-0 md:group-hover/row:opacity-100"
            title={t("More")}
          >
            <MoreHorizontal />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-40">
          <DropdownMenuItem onSelect={() => (setName(c.title), setEditing(true))}>
            <Pencil /> Rename
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => copyChat(c.id)}>
            <Copy /> Copy chat
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem variant="destructive" onSelect={() => onDelete(c)}>
            <Trash2 /> Delete
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  );
});

/** Deletes every job of a conversation, after asking (like DeleteJob). Running answers are stopped first. */
export function DeleteConversation({ c, onClose, onDeleted }: { c: Conversation | null; onClose: () => void; onDeleted: (c: Conversation) => void }) {
  const drop = useApp((s) => s.drop);
  const clearQueue = useQueue((s) => s.clear);
  const starred = c?.jobs.some((j) => j.starred);
  return (
    <AlertDialog open={!!c} onOpenChange={(o) => !o && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t("Delete this chat?")}</AlertDialogTitle>
          <AlertDialogDescription>
            {c?.jobs.length === 1 ? t("“{title}” and its answer will be deleted.", { title: c?.title ?? "" }) : t("“{title}” and its {n} answers will be deleted.", { title: c?.title ?? "", n: c?.jobs.length ?? 0 })} {starred ? t("Some are starred.") : ""} {t("This cannot be undone.")}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>{t("Cancel")}</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            onClick={async () => {
              if (!c) return;
              clearQueue(c.id); // first: a queued message must not follow the answers being stopped
              let failed = 0;
              for (const j of c.jobs) {
                try {
                  if (isActive(j)) await api.cancel(j.id).catch(() => {});
                  await api.remove(j.id);
                  drop(j.id);
                } catch {
                  failed++;
                }
              }
              if (failed) toast.error(t("Delete failed"), { description: t("{n} of the answers could not be deleted.", { n: failed }) });
              onDeleted(c);
            }}
          >
            {t("Delete")}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
