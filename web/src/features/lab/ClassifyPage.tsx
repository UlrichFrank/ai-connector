import { PencilLine, Plus, Tags, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import type { Job, Option } from "@/api/types";
import { ExamplesMenu } from "@/components/kit/Examples";
import { CreateButton, JobError, JobProgress } from "@/components/kit/JobBits";
import { CreateLayout, Empty, Field, SectionHeader } from "@/components/kit/Layout";
import { ModelPicker, useModelChoice } from "@/components/kit/ModelPicker";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { duration, took } from "@/lib/format";
import { submitEach } from "@/lib/jobs";
import { navigate } from "@/lib/route";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";
import { isActive, useApp } from "@/store/app";
import { ConnectionNotice, RunHistory, Stat, useRuns } from "./shared";
import { TextList, useTextList } from "./TextList";

interface Answer {
  text: string;
  choice: string;
  confidence: number;
  probabilities: Record<string, number>;
}

const answersOf = (j: Job): Answer[] | null => {
  const a = (j.result as { answers?: Answer[] } | undefined)?.answers;
  return Array.isArray(a) ? a : null;
};

interface OptionRow extends Option {
  id: number;
}
let nextOption = 1;
const rows = (o: Option[]): OptionRow[] => o.map((x) => ({ label: x.label, description: x.description ?? "", id: nextOption++ }));

/** Lab → Classify: asks one multiple-choice question about each text (Laya picks an option and says how sure it is). */
export function ClassifyPage({ arg }: { arg: string }) {
  const choice = useModelChoice("classify");
  const texts = useTextList("draft.classify.texts");
  const [question, setQuestion] = useState(() => local.get("draft.classify.question", ""));
  const [options, setOptions] = useState<OptionRow[]>(() => rows(local.get<Option[]>("draft.classify.options", [{ label: "" }, { label: "" }])));
  const { runs, current, missing } = useRuns("classify", arg);
  const [busy, setBusy] = useState(false);

  useEffect(() => local.set("draft.classify.question", question), [question]);
  useEffect(() => local.set("draft.classify.options", options.map(({ label, description }) => ({ label, description }))), [options]);

  const valid = options.filter((o) => o.label.trim());
  const ready = !!question.trim() && valid.length >= 2 && texts.texts.length > 0;

  const fill = (q: string, o: Option[], t: string[]) => {
    setQuestion(q);
    setOptions(rows(o.length ? o : [{ label: "" }, { label: "" }]));
    texts.set(t);
  };

  const run = async () => {
    setBusy(true);
    const [j] = await submitEach(choice.ids.slice(0, 1), {
      instructions: question.trim(),
      options: valid.map((o) => ({ label: o.label.trim(), ...(o.description?.trim() ? { description: o.description.trim() } : {}) })),
      states: texts.texts,
    });
    setBusy(false);
    if (j) navigate("classify", j.id);
  };

  const missingWhat = !question.trim() ? "Ask a question" : valid.length < 2 ? "Give at least two options" : !texts.texts.length ? "Add a text to classify" : "";

  return (
    <CreateLayout
      composer={
        <>
          <div className="flex items-start gap-2">
            <div className="min-w-0 flex-1">
              <h1 className="text-base font-semibold">Classify texts</h1>
              <p className="mt-1 text-sm text-muted-foreground">Ask one question with fixed answers; the model picks an answer for every text.</p>
            </div>
            <ExamplesMenu kind="classify" onPick={(e) => fill(e.question ?? "", (e.options as Option[] | undefined) ?? [], e.texts ?? [])} />
          </div>
          <Field label="Question">
            <Textarea value={question} onChange={(e) => setQuestion(e.target.value)} rows={2} placeholder="e.g. Which team should handle this ticket?" className="min-h-0 resize-y" />
          </Field>
          <Field label={`Options · ${valid.length}`} hint="A short description helps the model tell close options apart.">
            <OptionsEditor options={options} setOptions={setOptions} />
          </Field>
          <Field label={`Texts${texts.texts.length ? ` · ${texts.texts.length}` : ""}`} hint="Each text is classified on its own. Pasting several lines makes a row of each.">
            <TextList state={texts} placeholder="Add a text to classify …" />
          </Field>
          <Field label="Model">
            <ModelPicker choice={choice} multiple={false} />
          </Field>
        </>
      }
      footer={
        <div className="flex flex-col gap-1.5">
          <CreateButton count={choice.ids.length ? 1 : 0} busy={busy} disabled={!ready} onClick={run} label={ready ? `Classify ${texts.texts.length} ${texts.texts.length === 1 ? "text" : "texts"}` : "Classify"} />
          {!ready && <p className="text-center text-xs text-muted-foreground">{missingWhat}.</p>}
        </div>
      }
    >
      <ConnectionNotice className="mb-5" />
      {current ? (
        <Result key={current.id} job={current} onEdit={(j) => fill(j.request.instructions ?? "", j.request.options ?? [], j.request.states ?? [])} />
      ) : missing ? (
        <Empty icon={<Tags />} title="This run is gone">
          It was deleted, or it is still loading.
        </Empty>
      ) : (
        <Empty icon={<Tags />} title="Nothing classified yet">
          Write a question, give the possible answers and add the texts to sort — or start from an example.
        </Empty>
      )}
      <RunHistory
        runs={runs}
        current={current}
        page="classify"
        summary={(j) => ({
          title: j.request.instructions || "Untitled question",
          meta: `${j.request.states?.length ?? 0} texts · ${(j.request.options ?? []).map((o) => o.label).join(" / ")}`,
        })}
      />
    </CreateLayout>
  );
}

function OptionsEditor({ options, setOptions }: { options: OptionRow[]; setOptions: React.Dispatch<React.SetStateAction<OptionRow[]>> }) {
  const refs = useRef(new Map<number, HTMLInputElement>());
  const [focus, setFocus] = useState<number | null>(null);
  useEffect(() => {
    if (focus != null) refs.current.get(focus)?.focus();
    setFocus(null);
  }, [focus]);

  const update = (id: number, patch: Partial<Option>) => setOptions((l) => l.map((o) => (o.id === id ? { ...o, ...patch } : o)));
  const add = () => {
    const [r] = rows([{ label: "" }]);
    setOptions((l) => [...l, r]);
    setFocus(r.id);
  };
  return (
    <div className="flex flex-col gap-2">
      <ul className="flex flex-col overflow-hidden rounded-lg border bg-background/40 dark:bg-input/20">
        {options.map((o, i) => (
          <li key={o.id} className="group flex items-start border-b last:border-b-0 focus-within:bg-accent/40">
            <span className="grid h-9 w-9 shrink-0 place-items-center">
              <span className="size-2 rounded-full bg-primary/70" />
            </span>
            <div className="flex min-w-0 flex-1 flex-col py-1">
              <input
                ref={(el) => void (el ? refs.current.set(o.id, el) : refs.current.delete(o.id))}
                value={o.label}
                onChange={(e) => update(o.id, { label: e.target.value })}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    if (i === options.length - 1) add();
                    else setFocus(options[i + 1].id);
                  }
                }}
                placeholder={`Option ${i + 1}`}
                aria-label={`Option ${i + 1}`}
                className="h-7 min-w-0 bg-transparent text-sm font-medium outline-none placeholder:font-normal placeholder:text-muted-foreground/70"
              />
              <input
                value={o.description ?? ""}
                onChange={(e) => update(o.id, { description: e.target.value })}
                placeholder="Description (optional)"
                aria-label={`Description of option ${i + 1}`}
                className="h-6 min-w-0 bg-transparent text-xs text-muted-foreground outline-none placeholder:text-muted-foreground/50"
              />
            </div>
            <button
              type="button"
              onClick={() => setOptions((l) => l.filter((x) => x.id !== o.id))}
              aria-label={`Remove option ${i + 1}`}
              className="m-1 mt-2 grid size-7 shrink-0 place-items-center rounded-md text-muted-foreground opacity-60 hover:bg-accent hover:text-foreground hover:opacity-100 focus-visible:opacity-100 sm:opacity-0 sm:group-hover:opacity-100"
            >
              <X className="size-3.5" />
            </button>
          </li>
        ))}
      </ul>
      <Button variant="outline" size="sm" className="self-start" onClick={add} disabled={options.length >= 16}>
        <Plus /> Add option
      </Button>
    </div>
  );
}

