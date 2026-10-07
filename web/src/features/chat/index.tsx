import { ArrowDown, Brain, Copy, MessageSquarePlus, MessagesSquare, MoreHorizontal, PanelLeft, Pencil, Trash2 } from "lucide-react";
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import { startFailed } from "@/lib/jobs";
import type { Example, Job, Message, Zone } from "@/api/types";
import { ExamplesMenu } from "@/components/kit/Examples";
import { ModelPicker, useModelChoice } from "@/components/kit/ModelPicker";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Sheet, SheetContent, SheetTitle } from "@/components/ui/sheet";
import { navigate } from "@/lib/route";
import { claim, enqueue, nextStep, pause, restore, resume, sendNow as queueFirst, sent, unsent } from "@/lib/chatQueue";
import { local } from "@/lib/storage";
import { thinkFor, thinkKey } from "@/lib/thinkSteps";
import { cn } from "@/lib/utils";
import { isActive, useApp } from "@/store/app";
import { Composer, type ComposerHandle, type Settings } from "./Composer";
import { ConversationList, DeleteConversation } from "./ConversationList";
import { copyChat } from "./copy";
import { Answer, Question, StaticAnswer } from "./Message";
import { QueueView } from "./QueueList";
import { fromLoad, queueId, useQueue, useQueued, type SendConfig } from "./queueStore";
import { followUp, type ChatOpts } from "./send";
import { columnsOf, deriveThreads, picksFor, systemOf, type Column, type Conversation } from "./threads";
import { listenForTools, pendingOf, useTools } from "./tools";
import { ZoneBadge, ZoneCards, ZonePicker, zoneInfo } from "./zones";

// temperature and length: the model's own defaults (Auto) until changed
const defaults: Settings = { system: "chat-assistant", think: false, temperature: null, maxTokens: null };

/**
 * The saved chat settings. Before version 2 the old fixed defaults (temperature 0.3, 4096 tokens) were saved with
 * any change: they become Auto (a low temperature makes a thinking model think in circles).
 */
function loadSettings(): Settings {
  const { v, ...s } = local.get<Partial<Settings> & { v?: number }>("chat.settings", {});
  if (!v) {
    if (s.temperature === 0.3) s.temperature = null;
    if (s.maxTokens === 4096) s.maxTokens = null;
  }
  return { ...defaults, ...s };
}

/** Which first answers belong together (the compare chats this browser started). */
function useLinks() {
  const [links, setLinks] = useState(() => local.get<Record<string, string>>("chat.links", {}));
  const add = useCallback((more: Record<string, string>) => {
    setLinks((l) => {
      const next = { ...l, ...more };
      local.set("chat.links", next);
      return next;
    });
  }, []);
  return [links, add] as const;
}

