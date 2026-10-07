import { ArrowLeft, Check, Loader2, Mic, MoreHorizontal, Pencil, Plus, RotateCcw, ShieldCheck, Sparkles, Trash2, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Voice } from "@/api/types";
import { Empty } from "@/components/kit/Layout";
import { Field } from "@/components/kit/Layout";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Textarea } from "@/components/ui/textarea";
import { ago, duration } from "@/lib/format";
import { waitJob } from "@/lib/jobs";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";
import { PreviewButton } from "./Preview";
import { Recorder } from "./Recorder";
import { VoiceKind } from "./VoicePicker";
import { asrModel, designExamples, isAsr, languageName, languages, useVoiceLibrary, useVoices } from "./voices";

type View = "list" | "clone" | "design";

/**
 * The voice library as a sheet: the voices (listen, rename, delete, use), and adding one — cloned from a recording
 * of the speaker (with their consent), or designed from a description.
 */
export function VoiceLibrary({ open, onOpenChange, onUse, start }: { open: boolean; onOpenChange: (o: boolean) => void; onUse?: (v: Voice) => void; start?: View }) {
  const [view, setView] = useState<View>("list");
  useEffect(() => {
    if (open) setView(start ?? "list");
  }, [open, start]);
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="w-full gap-0 sm:max-w-md">
        <SheetHeader className="border-b pr-12">
          <div className="flex items-center gap-1">
            {view !== "list" && (
              <Button variant="ghost" size="icon-sm" className="-ml-1.5" title="Back to the voices" onClick={() => setView("list")}>
                <ArrowLeft />
              </Button>
            )}
            <SheetTitle>{view === "clone" ? "Clone a voice" : view === "design" ? "Design a voice" : "Voices"}</SheetTitle>
          </div>
          <SheetDescription>
            {view === "clone"
              ? "A short recording of one person; the speech models speak in that voice."
              : view === "design"
                ? "Describe a voice in words; the model makes it up."
                : "Your own voices for speech and avatars, next to the models' preset voices."}
          </SheetDescription>
        </SheetHeader>
        <div className="min-h-0 flex-1 overflow-y-auto p-4">
          {view === "list" && <VoiceList onAdd={setView} onUse={onUse ? (v) => (onUse(v), onOpenChange(false)) : undefined} />}
          {view === "clone" && <CloneForm onDone={() => setView("list")} />}
          {view === "design" && <DesignForm onDone={() => setView("list")} />}
        </div>
      </SheetContent>
    </Sheet>
  );
}

function VoiceList({ onAdd, onUse }: { onAdd: (v: View) => void; onUse?: (v: Voice) => void }) {
  const { voices, loaded, error, refresh } = useVoiceLibrary();
  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-2 gap-2">
        <Button variant="outline" className="h-auto flex-col items-start gap-0.5 px-3 py-2.5 text-left whitespace-normal" onClick={() => onAdd("clone")}>
          <span className="flex items-center gap-1.5 text-sm font-medium">
            <Mic className="size-4" /> Clone
          </span>
          <span className="text-xs font-normal text-muted-foreground">from 5–15 s of someone speaking</span>
        </Button>
        <Button variant="outline" className="h-auto flex-col items-start gap-0.5 px-3 py-2.5 text-left whitespace-normal" onClick={() => onAdd("design")}>
          <span className="flex items-center gap-1.5 text-sm font-medium">
            <Sparkles className="size-4" /> Design
          </span>
          <span className="text-xs font-normal text-muted-foreground">from a description in words</span>
        </Button>
      </div>

      {error != null && (
        <div className="flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm">
          <span className="flex-1">Could not load the voices: {errorText(error)}</span>
          <Button size="sm" variant="outline" onClick={() => void refresh()}>
            <RotateCcw /> Again
          </Button>
        </div>
      )}

      {!loaded ? (
        <div className="flex items-center gap-2 py-6 text-sm text-muted-foreground">
          <Loader2 className="size-4 animate-spin" /> Loading voices …
        </div>
      ) : voices.length ? (
        <ul className="flex flex-col divide-y rounded-xl border">
          {voices.map((v) => (
            <VoiceRow key={v.id} voice={v} onUse={onUse} />
          ))}
        </ul>
      ) : (
        !error && (
          <Empty icon={<Mic />} title="No voices of your own yet">
            Clone one from a short recording, or design one from a description. The models' preset voices are always there.
          </Empty>
        )
      )}
    </div>
  );
}