function Result({ job: j, onEdit }: { job: Job; onEdit: (j: Job) => void }) {
  const answers = answersOf(j);
  const model = useApp((s) => s.models.find((m) => m.id === j.model));
  const labels = (j.request.options ?? []).map((o) => o.label);
  const secs = Number(j.stats?.seconds);
  const counts = new Map<string, number>();
  for (const a of answers ?? []) counts.set(a.choice, (counts.get(a.choice) ?? 0) + 1);
  return (
    <div className="flex flex-col gap-6">
      <div>
        <h2 className="text-lg font-semibold text-balance">{j.request.instructions}</h2>
        <div className="mt-2 flex flex-wrap items-center gap-x-5 gap-y-1 text-xs">
          <Stat label="Texts">{j.request.states?.length ?? 0}</Stat>
          <Stat label="Model">{model?.name ?? j.model}</Stat>
          {isFinite(secs) && secs > 0 ? <Stat label="Answered in">{duration(secs)}</Stat> : j.finished && <Stat label="Took">{duration(took(j))}</Stat>}
          <div className="flex-1" />
          <Button variant="ghost" size="sm" className="-mr-2 text-muted-foreground" onClick={() => onEdit(j)}>
            <PencilLine /> Edit this question
          </Button>
        </div>
      </div>
      {isActive(j) && <JobProgress job={j} />}
      {(j.state === "failed" || j.state === "canceled") && <JobError job={j} />}
      {answers && (
        <>
          <div className="flex flex-wrap gap-1.5" aria-label="How many texts got each option">
            {labels.map((l) => (
              <span key={l} className={cn("inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs", counts.get(l) ? "bg-card" : "text-muted-foreground opacity-70")}>
                <span className="font-medium">{l}</span>
                <span className="tabular-nums text-muted-foreground">{counts.get(l) ?? 0}</span>
              </span>
            ))}
          </div>
          <section>
            <SectionHeader title="Answers" />
            <ol className="grid gap-3 [grid-template-columns:repeat(auto-fill,minmax(min(100%,22rem),1fr))]">
              {answers.map((a, i) => (
                <AnswerCard key={i} n={i + 1} answer={a} labels={labels} />
              ))}
            </ol>
          </section>
        </>
      )}
    </div>
  );
}

