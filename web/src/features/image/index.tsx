import { Dices, Download, Lock, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import type { Job, Model } from "@/api/types";
import { ExamplesMenu } from "@/components/kit/Examples";
import { CreateButton } from "@/components/kit/JobBits";
import { Advanced, CreateLayout, Field } from "@/components/kit/Layout";
import { ModelPicker, useModelChoice } from "@/components/kit/ModelPicker";
import { PromptBox } from "@/components/kit/PromptBox";
import { RunOn, runOnWorker } from "@/components/kit/RunOn";
import { dims, mainFile } from "@/components/media/media";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { bytes } from "@/lib/format";
import { submitEach } from "@/lib/jobs";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";
import { defaultForm, formFrom, maxRefs, randomSeed, refsProblem, requestOf, snap, type ImageForm, type Quantize } from "./form";
import { Gallery } from "./Gallery";
import { References } from "./References";
import { SizePicker } from "./SizePicker";

/** Text to image (and editing with reference images) with FLUX.2 klein and Qwen-Image. `arg`: a job id whose settings to load. */
export function ImagePage({ arg }: { arg: string }) {
  const choice = useModelChoice("image");
  const [form, setForm] = useState<ImageForm>(() => ({ ...defaultForm, ...local.get<Partial<ImageForm>>("image.form", {}) }));
  const [busy, setBusy] = useState(false);
  const refsBox = useRef<HTMLDivElement>(null);
  const results = useRef<HTMLDivElement>(null);
  const set = (p: Partial<ImageForm>) => setForm((f) => ({ ...f, ...p }));

  useEffect(() => local.set("image.form", form), [form]);

  const loadSettings = (j: Job) => {
    setForm(formFrom(j));
    if (choice.models.some((m) => m.id === j.model)) choice.set([j.model]);
    toast.success("Settings loaded", { description: "Prompt, size, seed and model are in the composer." });
  };

  // #/image/<id> (e.g. "Use again" in the library): that image's settings
  const target = useApp((s) => (arg ? s.jobs[arg] : undefined));
  const applied = useRef("");
  useEffect(() => {
    if (!target || applied.current === target.id) return;
    applied.current = target.id;
    loadSettings(target);
    location.replace("#/image");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [target?.id]);

  const edit = (j: Job) => {
    const f = mainFile(j);
    if (!f) return;
    if (form.refs.includes(f)) return;
    if (form.refs.length >= maxRefs) return void toast.info(`Up to ${maxRefs} reference images`);
    // the first reference sets the shape: an edit keeps the picture's size
    const { w, h } = dims(j);
    set({ refs: [...form.refs, f], ...(form.refs.length ? {} : { width: snap(w), height: snap(h) }) });
    requestAnimationFrame(() => refsBox.current?.scrollIntoView({ behavior: "smooth", block: "center" }));
  };

  const m = choice.primary;
  const editing = form.refs.length > 0;
  const problem = refsProblem(
    choice.models.filter((x) => choice.ids.includes(x.id)),
    editing,
  );

  const create = async () => {
    if (!form.prompt.trim() || busy || problem) return;
    setBusy(true);
    const jobs = await submitEach(choice.ids, { ...requestOf(form), worker: runOnWorker("image") });
    setBusy(false);
    // on a phone the results are below the composer
    if (jobs.length && window.matchMedia("(max-width: 1023px)").matches) results.current?.scrollIntoView({ behavior: "smooth" });
  };


  const composer = (
    <>
      <Field label="Model">
        <ModelPicker choice={choice} className="w-full" />
        <ModelNote models={choice.models.filter((x) => choice.ids.includes(x.id))} />
      </Field>

      <PromptBox
        label={editing ? "What should change" : "Prompt"}
        value={form.prompt}
        onChange={(prompt) => set({ prompt })}
        enhance="enhance-image"
        rows={5}
        className="[&_textarea]:min-h-28 [&_textarea]:resize-y"
        placeholder={editing ? "Make it a winter evening, snow on the roofs, warm light in the windows" : "A cozy reading nook in a treehouse, warm evening light, film photo"}
        onKeyDown={(e) => (e.metaKey || e.ctrlKey) && e.key === "Enter" && (e.preventDefault(), void create())}
        actions={
          <ExamplesMenu
            kind="image"
            onPick={(e) => {
              const [w, h] = (e.size ?? "").split("x").map(Number);
              set({ prompt: e.prompt ?? "", ...(w && h ? { width: snap(w), height: snap(h) } : {}) });
            }}
          />
        }
      />

      <Field label="Reference images" hint={editing ? "The model edits these following the prompt." : undefined}>
        <References ref={refsBox} refs={form.refs} onChange={(refs) => set({ refs })} onAdd={(url) => setForm((f) => (f.refs.includes(url) ? f : { ...f, refs: [...f.refs, url] }))} />
        {problem && <p className="text-xs text-amber-700 dark:text-amber-400">{problem}</p>}
      </Field>

      <Field label="Size">
        <SizePicker width={form.width} height={form.height} onChange={(width, height) => set({ width, height })} />
      </Field>

      <Advanced id="image" summary={summary(form, m)}>
        <div className="grid grid-cols-2 gap-3">
          <Field label="Steps">
            <NumberInput value={form.steps} placeholder={String(m?.defaults.steps ?? "auto")} min={1} max={100} onChange={(steps) => set({ steps })} />
          </Field>
          <Field label="Guidance">
            <NumberInput value={form.guidance} placeholder={String(m?.defaults.guidance ?? "auto")} min={0} max={20} step={0.5} decimals onChange={(guidance) => set({ guidance })} />
          </Field>
        </div>
        <Field
          label="Seed"
          hint={form.seed == null ? "Blank: a new random seed each time. The same seed and settings make the same picture." : undefined}
          actions={
            form.seed != null && (
              <Button variant="ghost" size="xs" className="text-muted-foreground" onClick={() => set({ seed: null })}>
                <X /> Random
              </Button>
            )
          }
        >
          <div className="flex gap-1.5">
            <NumberInput value={form.seed} placeholder="random" min={0} onChange={(seed) => set({ seed })} />
            <Button variant="outline" size="icon" className="size-9 shrink-0" title="Pick a seed" onClick={() => set({ seed: randomSeed() })}>
              <Dices />
            </Button>
          </div>
        </Field>
        <Field label="Precision" hint="Lower precision needs less memory; full precision is slower and a little sharper.">
          <div role="radiogroup" className="grid grid-cols-3 gap-1 rounded-lg bg-muted/60 p-1">
            {(
              [
                [8, "8-bit"],
                [4, "4-bit"],
                [-1, "Full (bf16)"],
              ] as [Quantize, string][]
            ).map(([q, label]) => (
              <button
                key={q}
                type="button"
                role="radio"
                aria-checked={form.quantize === q}
                onClick={() => set({ quantize: q })}
                className={cn(
                  "rounded-md px-2 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground",
                  form.quantize === q && "bg-background text-foreground shadow-sm ring-1 ring-foreground/10",
                )}
              >
                {label}
              </button>
            ))}
          </div>
        </Field>
        <RunOn page="image" model={m} />
      </Advanced>
    </>
  );

  return (
    <CreateLayout
      composer={composer}
      footer={<CreateButton count={choice.ids.length} busy={busy} disabled={!form.prompt.trim() || !!problem} onClick={create} label={editing ? "Edit" : "Create"} />}
    >
      <div ref={results} className="scroll-mt-4">
        <Gallery onUseAgain={loadSettings} onEdit={edit} />
      </div>
    </CreateLayout>
  );
}

/** "4 steps · 8-bit · random seed" */
function summary(f: ImageForm, m?: Model): string {
  const steps = f.steps ?? m?.defaults.steps;
  return [steps ? `${steps} steps` : "", f.quantize === -1 ? "bf16" : `${f.quantize}-bit`, f.seed != null ? `seed ${f.seed}` : "random seed"].filter(Boolean).join(" · ");
}

/** What happens with a model that is not on this Mac yet. */
function ModelNote({ models }: { models: Model[] }) {
  const away = models.filter((m) => !m.here);
  if (!away.length) return null;
  return (
    <div className="flex flex-col gap-1">
      {away.map((m) => (
        <p key={m.id} className="flex items-start gap-1.5 text-xs text-amber-700 dark:text-amber-400">
          {m.gated ? <Lock className="mt-px size-3.5 shrink-0" /> : <Download className="mt-px size-3.5 shrink-0" />}
          <span>
            {m.name} is not downloaded yet ({bytes(m.size)}). Creating downloads it first{m.gated ? "; it needs a Hugging Face token (HF_TOKEN) with access" : ""}.
          </span>
        </p>
      ))}
    </div>
  );
}

/** A number field where blank means "the default". */
function NumberInput({
  value,
  onChange,
  placeholder,
  min,
  max,
  step = 1,
  decimals,
}: {
  value: number | null;
  onChange: (v: number | null) => void;
  placeholder?: string;
  min?: number;
  max?: number;
  step?: number;
  decimals?: boolean;
}) {
  const [text, setText] = useState<string | null>(null);
  return (
    <Input
      type="number"
      inputMode={decimals ? "decimal" : "numeric"}
      className="h-9 tabular-nums"
      value={text ?? (value == null ? "" : String(value))}
      placeholder={placeholder}
      min={min}
      max={max}
      step={step}
      onChange={(e) => {
        setText(e.target.value);
        const t = e.target.value.trim();
        if (!t) return onChange(null);
        const n = decimals ? parseFloat(t) : parseInt(t, 10);
        if (isFinite(n)) onChange(Math.max(min ?? -Infinity, Math.min(max ?? Infinity, n)));
      }}
      onBlur={() => setText(null)}
    />
  );
}
