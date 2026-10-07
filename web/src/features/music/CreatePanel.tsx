import { Dices, Folder, Loader2, PenLine, Sparkles, Upload, Wand2 } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Example } from "@/api/types";
import { ExamplesMenu } from "@/components/kit/Examples";
import { CreateButton } from "@/components/kit/JobBits";
import { Advanced, Field } from "@/components/kit/Layout";
import { ModelPicker, useModelChoice } from "@/components/kit/ModelPicker";
import { PromptBox } from "@/components/kit/PromptBox";
import { RunOn, runOnWorker } from "@/components/kit/RunOn";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Slider } from "@/components/ui/slider";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { edmWords } from "@/lib/edm";
import { helperModel, runChat, submitEach } from "@/lib/jobs";
import { navigate } from "@/lib/route";
import { local } from "@/lib/storage";
import { useApp } from "@/store/app";
import { fitLength, mmss, parseSong, STARRED, styleBPM } from "./songs";
import { useStudio, type Draft } from "./state";
import { StylePicker } from "./StylePicker";
import { useStyles } from "./styles";

const IDEAS = [
  "A summer road trip anthem with handclaps",
  "Melancholic lo-fi about rainy Sunday mornings",
  "90s eurodance about dancing until sunrise",
  "A cozy acoustic lullaby for a cat",
];

