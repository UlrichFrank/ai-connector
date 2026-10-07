import { AudioLines, Dices, Download, Library, Lightbulb, Lock, Mic, Waves, X } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import type { Job, Model, Voice } from "@/api/types";
import { ExamplesMenu } from "@/components/kit/Examples";
import { CreateButton } from "@/components/kit/JobBits";
import { Advanced, CreateLayout, Empty, Field, SectionHeader } from "@/components/kit/Layout";
import { ModelPicker, useModelChoice } from "@/components/kit/ModelPicker";
import { PromptBox } from "@/components/kit/PromptBox";
import { RunOn, runOnWorker } from "@/components/kit/RunOn";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Slider } from "@/components/ui/slider";
import { Textarea } from "@/components/ui/textarea";
import { bytes, duration } from "@/lib/format";
import { submitEach } from "@/lib/jobs";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";
import { AudioCard } from "./AudioCard";
import { defaultSound, defaultSpeech, soundExamples, soundFrom, soundRequest, speechFrom, speechRequest, type SoundForm, type SpeechForm } from "./form";
import { Segmented } from "./Segmented";
import { VoiceLibrary } from "./VoiceLibrary";
import { VoicePicker } from "./VoicePicker";
import { estimateSeconds, isSpoken, languages, moods, speaks, useVoiceLibrary } from "./voices";

export { AudioCard } from "./AudioCard";
export { Recorder } from "./Recorder";
export { PreviewButton } from "./Preview";
export { Segmented } from "./Segmented";
export { VoiceLibrary } from "./VoiceLibrary";
export { VoicePicker, VoiceKind } from "./VoicePicker";
export * from "./voices";

type Tab = "speech" | "sound";
const isSound = (j: Job) => j.kind === "sound";
const page = 30;

/**
 * Voice: text to speech (library or preset voices, how it should sound, language, speed) and a Sound effects tab.
 * `arg`: "sound" for the tab, or a job id whose settings to load (Use again, the player bar).
 */
