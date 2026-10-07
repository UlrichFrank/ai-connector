import { ArrowLeft, Check, ChevronDown, Copy, CopyPlus, ExternalLink, FlaskConical, Loader2, Plus, RotateCcw, ScrollText, Trash2 } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Prompt, PromptUse } from "@/api/types";
import { ErrorNotice } from "@/components/connection/ErrorNotice";
import { Empty, Field } from "@/components/kit/Layout";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import { ago } from "@/lib/format";
import { helperModel, runChat } from "@/lib/jobs";
import { navigate } from "@/lib/route";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";
import { diffLines } from "./diff";

/** What each use is for, in the order the server lists them. */
const uses: { use: PromptUse; title: string; about: string; sample?: string }[] = [
  { use: "chat", title: "Chat", about: "The system prompt a chat starts with: who the assistant is and how it answers." },
  { use: "enhance-image", title: "Enhance · image", about: "✨ Enhance on the Image page: turns a short idea into a detailed image prompt.", sample: "a fox in the snow" },
  { use: "enhance-video", title: "Enhance · video", about: "✨ Enhance on the Video page: turns an idea into a scene the video model can film.", sample: "waves crashing on black rocks at sunset" },
  { use: "enhance-music", title: "Enhance · music", about: "✨ Enhance for a song's style: turns an idea into style tags.", sample: "a chill summer evening song" },
  { use: "write-song", title: "Write a song", about: "“Song from an idea” on the Music page: writes a title, a style and the lyrics.", sample: "a short song about a Mac making pictures at night" },
  {
    use: "cover-art",
    title: "Cover art",
    about: "Writes the image prompt for a song's cover from its title, style and lyrics.",
    sample: "Title: Night Shift\nStyle: synthwave, 110 bpm, dreamy female vocal\nLyrics: neon on the window, the fans hum low, pictures keep coming while the city sleeps",
  },
];
const useInfo = (u: PromptUse) => uses.find((x) => x.use === u) ?? { use: u, title: u, about: "" };

interface Draft {
  name: string;
  text: string;
  source: string;
  use: PromptUse;
}
const draftOf = (p: Prompt): Draft => ({ name: p.name, text: p.text, source: p.source ?? "", use: p.use });
const same = (a: Draft, b: Draft) => a.name === b.name && a.text === b.text && a.source === b.source && a.use === b.use;