/** Make a new song: Simple (one idea; a chat model writes it) or Custom (title, style, lyrics, length, seed). */
export function CreatePanel() {
  const draft = useStudio((s) => s.draft);
  const setDraft = useStudio((s) => s.setDraft);
  const folder = useStudio((s) => s.folder);
  const loadModel = useStudio((s) => s.loadModel);
  const choice = useModelChoice("music");
  const [busy, setBusy] = useState<"" | "write" | "create">("");
  // just filled by Load into the form (which opens this page): the form flashes once
  const [flash] = useState(() => Date.now() - useStudio.getState().loaded < 2000);
  const into = folder === STARRED ? "" : folder;

  // Load into the form: pick the song's model
  useEffect(() => {
    if (!loadModel) return;
    if (choice.models.some((m) => m.id === loadModel)) choice.set([loadModel]);
    useStudio.setState({ loadModel: null });
  }, [loadModel, choice]);

  /** Asks a chat model for TITLE / STYLE / LYRICS from the idea; fills the Custom form. */
  const write = async (): Promise<Partial<Draft> | null> => {
    const idea = draft.idea.trim();
    const p = useApp.getState().prompts.find((x) => x.use === "write-song");
    const model = helperModel();
    if (!idea) return toast("Describe the song first."), null;
    if (!p || !model) return toast.error("Needs a chat model and the “Song from an idea” prompt."), null;
    const text = await runChat(model, p.text, draft.ideaInstrumental ? `${idea}\n\n(instrumental, no vocals)` : idea, { temperature: 0.8, maxTokens: 1200 });
    const s = parseSong(text);
    if (!s.style && !s.lyrics) throw new Error("The answer had no style or lyrics. Try again, or another chat model.");
    const instrumental = draft.ideaInstrumental || !s.lyrics.replace(/\[[^\]]*\]/g, "").trim();
    const d: Partial<Draft> = {
      title: s.title,
      style: s.style,
      lyrics: s.lyrics,
      instrumental,
      duration: fitLength(s.style, instrumental ? "" : s.lyrics).seconds,
    };
    setDraft(d);
    return d;
  };

  const sing = async (d: Draft) => {
    if (!d.style.trim() && !(d.lyrics.trim() && !d.instrumental)) return toast("Describe the style, or write some lyrics.");
    const req = {
      title: d.title.trim() || undefined,
      prompt: d.style.trim(),
      lyrics: d.instrumental ? "" : d.lyrics.trim(),
      instrumental: d.instrumental,
      duration: d.duration || undefined,
      seed: d.seed.trim() ? Number(d.seed) : undefined,
      bpm: d.bpm?.trim() ? Number(d.bpm) : undefined,
      lockTempo: d.lockTempo ?? true,
      takes: d.takes > 1 ? d.takes : undefined,
      folder: into || undefined,
      worker: runOnWorker("music"),
    };
    // a trained style goes with the model it was trained for (it runs on the Mac that has it); the others without
    const trained = useStyles.getState().styles.find((s) => s.id === d.trainedStyle && s.weights);
    const styled = trained ? choice.ids.filter((id) => id === trained.baseModel) : [];
    const jobs = [
      ...(styled.length ? await submitEach(styled, { ...req, style: trained!.id, styleStrength: d.styleStrength, worker: undefined }) : []),
      ...(await submitEach(choice.ids.filter((id) => !styled.includes(id)), req)),
    ];
    if (jobs[0]) navigate("music", jobs[0].id);
  };

  const run = async (what: "write" | "simple" | "custom") => {
    setBusy(what === "write" ? "write" : "create");
    try {
      if (what === "custom") return await sing(draft);
      const d = await write();
      if (!d) return;
      if (what === "write") setDraft({ mode: "custom" });
      else await sing({ ...draft, ...d });
    } catch (e) {
      toast.error("Writing the song", { description: errorText(e) });
    } finally {
      setBusy("");
    }
  };

  const pickExample = (e: Example) =>
    setDraft({ mode: "custom", title: e.title.replace(/\s*\(.*\)$/, ""), style: e.prompt ?? "", lyrics: e.lyrics ?? "", instrumental: !e.lyrics, duration: e.duration || draft.duration, seed: "" });

  const pickStyle = useCallback((trainedStyle: string, styleStrength: number) => setDraft({ trainedStyle, styleStrength }), [setDraft]);

  const simple = draft.mode === "simple";
  return (
    <div className="mx-auto flex w-full max-w-2xl flex-col gap-5 p-6 max-sm:p-4">
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold tracking-tight">New song</h1>
        <ToggleGroup type="single" variant="outline" size="sm" value={draft.mode} onValueChange={(v) => v && setDraft({ mode: v as Draft["mode"] })}>
          <ToggleGroupItem value="simple" className="px-3">
            Simple
          </ToggleGroupItem>
          <ToggleGroupItem value="custom" className="px-3">
            Custom
          </ToggleGroupItem>
        </ToggleGroup>
        <div className="flex-1" />
        {!simple && <ExamplesMenu kind="music" onPick={pickExample} />}
        <ImportButton folder={into} />
      </div>

      <div className="relative flex flex-col gap-5 rounded-xl border bg-card/50 p-5 max-sm:p-4">
        {flash && <div className="pointer-events-none absolute -inset-px rounded-xl ring-2 ring-primary animate-out fade-out fill-mode-forwards delay-500 duration-1000" />}
        {simple ? (
          <>
            <Field label="Describe your song" hint="A chat model writes the title, style and lyrics, then the song model sings it.">
              <Textarea
                autoFocus
                rows={5}
                value={draft.idea}
                onChange={(e) => setDraft({ idea: e.target.value })}
                placeholder="An upbeat synthwave song about driving through Hamburg at night, with a dreamy female voice"
                className="min-h-32 resize-none text-base"
                onKeyDown={(e) => e.key === "Enter" && (e.metaKey || e.ctrlKey) && run("simple")}
              />
            </Field>
            {!draft.idea.trim() && (
              <div className="-mt-2 flex flex-wrap gap-1.5">
                {IDEAS.map((i) => (
                  <button key={i} onClick={() => setDraft({ idea: i })} className="rounded-full border px-2.5 py-1 text-xs text-muted-foreground transition-colors hover:border-primary/50 hover:text-foreground">
                    {i}
                  </button>
                ))}
              </div>
            )}
            <label className="flex items-center gap-2.5 text-sm">
              <Switch checked={draft.ideaInstrumental} onCheckedChange={(v) => setDraft({ ideaInstrumental: v })} />
              Instrumental
            </label>
            <EdmHint text={draft.idea} instrumental={draft.ideaInstrumental} choice={choice} onInstrumental={() => setDraft({ ideaInstrumental: true })} />
          </>
        ) : (
          <>
            <Field label="Title">
              <Input value={draft.title} onChange={(e) => setDraft({ title: e.target.value })} placeholder="Untitled" />
            </Field>
            <PromptBox
              label="Style"
              enhance="enhance-music"
              context={draft.instrumental ? undefined : draft.lyrics}
              value={draft.style}
              onChange={(v) => setDraft({ style: v })}
              rows={3}
              placeholder="indie pop, bright acoustic guitar, soft drums, warm female vocal, 110 bpm"
            />
            <label className="flex items-center gap-2.5 text-sm">
              <Switch checked={draft.instrumental} onCheckedChange={(v) => setDraft({ instrumental: v })} />
              Instrumental
            </label>
            <EdmHint text={draft.style} instrumental={draft.instrumental} choice={choice} onInstrumental={() => setDraft({ instrumental: true })} />
            {!draft.instrumental && (
              <PromptBox
                label="Lyrics"
                value={draft.lyrics}
                onChange={(v) => setDraft({ lyrics: v })}
                rows={10}
                className="[&_textarea]:font-mono [&_textarea]:text-[13px] [&_textarea]:leading-relaxed"
                placeholder={"[verse]\nSoft morning light is touching the window\n…\n\n[chorus]\n…"}
              />
            )}
            <LengthField draft={draft} setDraft={setDraft} />
            <TempoFields draft={draft} setDraft={setDraft} />
            <Advanced id="music.create" summary={draft.seed ? `seed ${draft.seed}` : "random seed"}>
              <Field
                label="Seed"
                hint="The same seed with the same request makes the same song."
                actions={
                  <Button variant="ghost" size="xs" onClick={() => setDraft({ seed: "" })} disabled={!draft.seed}>
                    <Dices /> Random
                  </Button>
                }
              >
                <Input inputMode="numeric" value={draft.seed} onChange={(e) => setDraft({ seed: e.target.value.replace(/\D/g, "") })} placeholder="random" />
              </Field>
              <RunOn page="music" model={choice.primary} />
            </Advanced>
          </>
        )}
      </div>

      <div className="flex flex-col gap-3 rounded-xl border bg-card/50 p-4">
        <div className="flex flex-wrap items-center gap-2">
          <ModelPicker choice={choice} className="min-w-0 flex-1 max-sm:basis-full sm:max-w-72" />
          {simple && (
            <Button variant="outline" className="h-9" disabled={!!busy || !draft.idea.trim()} onClick={() => run("write")} title="Write title, style and lyrics, then edit them before the song is made">
              {busy === "write" ? <Loader2 className="animate-spin" /> : <PenLine />} Write lyrics first
            </Button>
          )}
          <div className="flex items-center gap-1.5 text-xs text-muted-foreground sm:ml-auto">
            <Folder className="size-3.5" />
            {into ? <span className="max-w-48 truncate">{into}</span> : "No folder"}
          </div>
        </div>
        <StylePicker model={choice.primary} value={draft.trainedStyle} strength={draft.styleStrength} onChange={pickStyle} />
        <CreateButton
          count={choice.ids.length}
          busy={busy === "create"}
          disabled={!!busy || (simple ? !draft.idea.trim() : !draft.style.trim() && !draft.lyrics.trim())}
          onClick={() => run(simple ? "simple" : "custom")}
          label={busy === "create" && simple ? "Writing the song …" : "Create"}
        />
        {busy && simple && (
          <p className="flex items-center justify-center gap-1.5 text-xs text-muted-foreground">
            <Wand2 className="size-3.5 text-primary" /> A chat model is writing title, style and lyrics.
          </p>
        )}
      </div>
    </div>
  );
}

