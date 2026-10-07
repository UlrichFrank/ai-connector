import { AudioLines, Info, SquareUserRound, Type } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import type { Job } from "@/api/types";
import { CreateButton } from "@/components/kit/JobBits";
import { Advanced, CreateLayout, Empty, Field, SectionHeader } from "@/components/kit/Layout";
import { ModelPicker, useModelChoice } from "@/components/kit/ModelPicker";
import { PromptBox } from "@/components/kit/PromptBox";
import { RunOn, runOnWorker } from "@/components/kit/RunOn";
import { MediaViewer } from "@/components/media/MediaViewer";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { duration } from "@/lib/format";
import { submitEach } from "@/lib/jobs";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";
import { estimateSeconds, ModelNote, moods, SeedField, Segmented, speaks, useVoiceLibrary, VoiceLibrary, VoicePicker } from "@/features/voice";
import { AudioSource } from "./AudioSource";
import { AvatarCard } from "./AvatarCard";
import { avatarFrom, avatarRequest, defaultAvatar, modelHint, sizeOf, sizes, waitHint, type AvatarForm, type SizeId } from "./form";
import { Portrait } from "./Portrait";

const page = 24;

/**
 * Avatar: a portrait that speaks. The speech comes from a text (said with a voice first) or from audio; a scene
 * prompt may say how the person moves. `arg`: a job id whose settings to load.
 */