/** Settings → Prompts: the system prompts, by what they are for; edit, add your own, try one. */
export function PromptsPage({ arg }: { arg: string }) {
  const prompts = useApp((s) => s.prompts);
  const [error, setError] = useState<unknown>(null);
  const [loaded, setLoaded] = useState(false);
  /** Unsaved edits by prompt id ("new" for a new one), so moving between prompts keeps them. */
  const [drafts, setDrafts] = useState<Record<string, Draft>>({});

  // our own load (the store's refresh keeps quiet about errors), into the store
  const load = useCallback(async () => {
    try {
      useApp.setState({ prompts: await api.prompts() });
      setError(null);
    } catch (e) {
      setError(e);
    } finally {
      setLoaded(true);
    }
  }, []);
  useEffect(() => void load(), [load]);

  const isNew = arg.startsWith("new");
  const selected = isNew ? undefined : (prompts.find((p) => p.id === arg) ?? (arg ? undefined : prompts[0]));
  const editorOpen = !!arg; // on a phone: the list, or one prompt

  const setDraft = (id: string, d: Draft | null) =>
    setDrafts((all) => {
      const { [id]: _, ...rest } = all;
      return d ? { ...rest, [id]: d } : rest;
    });

  const startNew = (use: PromptUse) => {
    setDraft("new", { name: "", text: "", source: "", use });
    navigate("prompts", "new");
  };

  if (!loaded && !prompts.length) return <PageSkeleton />;
  if (error != null && !prompts.length)
    return (
      <div className="mx-auto max-w-2xl p-6">
        <ErrorNotice error={error} onRetry={load} />
      </div>
    );

  return (
    <div className="flex h-full min-h-0">
      <aside className={cn("flex w-80 shrink-0 flex-col border-r bg-card/30 max-lg:w-full max-lg:border-r-0", editorOpen && "max-lg:hidden")}>
        <div className="flex items-center gap-2 border-b px-4 py-3">
          <div className="min-w-0 flex-1">
            <h1 className="text-sm font-semibold">System prompts</h1>
            <p className="text-xs text-muted-foreground">Shared by the web app, the TUI and the phone.</p>
          </div>
          <NewMenu onPick={startNew} />
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto px-2 py-3">
          {error != null && <ErrorNotice error={error} onRetry={load} className="mx-2 mb-3" />}
          {uses.map(({ use, title, about }) => {
            const list = prompts.filter((p) => p.use === use);
            return (
              <section key={use} className="mb-4">
                <div className="px-2 pb-1">
                  <h2 className="text-[11px] font-semibold tracking-wide text-muted-foreground uppercase">{title}</h2>
                  <p className="text-xs text-muted-foreground/80">{about}</p>
                </div>
                <ul className="flex flex-col gap-0.5">
                  {list.map((p) => (
                    <PromptItem key={p.id} prompt={p} on={p.id === selected?.id} implicit={!arg} dirty={!!drafts[p.id] && !same(drafts[p.id], draftOf(p))} />
                  ))}
                  {drafts.new?.use === use && (
                    <li>
                      <button type="button" onClick={() => navigate("prompts", "new")} className={cn("w-full rounded-md px-2 py-1.5 text-left text-sm italic text-muted-foreground hover:bg-accent/60", isNew && "bg-accent")}>
                        {drafts.new.name || "New prompt"} · unsaved
                      </button>
                    </li>
                  )}
                </ul>
              </section>
            );
          })}
        </div>
      </aside>
      <section className={cn("min-w-0 flex-1 overflow-y-auto", !editorOpen && "max-lg:hidden")}>
        {isNew && drafts.new ? (
          <Editor key="new" draft={drafts.new} setDraft={(d) => setDraft("new", d)} />
        ) : selected ? (
          <Editor key={selected.id} prompt={selected} draft={drafts[selected.id]} setDraft={(d) => setDraft(selected.id, d)} />
        ) : (
          <div className="p-6">
            <Empty icon={<ScrollText />} title={arg ? "This prompt is gone" : "No prompts"}>
              {arg ? "It was deleted. Pick another one from the list." : "Make one with New."}
            </Empty>
          </div>
        )}
      </section>
    </div>
  );
}