export function ChatPage({ arg }: { arg: string }) {
  const jobs = useApp((s) => s.jobs);
  const put = useApp((s) => s.put);
  const prompts = useApp((s) => s.prompts);
  const chatPrompts = useMemo(() => prompts.filter((p) => p.use === "chat"), [prompts]);
  const [links, addLinks] = useLinks();
  const threads = useMemo(() => deriveThreads(jobs, prompts, links), [jobs, prompts, links]);
  const conv = arg ? threads.byJob.get(arg) : undefined;

  const choice = useModelChoice("chat");
  useEffect(listenForTools, []);

  // the tools of a chat: chosen for a new one, fixed for a conversation
  const [newZone, setNewZone] = useState<Zone>("");
  const zone: Zone = conv ? (conv.root.request.zone ?? "") : newZone;
  const [saved, setSaved] = useState<Settings>(loadSettings);

  // what the open conversation uses: its own model and system prompt until changed here
  const [own, setOwn] = useState<{ id: string; system?: string; model?: string }>({ id: "" });
  const mine = own.id === conv?.id ? own : { id: conv?.id ?? "" };

  const [picks, setPicks] = useState<Record<string, string>>({});
  // a link to a turn deep in a conversation shows the branch it is on
  useEffect(() => {
    if (conv && arg !== conv.id) setPicks((p) => ({ ...p, ...picksFor(arg, conv, threads) }));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [arg, conv?.id]);
  const columns = useMemo(() => (conv ? columnsOf(conv, threads, picks) : []), [conv, threads, picks]);

  const convSystem = conv && columns[0]?.leaf ? systemOf(columns[0].leaf) : undefined;
  const libraryMatch = convSystem != null ? chatPrompts.find((p) => p.text.trim() === convSystem.trim()) : undefined;
  const keepText = convSystem != null && !libraryMatch ? convSystem : undefined;
  // Think, per zone: on for tools by default, the user's choice once made (lib/thinkSteps.ts thinkFor)
  const [thinkBy, setThinkBy] = useState(() => local.get<Record<string, boolean>>("chat.think", {}));
  const think = thinkFor(zone, thinkBy, saved.think);
  const settings: Settings = { ...saved, think, ...(conv ? { system: mine.system ?? (convSystem == null ? "none" : (libraryMatch?.id ?? "keep")) } : {}) };
  const setSettings = (patch: Partial<Settings>) => {
    const { system, think: thinkNow, ...more } = patch;
    const rest: Partial<Settings> = more;
    if (thinkNow !== undefined) {
      const next = { ...local.get<Record<string, boolean>>("chat.think", {}), [thinkKey(zone)]: thinkNow };
      setThinkBy(next);
      local.set("chat.think", next);
    }
    if (system !== undefined) {
      if (conv) setOwn({ ...mine, id: conv.id, system });
      else rest.system = system;
    }
    if (Object.keys(rest).length) {
      const next = { ...saved, ...rest };
      setSaved(next);
      local.set("chat.settings", { ...next, v: 2 });
    }
  };

  const [draft, setDraft] = useState("");
  const composer = useRef<ComposerHandle>(null);
  const [listOpen, setListOpen] = useState(false);
  const [sending, setSending] = useState(false);
  const streaming = !!conv?.active;

  const singleModel = columns.length === 1 ? (mine.model ?? columns[0].leaf?.model ?? columns[0].model) : undefined;

  const systemText = (s: Pick<Settings, "system">, leaf?: Job): string | undefined => {
    if (s.system === "none") return undefined;
    if (s.system === "keep") return leaf ? systemOf(leaf) : undefined;
    return chatPrompts.find((p) => p.id === s.system)?.text;
  };

  // unset fields take the model's defaults (the keys stay, undefined, so they replace a regenerated request's)
  const optsOf = (s: Pick<Settings, "temperature" | "maxTokens" | "think">): ChatOpts => ({
    temperature: s.temperature ? s.temperature : undefined,
    greedy: s.temperature === 0 ? true : undefined,
    maxTokens: s.maxTokens ? Math.max(16, s.maxTokens) : undefined,
    think: s.think || undefined,
  });
  const opts = () => optsOf(settings);

  // ---------- the queue: messages typed while an answer runs ----------
  const queued = useQueued(conv?.id);
  const updateQueue = useQueue((s) => s.update);
  const [editingQueued, setEditingQueued] = useState<string | null>(null);
  const leafIds = (cols: Column[]) => cols.flatMap((c) => (c.leaf ? [c.leaf.id] : []));

  const queueDraft = () => {
    const text = draft.trim();
    if (!conv || !text) return;
    const config: SendConfig = { model: columns.length === 1 ? singleModel : undefined, system: settings.system, temperature: settings.temperature, maxTokens: settings.maxTokens, think: settings.think };
    const running = conv.jobs.filter(isActive).map((j) => j.id);
    updateQueue(conv.id, (q) => enqueue(q, { id: queueId(), text }, running, config));
    setDraft("");
    stick.current = true;
  };
  /** Stops the running answer and sends this message next. */
  const sendQueuedNow = async (id: string) => {
    if (!conv) return;
    const active = conv.jobs.filter(isActive).map((j) => j.id);
    updateQueue(conv.id, (q) => queueFirst(q, id, active, leafIds(columns)));
    for (const j of active) await api.cancel(j).catch((e) => toast.error("Could not stop", { description: errorText(e) }));
  };
  const continueQueue = () => conv && updateQueue(conv.id, (q) => resume(q, leafIds(columns)));

  // sends the next queued message of each conversation when its turn ended well (pauses when it did not)
  const queues = useQueue((s) => s.queues);
  const toolJobs = useTools((s) => s.jobs);
  const busy = useRef(new Set<string>());
  useEffect(() => {
    for (const [cid, q] of Object.entries(queues)) {
      if (busy.current.has(cid)) continue;
      const c = threads.byJob.get(cid);
      if (!c) continue; // not loaded yet, or deleted
      if (fromLoad(cid)) {
        const r = restore(q, (id) => jobs[id]);
        if (r !== q) {
          updateQueue(cid, (x) => restore(x, (id) => jobs[id]));
          continue;
        }
      }
      const cols = columnsOf(c, threads, picks);
      const leaves = cols.flatMap((x) => (x.leaf ? [x.leaf] : []));
      const step = nextStep(q, { active: c.active, pending: pendingOf(c.jobs, toolJobs), leaves, after: q.after.map((id) => jobs[id]) });
      if (step.do === "pause") updateQueue(cid, (x) => pause(x, step.reason));
      if (step.do !== "send") continue;
      let item: { id: string; text: string } | undefined;
      const cfg = updateQueue(cid, (x) => {
        const [it, next] = claim(x, Date.now());
        item = it;
        return next;
      }).config;
      if (!item) continue; // another tab is sending it
      busy.current.add(cid);
      const s = cfg ?? { ...saved, think: thinkFor(c.root.request.zone ?? "", local.get<Record<string, boolean>>("chat.think", {}), saved.think) };
      followUp({
        columns: cols,
        threads,
        zone: c.root.request.zone ?? "",
        text: item.text,
        model: cols.length === 1 ? cfg?.model : undefined,
        system: (leaf) => systemText(cfg ?? { system: "keep" }, leaf),
        opts: optsOf(s),
        put,
      })
        .then((made) => updateQueue(cid, (x) => sent(x, made.map((j) => j.id))))
        .catch((e) => {
          toast.error("Could not send a queued message", { description: errorText(e) });
          updateQueue(cid, (x) => unsent(x));
        })
        .finally(() => busy.current.delete(cid));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [queues, threads, picks, toolJobs, jobs]);

  const send = async () => {
    const text = draft.trim();
    if (!text || sending) return;
    if (conv && streaming) return queueDraft();
    setSending(true);
    try {
      if (!conv) {
        const sys = systemText(settings);
        const messages: Message[] = [...(sys ? [{ Role: "system" as const, Content: sys }] : []), { Role: "user", Content: text }];
        const made: Job[] = [];
        for (const model of choice.ids) {
          const j = await api.submit({ model, messages, ...(zone ? { zone } : {}), ...opts(), parent: made[0]?.id });
          put(j);
          made.push(j);
        }
        if (made.length > 1) addLinks(Object.fromEntries(made.slice(1).map((j) => [j.id, made[0].id])));
        setDraft("");
        if (made[0]) navigate("chat", made[0].id);
      } else {
        await followUp({ columns, threads, zone, text, model: singleModel, system: (leaf) => systemText(settings, leaf), opts: opts(), put });
        setDraft("");
      }
      stick.current = true;
    } catch (e) {
      startFailed(e, "Could not send");
    } finally {
      setSending(false);
    }
  };

  const stop = async () => {
    for (const j of conv?.jobs ?? []) if (isActive(j)) await api.cancel(j.id).catch((e) => toast.error("Could not stop", { description: errorText(e) }));
  };

  const regenerate = useCallback(
    async (j: Job) => {
      const p = threads.parent.get(j.id);
      const { title: _, ...req } = j.request;
      try {
        const n = await api.submit({ ...req, ...opts(), parent: p ?? j.id });
        put(n);
        setPicks((s) => ({ ...s, [p ?? `root:${conv?.id}:${j.model}`]: n.id }));
        if (!p && conv) addLinks({ [n.id]: conv.id });
      } catch (e) {
        startFailed(e, "Could not answer again");
      }
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [threads, conv?.id, settings.temperature, settings.maxTokens, settings.think],
  );
  const pick = useCallback((slot: string, id: string) => setPicks((s) => ({ ...s, [slot]: id })), []);

  const openConv = (c: Conversation) => {
    setListOpen(false);
    navigate("chat", c.id);
  };
  const newChat = () => {
    setListOpen(false);
    navigate("chat", "");
    setTimeout(() => composer.current?.focus(), 0);
  };
  const onExample = (e: Example) => {
    setDraft(e.prompt ?? e.question ?? "");
    composer.current?.focus();
  };

  // ---------- scrolling: follow the answer while at the bottom ----------
  const scroller = useRef<HTMLDivElement>(null);
  const content = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  const [away, setAway] = useState(false);
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    stick.current = true;
    setAway(false);
    const el = scroller.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [conv?.id]);
  useEffect(() => {
    const el = scroller.current;
    const inner = content.current;
    if (!el || !inner) return;
    const ro = new ResizeObserver(() => {
      setWidth(el.clientWidth);
      if (stick.current) el.scrollTop = el.scrollHeight;
    });
    ro.observe(el);
    ro.observe(inner);
    return () => ro.disconnect();
  }, []);
  const onScroll = () => {
    const el = scroller.current!;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 48;
    stick.current = atBottom;
    setAway(!atBottom);
  };
  const toBottom = () => {
    const el = scroller.current;
    stick.current = true;
    el?.scrollTo({ top: el.scrollHeight, behavior: "smooth" });
  };

  // ---------- compare: columns when there is room, else tabs ----------
  const wide = columns.length > 1 && width >= columns.length * 340;
  const [tab, setTab] = useState("");
  const shownColumns = columns.length > 1 && !wide ? [columns.find((c) => c.key === tab) ?? columns[0]] : columns;

  const models = useApp((s) => s.models);
  const modelName = (id: string) => models.find((m) => m.id === id)?.name ?? id;

  const picker = conv ? (
    columns.length > 1 ? (
      <span className="flex h-7 items-center gap-1.5 rounded-md px-2 text-[0.8rem] text-muted-foreground" title={columns.map((c) => modelName(c.model)).join(", ")}>
        <span className="size-2 rounded-full bg-emerald-500" /> {columns.length} models
      </span>
    ) : (
      <ModelPicker
        multiple={false}
        className="h-7 max-w-36 sm:max-w-64 border-0 bg-transparent px-2 text-[0.8rem] text-muted-foreground shadow-none hover:text-foreground dark:bg-transparent"
        choice={{
          models: choice.models,
          ids: singleModel ? [singleModel] : [],
          set: (ids: string[] | ((p: string[]) => string[])) => {
            const next = typeof ids === "function" ? ids(singleModel ? [singleModel] : []) : ids;
            if (conv && next[0]) setOwn({ ...mine, id: conv.id, model: next[0] });
          },
          primary: choice.models.find((m) => m.id === singleModel),
        }}
      />
    )
  ) : (
    <ModelPicker choice={choice} className="h-7 max-w-36 sm:max-w-64 border-0 bg-transparent px-2 text-[0.8rem] text-muted-foreground shadow-none hover:text-foreground dark:bg-transparent" />
  );

  const list = (
    <ConversationList
      conversations={threads.conversations}
      current={conv?.id}
      onOpen={openConv}
      onNew={newChat}
      onDeleted={(c) => c.id === conv?.id && navigate("chat", "")}
    />
  );

  return (
    <div className="flex h-full min-h-0">
      <aside className="hidden w-64 shrink-0 flex-col border-r bg-card/30 md:flex">{list}</aside>
      <Sheet open={listOpen} onOpenChange={setListOpen}>
        <SheetContent side="left" className="w-[85vw] max-w-80 gap-0 p-0 pt-10" showCloseButton>
          <SheetTitle className="sr-only">Chats</SheetTitle>
          {list}
        </SheetContent>
      </Sheet>

      <section className="relative flex min-w-0 flex-1 flex-col">
        <Header
          conv={conv}
          zone={zone}
          columns={columns}
          wide={wide}
          tab={shownColumns[0]?.key}
          setTab={setTab}
          modelName={modelName}
          copyId={shownColumns[0]?.leaf?.id ?? conv?.id}
          onList={() => setListOpen(true)}
          onNew={newChat}
          onDeleted={() => navigate("chat", "")}
        />
        <div ref={scroller} onScroll={onScroll} className="min-h-0 flex-1 overflow-y-auto overscroll-contain">
          <div ref={content} className={cn("mx-auto w-full px-4 pt-6 pb-10 md:px-6", wide ? "max-w-[min(100%,110rem)]" : "max-w-3xl")}>
            {conv ? (
              <>
                <Transcript columns={shownColumns} wide={wide} onRegenerate={regenerate} onPick={pick} modelName={modelName} />
                <div className={cn("mt-5", wide && "mx-auto max-w-3xl")}>
                  <QueueView conv={conv.id} editing={editingQueued} setEditing={setEditingQueued} onSendNow={sendQueuedNow} onContinue={continueQueue} />
                </div>
              </>
            ) : (
              <Welcome zone={newZone} onZone={setNewZone} onExample={onExample} model={choice.ids.length > 1 ? `${choice.ids.length} models side by side` : modelName(choice.ids[0] ?? "")} missing={arg !== "" && Object.keys(jobs).length > 0} />
            )}
          </div>
        </div>
        {away && (
          <Button variant="outline" size="icon" className="absolute bottom-36 left-1/2 z-10 size-8 -translate-x-1/2 rounded-full bg-background shadow-md" onClick={toBottom} title="Scroll to the end">
            <ArrowDown />
          </Button>
        )}
        <div className="px-3 pb-3 md:px-6 md:pb-4">
          <div className={cn("mx-auto w-full", wide ? "max-w-4xl" : "max-w-3xl")}>
            <Composer
              ref={composer}
              value={draft}
              onChange={setDraft}
              onSend={send}
              onStop={stop}
              streaming={streaming}
              canSend={!sending && (conv ? true : choice.ids.length > 0)}
              queueing={!!conv}
              queued={queued}
              onEditLast={() => {
                const q = conv && useQueue.getState().queues[conv.id];
                const last = q?.items.at(-1);
                if (last) setEditingQueued(last.id);
              }}
              models={picker}
              tools={<ZonePicker value={zone} onChange={setNewZone} locked={!!conv} onNewChat={newChat} />}
              settings={settings}
              sampling={(choice.models.find((m) => m.id === singleModel) ?? choice.primary)?.sampling}
              setSettings={setSettings}
              prompts={chatPrompts}
              keepText={conv ? keepText : undefined}
              placeholder={
                conv
                  ? columns.length > 1
                    ? `Reply to all ${columns.length} models`
                    : "Reply"
                  : choice.ids.length > 1
                    ? `Ask ${choice.ids.length} models at once`
                    : (placeholders[zone] ?? "Ask anything")
              }
            />
            <p className="mt-1.5 hidden text-center text-[11px] text-muted-foreground/70 md:block">
              {conv && streaming ? "Enter to queue · sent when the answer is done · ↑ edits the last queued" : "Enter to send · Shift+Enter for a new line · runs on your Mac"}
            </p>
          </div>
        </div>
      </section>
    </div>
  );
}

const placeholders: Partial<Record<Zone, string>> = {
  home: "Ask about your home or your vboard",
  web: "Ask anything; it can search and read the web",
  files: "Describe the document, report or chart to make",
};

function Header({
  conv,
  zone,
  columns,
  wide,
  tab,
  setTab,
  modelName,
  copyId,
  onList,
  onNew,
  onDeleted,
}: {
  conv?: Conversation;
  zone: Zone;
  columns: Column[];
  wide: boolean;
  tab?: string;
  setTab: (k: string) => void;
  modelName: (id: string) => string;
  /** The turn whose branch "Copy chat" copies (the one shown). */
  copyId?: string;
  onList: () => void;
  onNew: () => void;
  onDeleted: () => void;
}) {
  const [renaming, setRenaming] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const compareTabs = columns.length > 1 && !wide;
  return (
    <>
      <header className="flex h-11 shrink-0 items-center gap-1 border-b px-2 md:px-4">
        <Button variant="ghost" size="icon-sm" className="md:hidden" onClick={onList} title="Chats">
          <PanelLeft />
        </Button>
        <h2 className="min-w-0 truncate px-1 text-sm font-medium">{conv ? conv.title : "New chat"}</h2>
        {conv && <ZoneBadge zone={zone} />}
        {conv && <Pending conv={conv} />}
        <span className="flex-1" />
        {conv && copyId && (
          <Button variant="ghost" size="icon-sm" className="text-muted-foreground max-sm:hidden" title="Copy chat (as text, for a closer look)" onClick={() => copyChat(copyId)}>
            <Copy />
          </Button>
        )}
        {conv && (
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon-sm" className="text-muted-foreground" title="More">
                <MoreHorizontal />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-48">
              <DropdownMenuItem onSelect={() => setRenaming(true)}>
                <Pencil /> Rename
              </DropdownMenuItem>
              {copyId && (
                <>
                  <DropdownMenuItem onSelect={() => copyChat(copyId)}>
                    <Copy /> Copy chat
                  </DropdownMenuItem>
                  <DropdownMenuItem onSelect={() => copyChat(copyId, { thinking: true })}>
                    <Brain /> Copy with thinking
                  </DropdownMenuItem>
                </>
              )}
              <DropdownMenuSeparator />
              <DropdownMenuItem variant="destructive" onSelect={() => setDeleting(true)}>
                <Trash2 /> Delete
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        )}
        <Button variant="ghost" size="icon-sm" className="text-muted-foreground md:hidden" onClick={onNew} title="New chat">
          <MessageSquarePlus />
        </Button>
        {conv && <RenameDialog key={conv.id + renaming} conv={conv} open={renaming} onOpenChange={setRenaming} />}
        <DeleteConversation c={deleting && conv ? conv : null} onClose={() => setDeleting(false)} onDeleted={onDeleted} />
      </header>
      {compareTabs && (
        <div className="flex shrink-0 justify-center border-b px-3 py-2">
          <div className="flex max-w-full gap-0.5 overflow-x-auto rounded-lg bg-muted p-0.5">
            {columns.map((c) => (
              <button
                key={c.key}
                onClick={() => setTab(c.key)}
                className={cn(
                  "min-w-0 truncate rounded-md px-3 py-1 text-xs whitespace-nowrap text-muted-foreground transition-colors",
                  c.key === tab && "bg-background text-foreground shadow-sm",
                )}
              >
                {modelName(c.model)}
              </button>
            ))}
          </div>
        </div>
      )}
    </>
  );
}

/** Calls of this chat that wait for the user. */
function Pending({ conv }: { conv: Conversation }) {
  const n = useTools((s) => pendingOf(conv.jobs, s.jobs));
  if (!n) return null;
  return (
    <span className="ml-1 inline-flex h-5 shrink-0 items-center gap-1 rounded-full bg-primary/10 px-1.5 text-[11px] font-medium text-primary">
      <span className="size-1.5 animate-pulse rounded-full bg-primary" />
      {n === 1 ? "Needs your OK" : `${n} need your OK`}
    </span>
  );
}

function RenameDialog({ conv, open, onOpenChange }: { conv: Conversation; open: boolean; onOpenChange: (o: boolean) => void }) {
  const [name, setName] = useState(conv.title);
  const put = useApp((s) => s.put);
  const save = async () => {
    onOpenChange(false);
    const t = name.trim();
    if (!t || t === conv.title) return;
    try {
      put(await api.rename(conv.root.id, t));
    } catch (e) {
      toast.error("Could not rename", { description: errorText(e) });
    }
  };
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Rename chat</DialogTitle>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            save();
          }}
        >
          <Input autoFocus value={name} onChange={(e) => setName(e.target.value)} />
          <DialogFooter className="mt-4">
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit">Save</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function Transcript({
  columns,
  wide,
  onRegenerate,
  onPick,
  modelName,
}: {
  columns: Column[];
  wide: boolean;
  onRegenerate: (j: Job) => void;
  onPick: (slot: string, id: string) => void;
  modelName: (id: string) => string;
}) {
  const rows = Math.max(0, ...columns.map((c) => c.turns.length));
  const single = columns.length === 1;
  // a model change within the chat is worth naming under its answers
  const mixed = single && new Set(columns[0].turns.map((t) => t.job?.model).filter(Boolean)).size > 1;
  return (
    <div className="flex flex-col gap-7">
      {wide && (
        <div
          className="sticky top-0 z-10 -mx-4 -mt-6 -mb-3 grid gap-4 border-b bg-background/85 px-4 py-2.5 backdrop-blur md:-mx-6 md:px-6"
          style={{ gridTemplateColumns: `repeat(${columns.length}, minmax(0, 1fr))` }}
        >
          {columns.map((c) => (
            <div key={c.key} className="flex items-center gap-2 truncate text-xs font-medium text-muted-foreground">
              <span className="size-1.5 rounded-full bg-primary/60" /> {modelName(c.model)}
            </div>
          ))}
        </div>
      )}
      {Array.from({ length: rows }, (_, i) => {
        const cells = columns.map((c) => c.turns[i]);
        const same = cells.every((t) => t?.question === cells[0]?.question);
        return (
          <div key={i} className="flex flex-col gap-4">
            {same && cells[0] && (
              <div className={cn(wide && "pl-[20%]")}>
                <Question text={cells[0].question} />
              </div>
            )}
            <div className={cn(wide ? "grid gap-4" : "flex flex-col gap-4")} style={wide ? { gridTemplateColumns: `repeat(${columns.length}, minmax(0, 1fr))` } : undefined}>
              {cells.map((t, k) => (
                <div key={columns[k].key} className={cn("min-w-0", wide && "rounded-xl border bg-card/40 p-4")}>
                  {t && (
                    <div className="flex flex-col gap-3">
                      {!same && <Question text={t.question} />}
                      {t.job ? (
                        <Answer
                          job={t.job}
                          variants={t.variants}
                          onPick={(id) => t.slot && onPick(t.slot, id)}
                          onRegenerate={onRegenerate}
                          showModel={!wide && (mixed || !single || i === 0)}
                        />
                      ) : (
                        <StaticAnswer text={t.answer ?? ""} />
                      )}
                    </div>
                  )}
                </div>
              ))}
            </div>
          </div>
        );
      })}
    </div>
  );
}