/** Tempo: a BPM (else "NNN bpm" in the style), the tempo lock and best of N (internal/engine/tempo.go). */
function TempoFields({ draft, setDraft }: { draft: Draft; setDraft: (d: Partial<Draft>) => void }) {
  const fromStyle = styleBPM(draft.style);
  const tempo = (draft.bpm ?? "").trim() || fromStyle;
  return (
    <div className="flex flex-wrap items-end gap-4">
      <Field label="BPM" hint={fromStyle && !(draft.bpm ?? "").trim() ? `${fromStyle} from the style` : "The tempo; empty: from the style"}>
        <Input
          className="w-24"
          inputMode="numeric"
          value={draft.bpm ?? ""}
          onChange={(e) => setDraft({ bpm: e.target.value.replace(/\D/g, "").slice(0, 3) })}
          placeholder={fromStyle ?? "auto"}
        />
      </Field>
      <label className="flex items-center gap-2.5 pb-2 text-sm" title="Measures the new song's tempo and stretches it to the exact BPM when it is off by at most 12 % (the original is kept)">
        <Switch checked={draft.lockTempo ?? true} disabled={!tempo} onCheckedChange={(v) => setDraft({ lockTempo: v })} />
        Lock tempo
      </label>
      <Field label="Best of" hint="Takes; the one closest to the tempo is kept">
        <ToggleGroup
          type="single"
          variant="outline"
          size="sm"
          value={String(draft.takes || 1)}
          onValueChange={(v) => v && setDraft({ takes: Number(v) })}
          disabled={!tempo}
        >
          {[1, 2, 3].map((n) => (
            <ToggleGroupItem key={n} value={String(n)} className="px-3">
              {n}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
      </Field>
    </div>
  );
}

function LengthField({ draft, setDraft }: { draft: Draft; setDraft: (d: Partial<Draft>) => void }) {
  const [fit, setFit] = useState("");
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  return (
    <Field
      label={
        <>
          Length <span className="ml-1 font-normal text-foreground tabular-nums">{mmss(draft.duration)}</span>
        </>
      }
      actions={
        <Button
          variant="ghost"
          size="xs"
          className="text-primary"
          onClick={() => {
            const f = fitLength(draft.style, draft.instrumental ? "" : draft.lyrics);
            setDraft({ duration: f.seconds });
            setFit(`≈ ${mmss(f.seconds)} at ${f.bpm} bpm`);
            clearTimeout(timer.current);
            timer.current = setTimeout(() => setFit(""), 2500);
          }}
        >
          {fit || "Fit to lyrics"}
        </Button>
      }
    >
      <Slider min={10} max={600} step={5} value={[draft.duration]} onValueChange={([v]) => setDraft({ duration: v })} className="py-2" />
    </Field>
  );
}

/** Import an audio file as a song (into the open folder); it can be remixed like any other. */
function ImportButton({ folder }: { folder: string }) {
  const input = useRef<HTMLInputElement>(null);
  const [busy, setBusy] = useState(false);
  return (
    <>
      <Button variant="outline" size="sm" disabled={busy} onClick={() => input.current?.click()} title="Bring in a song of your own to remix, extend or split">
        {busy ? <Loader2 className="animate-spin" /> : <Upload />} Import
      </Button>
      <input
        ref={input}
        type="file"
        accept="audio/*"
        hidden
        onChange={async (e) => {
          const f = e.target.files?.[0];
          e.target.value = "";
          if (!f) return;
          setBusy(true);
          try {
            const r = await api.upload(f, { import: true, folder: folder || undefined });
            if (r.job) {
              useApp.getState().put(r.job);
              navigate("music", r.job.id);
            }
          } catch (err) {
            toast.error("Import failed", { description: errorText(err) });
          } finally {
            setBusy(false);
          }
        }}
      />
    </>
  );
}

const SA3 = "stable-audio-3-medium";

/**
 * EDM words in the style → suggest Stable Audio 3 Medium (best by far for EDM in the user's tests). A suggestion,
 * never a switch: one click uses it; dismissing hides it for these words.
 */
function EdmHint({ text, instrumental, choice, onInstrumental }: { text: string; instrumental: boolean; choice: ReturnType<typeof useModelChoice>; onInstrumental: () => void }) {
  const words = edmWords(text);
  const key = words.slice(0, 3).join(",");
  const [dismissed, setDismissed] = useState<string[]>(() => local.get<string[]>("music.edmHint.dismissed", []));
  const sa3 = choice.models.find((m) => m.id === SA3);
  if (!words.length || !sa3 || choice.ids.includes(SA3) || dismissed.includes(key)) return null;
  const use = () => {
    if (!instrumental) onInstrumental();
    choice.set([SA3]);
  };
  const dismiss = () => {
    const next = [...dismissed, key].slice(-50);
    setDismissed(next);
    local.set("music.edmHint.dismissed", next);
  };
  return (
    <div className="flex items-start gap-2.5 rounded-lg border border-primary/30 bg-primary/5 p-3 text-sm">
      <Sparkles className="mt-0.5 size-4 shrink-0 text-primary" />
      <div className="min-w-0 flex-1">
        <div className="font-medium">
          Sounds like EDM ({words.slice(0, 2).join(", ")}) — {sa3.name} sounds best for it
        </div>
        <div className="mt-0.5 text-xs text-muted-foreground">
          {instrumental
            ? "Fast (seconds), exactly on tempo, up to 6 minutes."
            : "It makes instrumentals only: the lyrics would not be sung."}
        </div>
        <div className="mt-2 flex gap-1.5">
          <Button size="sm" onClick={use}>
            {instrumental ? `Use ${sa3.name}` : `Make it instrumental with ${sa3.name}`}
          </Button>
          <Button size="sm" variant="ghost" className="text-muted-foreground" onClick={dismiss}>
            Not now
          </Button>
        </div>
      </div>
    </div>
  );
}