const pct = (p: number) => `${Math.round(p * 100)}%`;

function AnswerCard({ n, answer: a, labels }: { n: number; answer: Answer; labels: string[] }) {
  const order = labels.length ? labels : Object.keys(a.probabilities);
  const unsure = a.confidence < 0.5;
  return (
    <li className="flex flex-col gap-3 rounded-xl border bg-card/50 p-4">
      <div className="flex gap-2.5">
        <span className="mt-0.5 grid h-5 min-w-5 shrink-0 place-items-center rounded bg-muted px-1 text-[11px] tabular-nums text-muted-foreground">{n}</span>
        <p className="min-w-0 flex-1 text-sm break-words">{a.text}</p>
      </div>
      <div className="flex items-center gap-2">
        <span className="rounded-md bg-primary px-2 py-0.5 text-sm font-semibold text-primary-foreground">{a.choice}</span>
        <span className={cn("text-xs", unsure ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground")} title="How sure the model is of its answer">
          {unsure ? "unsure · " : ""}
          {pct(a.confidence)} sure
        </span>
      </div>
      <ul className="flex flex-col gap-1.5" aria-label="Probability of each option">
        {order.map((l) => {
          const p = a.probabilities[l] ?? 0;
          const chosen = l === a.choice;
          return (
            <li key={l} className="grid grid-cols-[minmax(0,7rem)_1fr_2.75rem] items-center gap-2 text-xs">
              <span className={cn("truncate", chosen ? "font-medium" : "text-muted-foreground")} title={l}>
                {l}
              </span>
              <span className="h-1.5 overflow-hidden rounded-full bg-muted">
                <span className={cn("block h-full rounded-full", chosen ? "bg-primary" : "bg-muted-foreground/45")} style={{ width: `${Math.max(p > 0 ? 1.5 : 0, p * 100)}%` }} />
              </span>
              <span className={cn("text-right tabular-nums", chosen ? "font-medium" : "text-muted-foreground")}>{pct(p)}</span>
            </li>
          );
        })}
      </ul>
    </li>
  );
}