function NewMenu({ onPick }: { onPick: (u: PromptUse) => void }) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button size="sm" variant="outline">
          <Plus /> New <ChevronDown className="opacity-60" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-64">
        {uses.map((u) => (
          <DropdownMenuItem key={u.use} onSelect={() => onPick(u.use)} className="flex flex-col items-start gap-0.5">
            <span className="font-medium">{u.title}</span>
            <span className="line-clamp-2 text-xs text-muted-foreground">{u.about}</span>
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function PromptItem({ prompt: p, on, implicit, dirty }: { prompt: Prompt; on: boolean; implicit: boolean; dirty: boolean }) {
  return (
    <li>
      <button
        type="button"
        onClick={() => navigate("prompts", p.id)}
        aria-current={on || undefined}
        className={cn("flex w-full flex-col gap-0.5 rounded-md px-2 py-1.5 text-left transition-colors hover:bg-accent/60", on && "bg-accent", on && implicit && "max-lg:bg-transparent")}
      >
        <span className="flex items-center gap-1.5">
          <span className="truncate text-sm font-medium">{p.name}</span>
          {dirty && <span className="size-1.5 shrink-0 rounded-full bg-primary" title="Unsaved changes" />}
          <span className="flex-1" />
          {!p.builtin && <Tag>yours</Tag>}
          {p.modified && <Tag tone="amber">modified</Tag>}
        </span>
        <span className="line-clamp-1 text-xs text-muted-foreground">{p.text}</span>
      </button>
    </li>
  );
}

function Tag({ children, tone }: { children: React.ReactNode; tone?: "amber" }) {
  return (
    <span className={cn("shrink-0 rounded px-1.5 py-px text-[10px] font-medium", tone === "amber" ? "bg-amber-500/15 text-amber-700 dark:text-amber-300" : "bg-muted text-muted-foreground")}>{children}</span>
  );
}

function Editor({ prompt: p, draft, setDraft }: { prompt?: Prompt; draft?: Draft; setDraft: (d: Draft | null) => void }) {
  const saved: Draft = p ? draftOf(p) : { name: "", text: "", source: "", use: draft?.use ?? "chat" };
  const d = draft ?? saved;
  const dirty = !p || !same(d, saved);
  const [busy, setBusy] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const refresh = useApp((s) => s.refreshPrompts);
  const edit = (patch: Partial<Draft>) => setDraft({ ...d, ...patch });

  const save = async () => {
    if (!d.name.trim()) return toast.error("Give the prompt a name");
    if (!d.text.trim()) return toast.error("The prompt is empty");
    setBusy(true);
    try {
      const out = await api.savePrompt({ id: p?.id, name: d.name, text: d.text, use: d.use, source: d.source.trim() || undefined });
      await refresh();
      setDraft(null);
      toast.success(p ? "Saved" : "Prompt added");
      if (!p) navigate("prompts", out.id);
    } catch (e) {
      toast.error("Could not save", { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };

  const duplicate = async () => {
    setBusy(true);
    try {
      const out = await api.savePrompt({ name: `${d.name} (copy)`, text: d.text, use: d.use, source: d.source.trim() || undefined });
      await refresh();
      toast.success("Duplicated", { description: "Your copy can be changed freely." });
      navigate("prompts", out.id);
    } catch (e) {
      toast.error("Could not duplicate", { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!p) return setDraft(null), navigate("prompts");
    try {
      await api.deletePrompt(p.id);
      await refresh();
      setDraft(null);
      if (p.builtin) toast.success("Back to the default");
      else {
        toast.success(`“${p.name}” deleted`);
        navigate("prompts");
      }
    } catch (e) {
      toast.error(p.builtin ? "Could not reset" : "Could not delete", { description: errorText(e) });
    }
  };

  // ⌘S / Ctrl+S
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "s") {
        e.preventDefault();
        if (dirty && !busy) save();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  const info = useInfo(d.use);
  const isUrl = /^https?:\/\//.test(d.source.trim());
  const changedFromDefault = !!p?.builtin && p.default != null && (d.text !== p.default || d.name !== p.name);

  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-5 px-4 py-5 sm:px-6">
      <div className="flex flex-wrap items-center gap-2">
        <Button variant="ghost" size="sm" className="-ml-2 lg:hidden" onClick={() => navigate("prompts")}>
          <ArrowLeft /> Prompts
        </Button>
        <Badge variant="secondary" className="font-normal">
          {info.title}
        </Badge>
        {p ? (
          p.builtin ? (
            <Badge variant="outline" className="font-normal text-muted-foreground">
              built-in
            </Badge>
          ) : (
            <Badge variant="outline" className="font-normal text-muted-foreground">
              yours
            </Badge>
          )
        ) : (
          <Badge variant="outline" className="font-normal text-muted-foreground">
            new · unsaved
          </Badge>
        )}
        {p?.modified && <Tag tone="amber">modified</Tag>}
        {p?.updated && <span className="text-xs text-muted-foreground">saved {ago(p.updated)}</span>}
        <div className="flex-1" />
        {p && (
          <Button variant="ghost" size="sm" disabled={busy} onClick={duplicate} title="Make your own copy">
            <CopyPlus /> Duplicate
          </Button>
        )}
        {(!p || !p.builtin) && (
          <Button variant="ghost" size="sm" className="text-muted-foreground hover:text-destructive" onClick={() => (p ? setConfirm(true) : remove())}>
            <Trash2 /> {p ? "Delete" : "Discard"}
          </Button>
        )}
        {p?.builtin && p.modified && (
          <Button variant="ghost" size="sm" className="text-muted-foreground" onClick={() => setConfirm(true)}>
            <RotateCcw /> Reset to default
          </Button>
        )}
      </div>

      <Field label="Name">
        <Input value={d.name} onChange={(e) => edit({ name: e.target.value })} placeholder="What it does, in a few words" className="h-10 text-base font-medium" />
      </Field>

      {p && !p.builtin && (
        <Field label="Use" hint={info.about}>
          <Select value={d.use} onValueChange={(v) => edit({ use: v as PromptUse })}>
            <SelectTrigger className="w-full sm:w-64">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {uses.map((u) => (
                <SelectItem key={u.use} value={u.use}>
                  {u.title}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
      )}
      {(!p || p.builtin) && <p className="-mt-2 text-sm text-muted-foreground">{info.about}</p>}

      <Field label="System prompt" actions={<span className="text-xs text-muted-foreground tabular-nums">{d.text.length} characters</span>}>
        <Textarea
          value={d.text}
          onChange={(e) => edit({ text: e.target.value })}
          spellCheck={false}
          placeholder="You are …"
          className="min-h-80 resize-y font-mono text-[13px] leading-relaxed"
        />
      </Field>

      {changedFromDefault && <DefaultDiff from={p!.default!} to={d.text} saved={p!.modified} />}

      <Field label="Source" hint={p?.builtin ? "Built-in prompts keep their source." : "Where it comes from: a URL, a model card, a note."}>
        <div className="flex items-center gap-2">
          <Input value={d.source} onChange={(e) => edit({ source: e.target.value })} disabled={p?.builtin} placeholder={p?.builtin ? "mlxlab" : "https://…"} />
          {isUrl && (
            <Button variant="ghost" size="icon" asChild title="Open the source">
              <a href={d.source.trim()} target="_blank" rel="noreferrer">
                <ExternalLink />
              </a>
            </Button>
          )}
        </div>
      </Field>

      <div className="sticky bottom-0 -mx-4 flex items-center gap-2 border-t bg-background/90 px-4 py-3 backdrop-blur sm:-mx-6 sm:px-6">
        <span className="text-xs text-muted-foreground">{dirty ? (p ? "Unsaved changes" : "Not saved yet") : "All changes saved"}</span>
        <div className="flex-1" />
        {p && dirty && (
          <Button variant="ghost" onClick={() => setDraft(null)} disabled={busy}>
            Discard
          </Button>
        )}
        <Button onClick={save} disabled={!dirty || busy}>
          {busy ? <Loader2 className="animate-spin" /> : <Check />} Save
        </Button>
      </div>

      {d.use !== "chat" && <TryIt key={d.use} system={d.text} sample={info.sample ?? ""} />}

      <AlertDialog open={confirm} onOpenChange={setConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{p?.builtin ? `Reset “${p.name}” to its default?` : `Delete “${p?.name}”?`}</AlertDialogTitle>
            <AlertDialogDescription>{p?.builtin ? "Your changes to it are lost." : "This cannot be undone."}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction variant={p?.builtin ? "default" : "destructive"} onClick={remove}>
              {p?.builtin ? "Reset" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

/** What differs from the built-in default, line by line (sentence by sentence for one-paragraph prompts). */
function DefaultDiff({ from, to, saved }: { from: string; to: string; saved: boolean }) {
  const [open, setOpen] = useState(false);
  const lines = useMemo(() => (open ? diffLines(from, to) : []), [open, from, to]);
  return (
    <div className="rounded-lg border">
      <button type="button" onClick={() => setOpen(!open)} className="flex w-full items-center gap-2 px-3 py-2.5 text-left text-sm">
        <ChevronDown className={cn("size-4 transition-transform", !open && "-rotate-90")} />
        <span className="font-medium">Changes from the default</span>
        <span className="ml-auto text-xs text-muted-foreground max-sm:hidden">{saved ? "Reset to default brings it back" : "Discard to undo"}</span>
      </button>
      {open && (
        <div className="max-h-96 overflow-y-auto border-t py-2 font-mono text-xs leading-relaxed">
          {lines.map((l, i) => (
            <div
              key={i}
              className={cn(
                "flex gap-2 px-3 whitespace-pre-wrap",
                l.op === "del" && "bg-destructive/10 text-red-700 line-through decoration-red-500/40 dark:text-red-300",
                l.op === "add" && "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
                l.op === "same" && "text-muted-foreground",
              )}
            >
              <span className="w-3 shrink-0 select-none opacity-70">{l.op === "del" ? "−" : l.op === "add" ? "+" : " "}</span>
              <span className="min-w-0">{l.text || " "}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

/** Runs the prompt as it is in the editor (saved or not) on a sample, with the helper chat model. */
function TryIt({ system, sample }: { system: string; sample: string }) {
  const [input, setInput] = useState(sample);
  const [out, setOut] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  const modelId = helperModel();
  const model = useApp((s) => s.models.find((m) => m.id === modelId));

  const run = async () => {
    if (!modelId || !input.trim() || !system.trim()) return;
    setBusy(true);
    setOut(null);
    try {
      setOut(await runChat(modelId, system, input.trim()));
    } catch (e) {
      toast.error("The try failed", { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="flex flex-col gap-3 rounded-xl border bg-card/40 p-4">
      <div className="flex flex-wrap items-baseline gap-x-2">
        <h2 className="flex items-center gap-1.5 text-sm font-semibold">
          <FlaskConical className="size-4 text-primary" /> Try it
        </h2>
        <span className="text-xs text-muted-foreground">
          {model ? `with ${model.name}${model.here ? "" : " (downloads first)"}` : "no chat model"} · uses the text above, saved or not
        </span>
      </div>
      <Textarea value={input} onChange={(e) => setInput(e.target.value)} rows={2} className="min-h-0 resize-y" placeholder="An idea to try the prompt on" />
      <div className="flex items-center gap-2">
        <Button variant="outline" size="sm" onClick={run} disabled={busy || !modelId || !input.trim() || !system.trim()}>
          {busy ? <Loader2 className="animate-spin" /> : <FlaskConical />} {busy ? "Running …" : "Run"}
        </Button>
        {busy && <span className="text-xs text-muted-foreground">It shows in Activity too.</span>}
      </div>
      {out != null && (
        <div className="relative rounded-lg border bg-background/60 p-3 pr-10 text-sm whitespace-pre-wrap">
          {out || <span className="text-muted-foreground">(no answer)</span>}
          <Button
            variant="ghost"
            size="icon-sm"
            className="absolute top-1.5 right-1.5 text-muted-foreground"
            title="Copy"
            onClick={() => {
              navigator.clipboard.writeText(out);
              setCopied(true);
              setTimeout(() => setCopied(false), 1500);
            }}
          >
            {copied ? <Check /> : <Copy />}
          </Button>
        </div>
      )}
    </section>
  );
}

function PageSkeleton() {
  return (
    <div className="flex h-full">
      <div className="flex w-80 flex-col gap-2 border-r p-4 max-lg:w-full">
        {[0, 1, 2, 3, 4, 5].map((i) => (
          <Skeleton key={i} className="h-10 w-full" />
        ))}
      </div>
      <div className="flex-1 p-6 max-lg:hidden">
        <Skeleton className="h-80 w-full" />
      </div>
    </div>
  );
}