export function VoicePage({ arg }: { arg: string }) {
  const [tab, setTab] = useState<Tab>(() => (arg === "sound" ? "sound" : local.get<Tab>("voice.tab", "speech")));
  const [libOpen, setLibOpen] = useState(false);
  const [libStart, setLibStart] = useState<"list" | "clone" | "design">("list");
  const [speech, setSpeech] = useState<SpeechForm>(() => ({ ...defaultSpeech, ...local.get<Partial<SpeechForm>>("voice.speech", {}) }));
  const [sound, setSound] = useState<SoundForm>(() => ({ ...defaultSound, ...local.get<Partial<SoundForm>>("voice.sound", {}) }));
  const [busy, setBusy] = useState(false);
  const [highlight, setHighlight] = useState("");
  const results = useRef<HTMLDivElement>(null);
  const speechChoice = useModelChoice("speech", "", speaks);
  const soundChoice = useModelChoice("sound");
  const { voices, presets } = useVoiceLibrary();

  useEffect(() => local.set("voice.tab", tab), [tab]);
  useEffect(() => local.set("voice.speech", speech), [speech]);
  useEffect(() => local.set("voice.sound", sound), [sound]);

  const setS = (p: Partial<SpeechForm>) => setSpeech((f) => ({ ...f, ...p }));
  const setX = (p: Partial<SoundForm>) => setSound((f) => ({ ...f, ...p }));

  const loadSettings = (j: Job) => {
    if (j.kind === "sound") {
      setTab("sound");
      setSound(soundFrom(j));
      if (soundChoice.models.some((m) => m.id === j.model)) soundChoice.set([j.model]);
    } else {
      setTab("speech");
      setSpeech(speechFrom(j));
      if (speechChoice.models.some((m) => m.id === j.model)) speechChoice.set([j.model]);
    }
    toast.success("Settings loaded", { description: j.kind === "sound" ? "Prompt, length and model are in the composer." : "Text, voice, style and model are in the composer." });
  };

  // #/voice/<id> (the library, the player bar, a toast): its tab, the card highlighted; #/voice/sound: the tab
  const target = useApp((s) => (arg && arg !== "sound" ? s.jobs[arg] : undefined));
  const applied = useRef("");
  useEffect(() => {
    if (arg === "sound") setTab("sound");
    if (!target || applied.current === target.id || (target.kind !== "speech" && target.kind !== "sound")) return;
    applied.current = target.id;
    setTab(target.kind === "sound" ? "sound" : "speech");
    setHighlight(target.id);
    setTimeout(() => document.getElementById(`audio-${target.id}`)?.scrollIntoView({ behavior: "smooth", block: "center" }), 120);
    location.replace("#/voice");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [arg, target?.id]);

  // a preset of another model does not fit the one chosen now
  const model = speechChoice.primary;
  useEffect(() => {
    const v = speech.voice;
    if (!v || !model || voices.some((x) => x.id === v) || !Object.keys(presets).length) return;
    if (!(presets[model.id] ?? []).includes(v)) setS({ voice: "" });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [model?.id, presets]);

  const canCreate = tab === "speech" ? !!speech.text.trim() : !!sound.prompt.trim();
  const create = async () => {
    if (!canCreate || busy) return;
    setBusy(true);
    const jobs = tab === "speech" ? await submitEach(speechChoice.ids, { ...speechRequest(speech), worker: runOnWorker("speech") }) : await submitEach(soundChoice.ids, { ...soundRequest(sound), worker: runOnWorker("sound") });
    setBusy(false);
    if (jobs.length && window.matchMedia("(max-width: 1023px)").matches) results.current?.scrollIntoView({ behavior: "smooth" });
  };
  const keys = (e: React.KeyboardEvent) => (e.metaKey || e.ctrlKey) && e.key === "Enter" && (e.preventDefault(), void create());

  const manage = (start: "list" | "clone" | "design" = "list") => (setLibStart(start), setLibOpen(true));

  const composer = (
    <>
      <Segmented
        value={tab}
        onChange={setTab}
        options={[
          { id: "speech", label: (<><Mic /> Speech</>) },
          { id: "sound", label: (<><Waves /> Sound effects</>) },
        ]}
      />
      {tab === "speech" ? (
        <SpeechComposer form={speech} set={setS} choice={speechChoice} onManage={manage} onKeyDown={keys} />
      ) : (
        <SoundComposer form={sound} set={setX} choice={soundChoice} onKeyDown={keys} />
      )}
    </>
  );

  const choice = tab === "speech" ? speechChoice : soundChoice;
  return (
    <>
      <CreateLayout composer={composer} footer={<CreateButton count={choice.ids.length} busy={busy} disabled={!canCreate} onClick={create} />}>
        <div ref={results} className="scroll-mt-4">
          <Results tab={tab} onUseAgain={loadSettings} highlight={highlight} onManage={() => manage()} />
        </div>
      </CreateLayout>
      <VoiceLibrary open={libOpen} onOpenChange={setLibOpen} start={libStart} onUse={(v: Voice) => (setTab("speech"), setS({ voice: v.id }))} />
    </>
  );
}

function SpeechComposer({
  form,
  set,
  choice,
  onManage,
  onKeyDown,
}: {
  form: SpeechForm;
  set: (p: Partial<SpeechForm>) => void;
  choice: ReturnType<typeof useModelChoice>;
  onManage: (start?: "list" | "clone" | "design") => void;
  onKeyDown: (e: React.KeyboardEvent) => void;
}) {
  const secs = estimateSeconds(form.text, form.speed);
  return (
    <>
      <Field
        label="Text"
        actions={
          <ExamplesMenu
            kind="speech"
            onPick={(e) => set({ text: (e.text as string) ?? e.prompt ?? "", ...(e.instruct ? { instruct: e.instruct as string } : {}), ...(e.language ? { language: e.language as string } : {}) })}
          />
        }
      >
        <div className="relative">
          <Textarea
            value={form.text}
            onChange={(e) => set({ text: e.target.value })}
            onKeyDown={onKeyDown}
            rows={9}
            className="min-h-44 resize-y pb-7 text-[15px] leading-relaxed"
            placeholder="Type what should be said. Long texts are split at sentences and joined."
          />
          <div className="pointer-events-none absolute right-2.5 bottom-2 flex gap-2 text-[11px] text-muted-foreground tabular-nums">
            <span>{form.text.length.toLocaleString()} characters</span>
            {form.text.trim() && <span>≈ {duration(Math.max(1, secs))}</span>}
          </div>
        </div>
      </Field>

      <Field
        label="Voice"
        actions={
          <Button variant="ghost" size="xs" className="text-muted-foreground" onClick={() => onManage()}>
            <Library /> Voices
          </Button>
        }
      >
        <VoicePicker value={form.voice} onChange={(voice) => set({ voice })} model={choice.primary} onManage={() => onManage()} />
      </Field>

      <Field label="How it should sound" hint={form.instruct ? undefined : "Optional. Not every model follows it; Qwen3-TTS does best."}>
        <div className="flex flex-wrap gap-1.5">
          {moods.map((m) => {
            const on = form.instruct === m.text;
            return (
              <button
                key={m.label}
                type="button"
                onClick={() => set({ instruct: on ? "" : m.text })}
                className={cn(
                  "rounded-full border px-2.5 py-1 text-xs transition-colors",
                  on ? "border-primary bg-primary/10 text-foreground" : "text-muted-foreground hover:border-primary/50 hover:text-foreground",
                )}
              >
                {m.label}
              </button>
            );
          })}
        </div>
        <Textarea value={form.instruct} onChange={(e) => set({ instruct: e.target.value })} rows={2} className="min-h-14 resize-y" placeholder="e.g. Warm and slow, smiling, like telling a secret" />
      </Field>

      <div className="grid grid-cols-2 gap-3">
        <Field label="Language">
          <Select value={form.language} onValueChange={(language) => set({ language })}>
            <SelectTrigger className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {languages.map((l) => (
                <SelectItem key={l.id} value={l.id}>
                  {l.id === "auto" ? "Auto" : l.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
        <Field
          label="Speed"
          actions={
            <button type="button" className="text-xs text-muted-foreground tabular-nums hover:text-foreground" title="Back to normal" onClick={() => set({ speed: 1 })}>
              {form.speed.toFixed(2).replace(/0$/, "")}×
            </button>
          }
        >
          <Slider className="h-9" value={[form.speed]} min={0.5} max={2} step={0.05} onValueChange={([speed]) => set({ speed })} />
        </Field>
      </div>

      <Field label="Model">
        <ModelPicker choice={choice} className="w-full" />
        <ModelNote models={choice.models.filter((x) => choice.ids.includes(x.id))} />
      </Field>

      <Advanced id="speech" summary={form.seed != null ? `seed ${form.seed}` : "random seed"}>
        <SeedField seed={form.seed} onChange={(seed) => set({ seed })} />
        <RunOn page="speech" model={choice.primary} />
      </Advanced>
    </>
  );
}

function SoundComposer({ form, set, choice, onKeyDown }: { form: SoundForm; set: (p: Partial<SoundForm>) => void; choice: ReturnType<typeof useModelChoice>; onKeyDown: (e: React.KeyboardEvent) => void }) {
  const server = useApp((s) => !!s.examples.sound?.length);
  return (
    <>
      <PromptBox
        label="What it should sound like"
        value={form.prompt}
        onChange={(prompt) => set({ prompt })}
        rows={5}
        className="[&_textarea]:min-h-28 [&_textarea]:resize-y"
        placeholder="Heavy rain on a tin roof, distant thunder, a dog barking once far away"
        onKeyDown={onKeyDown}
        actions={
          server ? (
            <ExamplesMenu kind="sound" onPick={(e) => set({ prompt: e.prompt ?? "", ...(e.duration ? { duration: e.duration } : {}) })} />
          ) : (
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="sm" className="text-muted-foreground">
                  <Lightbulb /> Examples
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-72">
                {soundExamples.map((e) => (
                  <DropdownMenuItem key={e.title} onSelect={() => set({ prompt: e.prompt, duration: e.duration })} className="flex flex-col items-start gap-0.5">
                    <span className="font-medium">{e.title}</span>
                    <span className="line-clamp-1 text-xs text-muted-foreground">{e.prompt}</span>
                  </DropdownMenuItem>
                ))}
              </DropdownMenuContent>
            </DropdownMenu>
          )
        }
      />
      <Field label="Length" actions={<span className="text-xs text-muted-foreground tabular-nums">{duration(form.duration)}</span>}>
        <Slider className="h-9" value={[form.duration]} min={1} max={30} step={0.5} onValueChange={([d]) => set({ duration: d })} />
      </Field>
      <Field label="Model">
        <ModelPicker choice={choice} className="w-full" />
        <ModelNote models={choice.models.filter((x) => choice.ids.includes(x.id))} />
      </Field>
      <Advanced id="sound" summary={form.seed != null ? `seed ${form.seed}` : "random seed"}>
        <SeedField seed={form.seed} onChange={(seed) => set({ seed })} />
        <RunOn page="sound" model={choice.primary} />
      </Advanced>
    </>
  );
}

function Results({ tab, onUseAgain, highlight, onManage }: { tab: Tab; onUseAgain: (j: Job) => void; highlight: string; onManage: () => void }) {
  const jobs = useApp((s) => s.jobs);
  const [limit, setLimit] = useState(page);
  const list = useMemo(
    () =>
      Object.values(jobs)
        .filter(tab === "speech" ? isSpoken : isSound)
        .sort((a, b) => Date.parse(b.created) - Date.parse(a.created)),
    [jobs, tab],
  );
  return (
    <div className="mx-auto flex max-w-3xl flex-col">
      <SectionHeader
        title={
          <>
            {tab === "speech" ? "Speech" : "Sound effects"} <span className="font-normal text-muted-foreground">· {list.length}</span>
          </>
        }
      >
        {tab === "speech" && (
          <Button variant="outline" size="sm" onClick={onManage}>
            <Library /> Voices
          </Button>
        )}
      </SectionHeader>
      {list.length ? (
        <div className="flex flex-col gap-3">
          {list.slice(0, limit).map((j) => (
            <AudioCard key={j.id} job={j} onUseAgain={onUseAgain} highlight={highlight === j.id} />
          ))}
          {limit < list.length && (
            <Button variant="ghost" className="self-center text-muted-foreground" onClick={() => setLimit(limit + page)}>
              Show more ({list.length - limit})
            </Button>
          )}
        </div>
      ) : tab === "speech" ? (
        <Empty icon={<AudioLines />} title="Nothing said yet">
          Type a text on the left, pick a voice, and press Create. Your own voices — cloned from a recording or designed from a description — are under{" "}
          <button className="text-primary hover:underline" onClick={onManage}>
            Voices
          </button>
          .
        </Empty>
      ) : (
        <Empty icon={<Waves />} title="No sounds yet">
          Describe a sound on the left — rain, footsteps, a door, a spaceship — choose how long, and press Create.
        </Empty>
      )}
    </div>
  );
}

/** What happens with a model that is not on this Mac yet. */
export function ModelNote({ models }: { models: Model[] }) {
  const away = models.filter((m) => !m.here);
  if (!away.length) return null;
  return (
    <div className="flex flex-col gap-1">
      {away.map((m) => (
        <p key={m.id} className="flex items-start gap-1.5 text-xs text-amber-700 dark:text-amber-400">
          {m.gated ? <Lock className="mt-px size-3.5 shrink-0" /> : <Download className="mt-px size-3.5 shrink-0" />}
          <span>
            {m.name} is not downloaded yet{m.size ? ` (${bytes(m.size)})` : ""}. Creating downloads it first{m.gated ? "; it needs a Hugging Face token (HF_TOKEN) with access" : ""}.
          </span>
        </p>
      ))}
    </div>
  );
}

/** Blank: a new random seed each time. */
export function SeedField({ seed, onChange }: { seed: number | null; onChange: (s: number | null) => void }) {
  const [text, setText] = useState<string | null>(null);
  return (
    <Field
      label="Seed"
      hint={seed == null ? "Blank: a new random seed each time. The same seed and settings give the same result." : undefined}
      actions={
        seed != null && (
          <Button variant="ghost" size="xs" className="text-muted-foreground" onClick={() => onChange(null)}>
            <X /> Random
          </Button>
        )
      }
    >
      <div className="flex gap-1.5">
        <Input
          inputMode="numeric"
          className="h-9 tabular-nums"
          placeholder="random"
          value={text ?? (seed == null ? "" : String(seed))}
          onChange={(e) => {
            setText(e.target.value);
            const n = parseInt(e.target.value, 10);
            onChange(e.target.value.trim() && isFinite(n) && n >= 0 ? n : null);
          }}
          onBlur={() => setText(null)}
        />
        <Button variant="outline" size="icon" className="size-9 shrink-0" title="Pick a seed" onClick={() => onChange(Math.floor(Math.random() * 1_000_000_000))}>
          <Dices />
        </Button>
      </div>
    </Field>
  );
}