function VoiceRow({ voice: v, onUse }: { voice: Voice; onUse?: (v: Voice) => void }) {
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(v.name);
  const [confirm, setConfirm] = useState(false);
  const put = useVoices((s) => s.put);
  const drop = useVoices((s) => s.drop);

  const rename = async () => {
    const n = name.trim();
    setEditing(false);
    if (!n || n === v.name) return setName(v.name);
    put({ ...v, name: n });
    try {
      put(await api.updateVoice(v.id, { name: n }));
    } catch (e) {
      put(v);
      setName(v.name);
      toast.error("Could not rename", { description: errorText(e) });
    }
  };

  return (
    <li className="flex items-start gap-2 px-3 py-2.5">
      {v.sample ? <PreviewButton url={v.sample} className="mt-0.5" /> : <span className="size-7 shrink-0" />}
      <div className="min-w-0 flex-1">
        {editing ? (
          <form className="flex items-center gap-1" onSubmit={(e) => (e.preventDefault(), void rename())}>
            <Input autoFocus value={name} onChange={(e) => setName(e.target.value)} onKeyDown={(e) => e.key === "Escape" && (setName(v.name), setEditing(false))} className="h-7" />
            <Button type="submit" size="icon-sm" variant="ghost" title="Save">
              <Check />
            </Button>
            <Button type="button" size="icon-sm" variant="ghost" title="Cancel" onClick={() => (setName(v.name), setEditing(false))}>
              <X />
            </Button>
          </form>
        ) : (
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="text-sm font-medium">{v.name}</span>
            <VoiceKind voice={v} />
          </div>
        )}
        <p className="mt-0.5 line-clamp-2 text-xs text-muted-foreground">{v.kind === "design" ? v.description : v.transcript ? `“${v.transcript}”` : "No transcript"}</p>
        <p className="mt-0.5 text-[11px] text-muted-foreground/80">{[languageName(v.language), ago(v.created)].filter(Boolean).join(" · ")}</p>
      </div>
      {onUse && (
        <Button size="sm" variant="outline" onClick={() => onUse(v)}>
          Use
        </Button>
      )}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button size="icon-sm" variant="ghost" title="More" className="text-muted-foreground">
            <MoreHorizontal />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onSelect={() => setEditing(true)}>
            <Pencil /> Rename
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem variant="destructive" onSelect={() => setConfirm(true)}>
            <Trash2 /> Delete
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <AlertDialog open={confirm} onOpenChange={setConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete the voice “{v.name}”?</AlertDialogTitle>
            <AlertDialogDescription>
              {v.kind === "clone" ? "Its recording is deleted too. " : ""}What was already made with it stays. This cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              onClick={async () => {
                try {
                  await api.deleteVoice(v.id);
                  drop(v.id);
                } catch (e) {
                  toast.error("Delete failed", { description: errorText(e) });
                }
              }}
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </li>
  );
}

/** The length of an audio file, in seconds (0 when the browser cannot tell). */
function audioLength(url: string): Promise<number> {
  return new Promise((resolve) => {
    const a = new Audio();
    a.preload = "metadata";
    a.onloadedmetadata = () => {
      // webm recordings report Infinity until read through
      if (isFinite(a.duration)) return resolve(a.duration);
      a.currentTime = 1e6;
      a.ontimeupdate = () => ((a.ontimeupdate = null), resolve(isFinite(a.duration) ? a.duration : 0));
    };
    a.onerror = () => resolve(0);
    a.src = url;
  });
}

function CloneForm({ onDone }: { onDone: () => void }) {
  const [consent, setConsent] = useState(false);
  const [name, setName] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const [url, setUrl] = useState("");
  const [len, setLen] = useState(0);
  const [transcript, setTranscript] = useState("");
  const [transcribing, setTranscribing] = useState(false);
  const [saving, setSaving] = useState(false);
  const put = useVoices((s) => s.put);
  const take = useRef(0);
  const hasAsr = useApp((s) => s.models.some((m) => m.kind === "speech" && isAsr(m)));

  useEffect(() => () => void (url && URL.revokeObjectURL(url)), [url]);

  const choose = async (f: File) => {
    const n = ++take.current;
    setFile(f);
    const u = URL.createObjectURL(f);
    setUrl(u);
    setTranscript("");
    setLen(await audioLength(u));
    if (!name.trim() && !/^recording\./.test(f.name)) setName(f.name.replace(/\.[^.]+$/, "").replace(/[_-]+/g, " "));
    // what it says, written down by the transcription model (editable; the server writes it down too when it is left empty)
    const m = asrModel();
    if (!m) return;
    setTranscribing(true);
    try {
      const { url: up } = await api.upload(f);
      const j = await api.submit({ model: m.id, audio: up });
      useApp.getState().put(j);
      const done = await waitJob(j.id);
      if (n !== take.current) return;
      if (done.state === "done") setTranscript((t) => t || (done.text ?? String(done.result?.text ?? "")).trim());
      else toast.error("Could not write down what it says", { description: done.problem?.title ?? done.error ?? "Type it in, or leave it empty." });
    } catch (e) {
      if (n === take.current) toast.error("Could not write down what it says", { description: errorText(e) });
    } finally {
      if (n === take.current) setTranscribing(false);
    }
  };

  const save = async () => {
    if (!file || !name.trim() || !consent) return;
    setSaving(true);
    try {
      const v = await api.cloneVoice(name.trim(), file, transcript, file.name);
      put(v);
      toast.success(`“${v.name}” is in your voices`);
      onDone();
    } catch (e) {
      toast.error("Could not add the voice", { description: errorText(e) });
    } finally {
      setSaving(false);
    }
  };

  const lenNote = !len ? "" : len < 5 ? `${duration(len)} — a little short; 5–15 s works best.` : len > 20 ? `${duration(len)} — the first 20 s are used.` : `${duration(len)} — a good length.`;

  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-col gap-2.5 rounded-lg border border-amber-500/35 bg-amber-500/8 p-3">
        <div className="flex items-start gap-2 text-sm">
          <ShieldCheck className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-400" />
          <p>
            <span className="font-medium">Only with the speaker’s consent.</span>{" "}
            <span className="text-muted-foreground">
              Clone your own voice, or the voice of someone who agreed to it. Never use a cloned voice to pretend to be someone, or to mislead.
            </span>
          </p>
        </div>
        <label className="flex cursor-pointer items-center gap-2 pl-6 text-sm">
          <Checkbox checked={consent} onCheckedChange={(c) => setConsent(c === true)} />
          It is my voice, or the speaker agreed to this.
        </label>
      </div>

      <div className={cn("flex flex-col gap-5 transition-opacity", !consent && "pointer-events-none opacity-50")} aria-disabled={!consent}>
        <Field label="Name">
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. Anna, calm" disabled={!consent} />
        </Field>

        <Field
          label="Recording"
          hint={
            <span className="flex flex-col gap-0.5">
              <span>5–15 seconds · speak clearly and naturally · no music or noise · one speaker only.</span>
              <span>Read a few full sentences; they set the voice’s tone.</span>
            </span>
          }
        >
          {file ? (
            <div className="flex flex-col gap-2 rounded-lg border p-2.5">
              <audio src={url} controls className="h-9 w-full" />
              <div className="flex items-center gap-2 text-xs text-muted-foreground">
                <span className={cn("min-w-0 flex-1 truncate", len && (len < 5 || len > 20) && "text-amber-700 dark:text-amber-400")}>{lenNote || file.name}</span>
                <Button size="xs" variant="ghost" onClick={() => (take.current++, setFile(null), setUrl(""), setLen(0), setTranscript(""), setTranscribing(false))}>
                  <RotateCcw /> Another
                </Button>
              </div>
            </div>
          ) : (
            <Recorder onFile={(f) => void choose(f)} />
          )}
        </Field>

        <Field
          label="What it says"
          hint={transcribing ? undefined : hasAsr ? "Filled in after the recording; correct it if a word is wrong — an exact transcript makes a better clone." : "Type what the recording says; left empty, the server writes it down."}
          actions={
            transcribing && (
              <span className="flex items-center gap-1 text-xs text-muted-foreground">
                <Loader2 className="size-3 animate-spin" /> writing it down …
              </span>
            )
          }
        >
          <Textarea value={transcript} onChange={(e) => setTranscript(e.target.value)} rows={3} disabled={!consent} placeholder={transcribing ? "" : "The exact words of the recording"} />
        </Field>

        <Button size="lg" className="h-10" disabled={!consent || !file || !name.trim() || saving} onClick={save}>
          {saving ? <Loader2 className="animate-spin" /> : <Plus />} Add the voice
        </Button>
      </div>
    </div>
  );
}

