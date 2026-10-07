import { FlaskConical, PencilLine } from "lucide-react";
import { useState } from "react";
import type { Job } from "@/api/types";
import { ExamplesMenu } from "@/components/kit/Examples";
import { CreateButton, JobError, JobProgress } from "@/components/kit/JobBits";
import { CreateLayout, Empty, Field, SectionHeader } from "@/components/kit/Layout";
import { ModelPicker, useModelChoice } from "@/components/kit/ModelPicker";
import { Button } from "@/components/ui/button";
import { duration, took } from "@/lib/format";
import { submitEach } from "@/lib/jobs";
import { navigate } from "@/lib/route";
import { isActive, useApp } from "@/store/app";
import { Heatmap, cellColor, fmt, pairs } from "./Heatmap";
import { ConnectionNotice, RunHistory, Stat, useRuns } from "./shared";
import { TextList, useTextList } from "./TextList";

interface EmbedResult {
  similarity: number[][];
  dim: number;
  texts: string[];
}

const resultOf = (j: Job): EmbedResult | null => {
  const r = j.result as Partial<EmbedResult> | undefined;
  return r?.similarity && r.texts ? (r as EmbedResult) : null;
};

/** Lab → Embeddings: how close in meaning some texts are, as a similarity matrix and a ranking of pairs. */
export function EmbedPage({ arg }: { arg: string }) {
  const choice = useModelChoice("embed");
  const texts = useTextList("draft.embed");
  const { runs, current, missing } = useRuns("embed", arg);
  const [busy, setBusy] = useState(false);
  const enough = texts.texts.length >= 2;

  const run = async () => {
    setBusy(true);
    const [j] = await submitEach(choice.ids.slice(0, 1), { texts: texts.texts });
    setBusy(false);
    if (j) navigate("embed", j.id);
  };

  return (
    <CreateLayout
      composer={
        <>
          <div>
            <h1 className="text-base font-semibold">Compare meanings</h1>
            <p className="mt-1 text-sm text-muted-foreground">An embedding model turns each text into a vector; texts that mean the same land close together, whatever the wording or language.</p>
          </div>
          <Field
            label={`Texts${texts.texts.length ? ` · ${texts.texts.length}` : ""}`}
            actions={
              <>
                {texts.list.length > 0 && (
                  <Button variant="ghost" size="sm" className="text-muted-foreground" onClick={() => texts.set([])}>
                    Clear
                  </Button>
                )}
                <ExamplesMenu kind="embed" onPick={(e) => e.texts && texts.set(e.texts)} />
              </>
            }
            hint="One text per row. Enter adds a row; pasting several lines makes a row of each."
          >
            <TextList state={texts} />
          </Field>
          <Field label="Model">
            <ModelPicker choice={choice} multiple={false} />
          </Field>
        </>
      }
      footer={
        <div className="flex flex-col gap-1.5">
          <CreateButton count={choice.ids.length ? 1 : 0} busy={busy} disabled={!enough} onClick={run} label={enough ? `Compare ${texts.texts.length} texts` : "Compare"} />
          {!enough && <p className="text-center text-xs text-muted-foreground">Add at least two texts.</p>}
        </div>
      }
    >
      <ConnectionNotice className="mb-5" />
      {current ? (
        <Result key={current.id} job={current} onEdit={(t) => texts.set(t)} />
      ) : missing ? (
        <Empty icon={<FlaskConical />} title="This run is gone">
          It was deleted, or it is still loading.
        </Empty>
      ) : (
        <Empty icon={<FlaskConical />} title="Nothing compared yet">
          Add a few texts on the left, or pick an example, then Compare. Paraphrases and translations of each other score close to 1.
        </Empty>
      )}
      <RunHistory
        runs={runs}
        current={current}
        page="embed"
        summary={(j) => {
          const t = j.request.texts ?? [];
          return { title: t.slice(0, 3).join(" · ") || "Untitled", meta: `${t.length} texts` };
        }}
      />
    </CreateLayout>
  );
}

function Result({ job: j, onEdit }: { job: Job; onEdit: (texts: string[]) => void }) {
  const r = resultOf(j);
  const model = useApp((s) => s.models.find((m) => m.id === j.model));
  const texts = r?.texts ?? j.request.texts ?? [];
  const secs = Number(j.stats?.seconds);
  return (
    <div className="flex flex-col gap-8">
      <div className="flex flex-wrap items-center gap-x-5 gap-y-1 text-xs">
        <Stat label="Texts">{texts.length}</Stat>
        {r && <Stat label="Dimensions">{r.dim}</Stat>}
        <Stat label="Model">{model?.name ?? j.model}</Stat>
        {isFinite(secs) && secs > 0 ? <Stat label="Embedded in">{duration(secs)}</Stat> : j.finished && <Stat label="Took">{duration(took(j))}</Stat>}
        <div className="flex-1" />
        <Button variant="ghost" size="sm" className="-mr-2 text-muted-foreground" onClick={() => onEdit(texts)}>
          <PencilLine /> Edit these texts
        </Button>
      </div>
      {isActive(j) && <JobProgress job={j} />}
      {(j.state === "failed" || j.state === "canceled") && <JobError job={j} />}
      {r && (
        <>
          <section>
            <SectionHeader title="Similarity" />
            <Heatmap texts={r.texts} sim={r.similarity} />
          </section>
          <NearestPairs texts={r.texts} sim={r.similarity} />
        </>
      )}
    </div>
  );
}

function NearestPairs({ texts, sim }: { texts: string[]; sim: number[][] }) {
  const all = pairs(sim);
  const [more, setMore] = useState(false);
  const shown = more ? all : all.slice(0, 6);
  if (!all.length) return null;
  return (
    <section>
      <SectionHeader title="Nearest pairs">
        <span className="text-xs text-muted-foreground">cosine similarity</span>
      </SectionHeader>
      <ol className="flex flex-col divide-y rounded-xl border bg-card/40">
        {shown.map((p, i) => (
          <li key={`${p.a}-${p.b}`} className="flex items-center gap-4 px-4 py-3">
            <span className="w-5 shrink-0 text-xs text-muted-foreground tabular-nums max-sm:hidden">{i + 1}.</span>
            <div className="flex min-w-0 flex-1 flex-col gap-1 text-sm">
              <PairText n={p.a} text={texts[p.a]} />
              <PairText n={p.b} text={texts[p.b]} />
            </div>
            <div className="flex w-28 shrink-0 flex-col items-end gap-1.5 max-sm:w-16">
              <span className="text-sm font-semibold tabular-nums">{fmt(p.v)}</span>
              <span className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
                <span className="block h-full rounded-full" style={{ width: `${Math.max(2, Math.max(0, p.v) * 100)}%`, background: cellColor(p.v) }} />
              </span>
            </div>
          </li>
        ))}
      </ol>
      {all.length > 6 && (
        <Button variant="ghost" size="sm" className="mt-2 text-muted-foreground" onClick={() => setMore(!more)}>
          {more ? "Show fewer" : `Show all ${all.length} pairs`}
        </Button>
      )}
    </section>
  );
}

function PairText({ n, text }: { n: number; text: string }) {
  return (
    <span className="flex min-w-0 gap-2">
      <span className="grid h-5 min-w-5 shrink-0 place-items-center rounded bg-muted px-1 text-[11px] tabular-nums text-muted-foreground">{n + 1}</span>
      <span className="min-w-0 max-sm:line-clamp-2 sm:truncate" title={text}>
        {text}
      </span>
    </span>
  );
}