export function AvatarPage({ arg }: { arg: string }) {
  const choice = useModelChoice("avatar");
  const speechChoice = useModelChoice("speech", "avatar", speaks);
  const [form, setForm] = useState<AvatarForm>(() => ({ ...defaultAvatar, ...local.get<Partial<AvatarForm>>("avatar.form", {}) }));
  const [busy, setBusy] = useState(false);
  const [libOpen, setLibOpen] = useState(false);
  const [viewing, setViewing] = useState<string | null>(null);
  const [highlight, setHighlight] = useState("");
  const [audioLen, setAudioLen] = useState(0);
  const results = useRef<HTMLDivElement>(null);
  const set = (p: Partial<AvatarForm>) => setForm((f) => ({ ...f, ...p }));
  useVoiceLibrary();

  useEffect(() => local.set("avatar.form", form), [form]);

  // the length of the audio, for the estimate
  useEffect(() => {
    if (form.source !== "audio" || !form.audio) return setAudioLen(0);
    const a = new Audio();
    a.preload = "metadata";
    a.onloadedmetadata = () => setAudioLen(isFinite(a.duration) ? a.duration : 0);
    a.src = form.audio;
  }, [form.source, form.audio]);

  const loadSettings = (j: Job) => {
    setForm((f) => avatarFrom(j, f));
    if (choice.models.some((m) => m.id === j.model)) choice.set([j.model]);
    if (j.request.speechModel && speechChoice.models.some((m) => m.id === j.request.speechModel)) speechChoice.set([j.request.speechModel]);
    toast.success("Settings loaded", { description: "Portrait, speech, scene and model are in the composer." });
  };

  const target = useApp((s) => (arg ? s.jobs[arg] : undefined));
  const applied = useRef("");
  useEffect(() => {
    if (!target || applied.current === target.id || target.kind !== "avatar") return;
    applied.current = target.id;
    loadSettings(target);
    setHighlight(target.id);
    setTimeout(() => document.getElementById(`avatar-${target.id}`)?.scrollIntoView({ behavior: "smooth", block: "center" }), 120);
    location.replace("#/avatar");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [target?.id]);

  const jobs = useApp((s) => s.jobs);
  const list = useMemo(
    () =>
      Object.values(jobs)
        .filter((j) => j.kind === "avatar")
        .sort((a, b) => Date.parse(b.created) - Date.parse(a.created)),
    [jobs],
  );
  const [limit, setLimit] = useState(page);

  const speechOk = form.source === "text" ? !!form.text.trim() : !!form.audio;
  const ready = !!form.portrait && speechOk;
  const missing = !form.portrait ? "Add a portrait" : !speechOk ? (form.source === "text" ? "Type what they say" : "Add the audio") : "";

  const create = async () => {
    if (!ready || busy) return;
    setBusy(true);
    const made = await submitEach(choice.ids, { ...avatarRequest(form, speechChoice.ids[0]), worker: runOnWorker("avatar") });
    setBusy(false);
    if (made.length && window.matchMedia("(max-width: 1023px)").matches) results.current?.scrollIntoView({ behavior: "smooth" });
  };

  const m = choice.primary;
  const seconds = form.source === "text" ? estimateSeconds(form.text) : audioLen;
  const { w, h } = sizeOf(form);
  const wait = waitHint(m, seconds);

  const composer = (
    <>
      <Field label="Portrait">
        <Portrait url={form.portrait} onChange={(portrait, portraitW, portraitH) => set({ portrait, portraitW, portraitH })} />
      </Field>

      <Field label="What they say">
        <Segmented
          value={form.source}
          onChange={(source) => set({ source })}
          options={[
            { id: "text", label: (<><Type /> Type text</>) },
            { id: "audio", label: (<><AudioLines /> Use audio</>) },
          ]}
        />
      </Field>

      {form.source === "text" ? (
        <div className="-mt-2 flex flex-col gap-4 rounded-lg border bg-background/40 p-3">
          <div className="relative">
            <Textarea
              value={form.text}
              onChange={(e) => set({ text: e.target.value })}
              rows={4}
              className="min-h-24 resize-y pb-6"
              placeholder="Hi! I’m your new assistant. Ask me anything about the weekend’s plans."
              onKeyDown={(e) => (e.metaKey || e.ctrlKey) && e.key === "Enter" && (e.preventDefault(), void create())}
            />
            {form.text.trim() && <span className="pointer-events-none absolute right-2.5 bottom-1.5 text-[11px] text-muted-foreground tabular-nums">≈ {duration(Math.max(1, seconds))}</span>}
          </div>
          <Field label="Voice">
            <VoicePicker value={form.voice} onChange={(voice) => set({ voice })} model={speechChoice.primary} onManage={() => setLibOpen(true)} />
          </Field>
          <Field label="How it should sound">
            <div className="flex flex-wrap gap-1.5">
              {moods.map((x) => {
                const on = form.instruct === x.text;
                return (
                  <button
                    key={x.label}
                    type="button"
                    onClick={() => set({ instruct: on ? "" : x.text })}
                    className={cn(
                      "rounded-full border px-2.5 py-1 text-xs transition-colors",
                      on ? "border-primary bg-primary/10 text-foreground" : "text-muted-foreground hover:border-primary/50 hover:text-foreground",
                    )}
                  >
                    {x.label}
                  </button>
                );
              })}
            </div>
          </Field>
          <Field label="Speech model">
            <ModelPicker choice={speechChoice} multiple={false} className="w-full" />
          </Field>
        </div>
      ) : (
        <div className="-mt-2">
          <AudioSource url={form.audio} name={form.audioName} onChange={(audio, audioName) => set({ audio, audioName })} />
        </div>
      )}

      <PromptBox
        label="Scene and motion (optional)"
        value={form.prompt}
        onChange={(prompt) => set({ prompt })}
        enhance="enhance-video"
        rows={3}
        className="[&_textarea]:min-h-20 [&_textarea]:resize-y"
        placeholder="Nods while speaking, a warm smile, soft window light, slight camera push-in"
      />

      <Field label="Model">
        <ModelPicker choice={choice} className="w-full" />
        {m && (
          <p className="flex items-start gap-1.5 text-xs text-muted-foreground">
            <Info className="mt-px size-3.5 shrink-0" />
            <span>{modelHint(m)}</span>
          </p>
        )}
        <ModelNote models={choice.models.filter((x) => choice.ids.includes(x.id))} />
      </Field>

      <Field label="Size" hint={`${w} × ${h}${seconds ? ` · about ${duration(seconds)} of video` : ""}${wait ? ` · ${wait}` : ""}`}>
        <Segmented<SizeId>
          value={form.size}
          onChange={(size) => set({ size })}
          options={[{ id: "match", label: "Auto", title: "The portrait’s own shape" }, ...sizes.map((s) => ({ id: s.id, label: s.label }))]}
          className="[&_button]:px-1"
        />
      </Field>

      <Advanced id="avatar" summary={form.seed != null ? `seed ${form.seed}` : "random seed"}>
        <SeedField seed={form.seed} onChange={(seed) => set({ seed })} />
        <RunOn page="avatar" />
      </Advanced>
    </>
  );

  return (
    <>
      <CreateLayout
        composer={composer}
        footer={
          <div className="flex flex-col gap-1.5">
            <CreateButton count={choice.ids.length} busy={busy} disabled={!ready} onClick={create} />
            {missing && <p className="text-center text-xs text-muted-foreground">{missing} to start.</p>}
          </div>
        }
      >
        <div ref={results} className="mx-auto flex max-w-5xl scroll-mt-4 flex-col">
          <SectionHeader
            title={
              <>
                Avatars <span className="font-normal text-muted-foreground">· {list.length}</span>
              </>
            }
          />
          {list.length ? (
            <>
              <div className="grid grid-cols-[repeat(auto-fill,minmax(280px,1fr))] items-start gap-4">
                {list.slice(0, limit).map((j) => (
                  <AvatarCard key={j.id} job={j} onOpen={() => setViewing(j.id)} onUseAgain={loadSettings} highlight={highlight === j.id} />
                ))}
              </div>
              {limit < list.length && (
                <Button variant="ghost" className="mt-3 self-center text-muted-foreground" onClick={() => setLimit(limit + page)}>
                  Show more ({list.length - limit})
                </Button>
              )}
            </>
          ) : (
            <Empty icon={<SquareUserRound />} title="No avatars yet">
              Add a portrait, type what they should say (or use a recording), and press Create. The speech is made first, then the face is animated to it.
            </Empty>
          )}
        </div>
      </CreateLayout>

      <VoiceLibrary open={libOpen} onOpenChange={setLibOpen} onUse={(v) => set({ voice: v.id, source: "text" })} />
      <MediaViewer items={list} id={viewing} onNavigate={setViewing} onClose={() => setViewing(null)} onUseAgain={loadSettings} />
    </>
  );
}