function Welcome({
  zone,
  onZone,
  onExample,
  model,
  missing,
}: {
  zone: Zone;
  onZone: (z: Zone) => void;
  onExample: (e: Example) => void;
  model: string;
  missing: boolean;
}) {
  const examples = useApp((s) => s.examples.chat) ?? [];
  return (
    <div className="flex min-h-[50vh] flex-col items-center justify-center gap-6 py-6 text-center">
      <div className="flex flex-col items-center gap-3">
        <div className="grid size-12 place-items-center rounded-2xl bg-primary/10 text-primary">
          <MessagesSquare className="size-6" />
        </div>
        <h2 className="text-xl font-semibold tracking-tight">{missing ? "This chat is gone" : "What can I help with?"}</h2>
        <p className="max-w-md text-sm text-muted-foreground">
          {missing ? "It was deleted, or it is still loading. Start a new one below." : <>Ask {model || "a model"} anything. Tick more models in the picker to compare their answers side by side.</>}
        </p>
      </div>
      <ZoneCards value={zone} onChange={onZone} />
      {zone && (
        <p className="-mt-3 max-w-xl text-xs text-muted-foreground">
          {zoneInfo(zone).name} tools stay with this chat: it can't switch to other tools later, so what one zone sees never reaches another.
          {zone === "home" && " Anything that changes something waits for your OK."}
        </p>
      )}
      {examples.length > 0 && (
        <div className="grid w-full max-w-2xl grid-cols-1 gap-2 sm:grid-cols-2">
          {examples.slice(0, 4).map((e) => (
            <button
              key={e.title}
              onClick={() => onExample(e)}
              className="flex flex-col gap-0.5 rounded-xl border bg-card/40 px-3.5 py-2.5 text-left transition-colors hover:bg-accent"
            >
              <span className="text-sm font-medium">{e.title}</span>
              <span className="line-clamp-1 text-xs text-muted-foreground">{e.prompt ?? e.question}</span>
            </button>
          ))}
        </div>
      )}
      {examples.length > 4 && <ExamplesMenu kind="chat" onPick={onExample} />}
    </div>
  );
}
