import { Boxes, RotateCw, Search } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import type { Model, ModelWorker } from "@/api/types";
import { Empty, ErrorNotice, Page, PageHeader } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { bytes } from "@/lib/format";
import { t } from "@/lib/i18n";
import { rankWorkers, whereText } from "@/lib/specs";
import { cn } from "@/lib/utils";
import { useLive } from "@/store/live";

/** The kinds clients use most, first. */
const kindOrder = ["chat", "embed", "classify", "image", "video", "music", "speech", "sound", "avatar"];

export function ModelsPage() {
  const models = useLive((s) => s.models);
  const error = useLive((s) => s.modelsError);
  const loaded = useLive((s) => s.modelsLoaded);
  const refresh = useLive((s) => s.refreshModels);
  const [kind, setKind] = useState("all");
  const [q, setQ] = useState("");
  useEffect(() => void refresh(), [refresh]);

  const kinds = useMemo(() => {
    const ks = [...new Set(models.map((m) => m.kind))];
    return ks.sort((a, b) => (kindOrder.indexOf(a) + 1 || 99) - (kindOrder.indexOf(b) + 1 || 99));
  }, [models]);
  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return models
      .filter((m) => kind === "all" || m.kind === kind)
      .filter((m) => !needle || m.id.toLowerCase().includes(needle) || m.name.toLowerCase().includes(needle) || m.backend.toLowerCase().includes(needle))
      .sort((a, b) => (kindOrder.indexOf(a.kind) + 1 || 99) - (kindOrder.indexOf(b.kind) + 1 || 99) || a.id.localeCompare(b.id));
  }, [models, kind, q]);

  return (
    <Page wide>
      <PageHeader title={t("Models")} description={t("Every model the connected workers offer, and where each can run. Clients name a model by its id.")}>
        <Button variant="outline" onClick={() => void refresh()}>
          <RotateCw /> {t("Refresh")}
        </Button>
      </PageHeader>

      {error && !models.length ? (
        <ErrorNotice error={error} onRetry={refresh} what="The model list" />
      ) : !loaded ? null : !models.length ? (
        <Empty icon={Boxes} title={t("No models")}>
          {t("The models come from the connected workers. Connect one on the Workers page.")}
        </Empty>
      ) : (
        <>
          <div className="mb-3 flex flex-wrap items-center gap-2">
            <div className="relative w-full sm:w-64">
              <Search className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
              <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder={t("Search models")} className="pl-8" aria-label={t("Search models")} />
            </div>
            <div className="flex flex-wrap gap-1">
              {["all", ...kinds].map((k) => (
                <button
                  key={k}
                  onClick={() => setKind(k)}
                  aria-pressed={kind === k}
                  className={cn("min-h-8 rounded-full border border-[var(--line-2)] bg-[var(--surface)] px-3 text-xs text-muted-foreground transition-colors hover:text-foreground", kind === k && "border-primary/40 bg-primary/10 text-[var(--teal-ink)]")}
                >
                  {k === "all" ? t("all") : k} {k === "all" ? models.length : models.filter((m) => m.kind === k).length}
                </button>
              ))}
            </div>
          </div>
          <div className="card overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr>
                  <th className="px-3 py-2.5 font-medium">{t("Model")}</th>
                  <th className="px-3 py-2.5 font-medium">{t("Kind")}</th>
                  <th className="px-3 py-2.5 font-medium max-md:hidden">{t("Backend")}</th>
                  <th className="px-3 py-2.5 font-medium">{t("Workers")}</th>
                </tr>
              </thead>
              <tbody>
                {shown.map((m) => (
                  <ModelRow key={m.id} m={m} />
                ))}
              </tbody>
            </table>
            {!shown.length && <div className="p-6 text-center text-sm text-muted-foreground">{t("No model matches.")}</div>}
          </div>
        </>
      )}
    </Page>
  );
}

function ModelRow({ m }: { m: Model }) {
  const ws = rankWorkers(m.workers ?? []);
  return (
    <tr className="border-t align-top" data-model={m.id}>
      <td className="max-w-0 px-3 py-2 sm:w-[40%]">
        <div className="truncate font-medium" title={m.name}>
          {m.name || m.id}
        </div>
        <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <code className="truncate font-mono" title={m.id}>
            {m.id}
          </code>
          {m.size ? <span className="shrink-0">· {bytes(m.size)}</span> : null}
          {m.nonCommercial && <span className="shrink-0 rounded bg-amber-500/15 px-1 text-[10px] text-amber-700 dark:text-amber-400">{t("non-commercial")}</span>}
        </div>
      </td>
      <td className="px-3 py-2">
        <span className="rounded bg-muted px-1.5 py-0.5 text-xs">{m.kind}</span>
      </td>
      <td className="px-3 py-2 text-muted-foreground max-md:hidden">{m.backend}</td>
      <td className="px-3 py-2">
        {ws.length ? (
          <div className="flex flex-wrap gap-1">
            {ws.map((w) => (
              <WorkerChip key={w.id} w={w} />
            ))}
          </div>
        ) : (
          <span className="text-xs text-muted-foreground">{m.here ? t("on disk") : "—"}</span>
        )}
      </td>
    </tr>
  );
}

function WorkerChip({ w }: { w: ModelWorker }) {
  const tone = !w.fits ? "text-muted-foreground line-through decoration-muted-foreground/50" : w.loaded ? "border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400" : w.here ? "" : "text-muted-foreground";
  return (
    <span className={cn("inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs", tone)} title={`${w.name}: ${whereText(w)}`}>
      {w.name}
      <span className="text-[10px] opacity-70">{whereText(w)}</span>
    </span>
  );
}