function DesignForm({ onDone }: { onDone: () => void }) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [language, setLanguage] = useState("auto");
  const [saving, setSaving] = useState(false);
  const put = useVoices((s) => s.put);

  const save = async () => {
    if (!name.trim() || !description.trim()) return;
    setSaving(true);
    try {
      const v = await api.designVoice({ name: name.trim(), description: description.trim(), ...(language !== "auto" ? { language } : {}) });
      put(v);
      toast.success(`“${v.name}” is in your voices`);
      onDone();
    } catch (e) {
      toast.error("Could not add the voice", { description: errorText(e) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="flex flex-col gap-5">
      <Field label="Name">
        <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. Narrator" />
      </Field>
      <Field label="Description" hint="Age, gender, pitch, pace, mood, accent — the more concrete, the better.">
        <Textarea value={description} onChange={(e) => setDescription(e.target.value)} rows={4} placeholder={designExamples[0]} />
      </Field>
      <div className="-mt-2 flex flex-col gap-1.5">
        <span className="text-xs text-muted-foreground">Examples</span>
        <div className="flex flex-wrap gap-1.5">
          {designExamples.map((d) => (
            <button
              key={d}
              type="button"
              onClick={() => setDescription(d)}
              className="max-w-full truncate rounded-full border px-2.5 py-1 text-left text-xs text-muted-foreground transition-colors hover:border-primary/50 hover:text-foreground"
            >
              {d}
            </button>
          ))}
        </div>
      </div>
      <Field label="Language">
        <Select value={language} onValueChange={setLanguage}>
          <SelectTrigger className="w-full">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {languages.map((l) => (
              <SelectItem key={l.id} value={l.id}>
                {l.id === "auto" ? "Any" : l.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </Field>
      <Button size="lg" className="h-10" disabled={!name.trim() || !description.trim() || saving} onClick={save}>
        {saving ? <Loader2 className="animate-spin" /> : <Plus />} Add the voice
      </Button>
    </div>
  );
}
