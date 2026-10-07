import { RotateCcw } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import type { Job, Model, Request } from "@/api/types";
import { CreateButton } from "@/components/kit/JobBits";
import { Field } from "@/components/kit/Layout";
import { ModelPicker, useModelChoice } from "@/components/kit/ModelPicker";
import { PromptBox } from "@/components/kit/PromptBox";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Slider } from "@/components/ui/slider";
import { Textarea } from "@/components/ui/textarea";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { submitEach } from "@/lib/jobs";
import { navigate } from "@/lib/route";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";
import { canDo, clock, isUpload, songLen, songScore, songTitle, TOOLS, trackName, TRACKS, type Task, type Tool } from "./songs";
import { useStudio } from "./state";

// one stable filter per task (useModelChoice memoises on it)
const filters = Object.fromEntries(TOOLS.map((t) => [t.task, (m: Model) => canDo(m, t.task)])) as Record<Task, (m: Model) => boolean>;

/** The studio: a tab per tool the models here can do; each makes a new version of the song. */
export function Tools({ job: j, file, length }: { job: Job; file: string; length: number }) {
  const models = useApp((s) => s.models);
  const tools = useMemo(
    () =>
      TOOLS.filter((t) => {
        if (t.id === "vary") return !isUpload(j);
        if (t.id === "restyle" && !songScore(j)) return false;
        return models.some((m) => m.kind === "music" && canDo(m, t.task));
      }),
    [models, j],
  );
  const toolId = useStudio((s) => s.tool);
  const setTool = useStudio((s) => s.setTool);
  const tool = tools.find((t) => t.id === toolId) ?? tools[0];
  if (!tool) return null;
  return (
    <section className="flex flex-col gap-3">
      <div className="flex flex-wrap gap-0.5">
        {tools.map((t) => (
          <button
            key={t.id}
            onClick={() => setTool(t.id)}
            className={cn(
              "shrink-0 rounded-full border px-2.5 py-1 text-[13px] font-medium transition-colors",
              t.id === tool.id ? "border-primary bg-primary/15 text-foreground" : "border-transparent text-muted-foreground hover:bg-accent hover:text-foreground",
            )}
          >
            {t.label}
          </button>
        ))}
      </div>
      <div className="rounded-xl border bg-card/50 p-4">
        <p className="mb-4 text-sm text-muted-foreground">{tool.note}</p>
        <ToolForm key={j.id + tool.id} job={j} tool={tool} file={file} length={length} />
      </div>
    </section>
  );
}

function TrackSelect({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger className="w-full">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {TRACKS.map((t) => (
          <SelectItem key={t} value={t}>
            {trackName(t)}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

function ToolForm({ job: j, tool, file, length }: { job: Job; tool: Tool; file: string; length: number }) {
  const r = j.request;
  const choice = useModelChoice("music", tool.task === "generate" ? "task.vary" : `task.${tool.task}`, filters[tool.task]);
  // without a choice of its own, a tool starts with the model that made the song (if it can)
  useEffect(() => {
    if (!local.get<string[]>(`models.music.${tool.task === "generate" ? "task.vary" : `task.${tool.task}`}`, []).length && choice.models.some((m) => m.id === j.model)) choice.set([j.model]);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const [style, setStyle] = useState(r.prompt ?? "");
  const [lyrics, setLyrics] = useState(tool.id === "remix" || tool.id === "restyle" ? (r.lyrics ?? "") : "");
  const [strength, setStrength] = useState(0.7);
  const [extend, setExtend] = useState(30);
  const [track, setTrack] = useState(tool.id === "stems" ? "vocals" : "guitar");
  const [count, setCount] = useState("2");
  const [busy, setBusy] = useState(false);
  const sel = useStudio((s) => (s.sel?.song === j.id ? s.sel : null));
  const setSel = useStudio((s) => s.setSel);
  const score = useScore(tool.id === "restyle" ? songScore(j) : undefined);
  const [abc, setAbc] = useState<string | null>(null);
  const abcText = abc ?? score.text;

  const needsSel = tool.id === "replace";
  const usesSel = tool.id === "replace" || tool.id === "lego";
  const lyricsOk = !r.instrumental;

  const go = async () => {
    setBusy(true);
    try {
      if (tool.id === "vary") {
        const { model: _, ...rest } = r;
        const req: Omit<Request, "model"> = { ...rest, seed: undefined, source: undefined, task: undefined, parent: j.id, title: songTitle(j) };
        for (let i = 0; i < +count; i++) await submitEach(choice.ids, req);
        return;
      }
      const range = usesSel ? (sel ? { start: sel.start, end: sel.end } : { start: 0, end: +length.toFixed(1) }) : {};
      const jobs = await submitEach(choice.ids, {
        task: tool.task,
        source: file,
        parent: j.id,
        title: songTitle(j),
        duration: songLen(j) || undefined,
        prompt: tool.id === "stems" ? r.prompt : style.trim(),
        lyrics: tool.id === "stems" || tool.id === "lego" ? undefined : lyrics.trim() || undefined,
        instrumental: r.instrumental,
        ...range,
        strength: tool.id === "remix" ? strength : undefined,
        track: tool.id === "stems" || tool.id === "lego" ? track : undefined,
        extend: tool.id === "extend" ? extend : undefined,
        abc: tool.id === "restyle" && abc != null && abc.trim() !== score.text.trim() ? abc : undefined,
      });
      if (jobs[0]) navigate("music", jobs[0].id);
    } finally {
      setBusy(false);
    }
  };

  const n = choice.ids.length * (tool.id === "vary" ? +count : 1);
  return (
    <div className="flex flex-col gap-4">
      <Field label="Model">
        <ModelPicker choice={choice} className="w-full sm:w-72" />
      </Field>

      {usesSel && (
        <Field
          label={tool.id === "lego" ? "Over" : "Part to replace"}
          hint={!sel ? (needsSel ? "Drag over the waveform above to choose the part." : "The whole song; drag over the waveform to play it over a part only.") : undefined}
        >
          <div className="flex items-center gap-2">
            <TimeInput label="From" value={sel?.start} placeholder="0:00.0" onChange={(v) => setSel({ song: j.id, start: v, end: Math.max(v + 0.5, sel?.end ?? length) })} />
            <span className="text-muted-foreground">–</span>
            <TimeInput label="To" value={sel?.end} placeholder={clock(length)} onChange={(v) => setSel({ song: j.id, start: Math.min(sel?.start ?? 0, v - 0.5), end: v })} />
          </div>
        </Field>
      )}

      {tool.id === "remix" && (
        <Field label={<>Keep of the original <span className="ml-1 font-normal text-foreground tabular-nums">{Math.round(strength * 100)}%</span></>} hint="Less keeps only the outline; more stays close to the song.">
          <Slider min={0.1} max={1} step={0.05} value={[strength]} onValueChange={([v]) => setStrength(v)} className="py-2" />
        </Field>
      )}

      {tool.id === "extend" && (
        <Field label={<>Add <span className="ml-1 font-normal text-foreground tabular-nums">{extend} s</span></>}>
          <Slider min={5} max={120} step={5} value={[extend]} onValueChange={([v]) => setExtend(v)} className="py-2" />
        </Field>
      )}

      {(tool.id === "stems" || tool.id === "lego") && (
        <Field label={tool.id === "stems" ? "Pull out" : "Instrument"}>
          <TrackSelect value={track} onChange={setTrack} />
        </Field>
      )}

      {tool.id === "vary" && (
        <Field label="How many">
          <ToggleGroup type="single" variant="outline" size="sm" value={count} onValueChange={(v) => v && setCount(v)} className="w-fit">
            {["1", "2", "3", "4"].map((c) => (
              <ToggleGroupItem key={c} value={c} className="w-10">
                {c}
              </ToggleGroupItem>
            ))}
          </ToggleGroup>
        </Field>
      )}

      {tool.id !== "stems" && tool.id !== "vary" && (
        <PromptBox
          label={{ remix: "New style", replace: "Style for this part", extend: "Style", lego: "How it should sound", restyle: "New style" }[tool.id] ?? "Style"}
          enhance="enhance-music"
          value={style}
          onChange={setStyle}
          rows={2}
          placeholder={tool.id === "lego" ? "warm fingerpicked acoustic guitar" : "the style of the new version"}
        />
      )}

      {lyricsOk && (tool.id === "remix" || tool.id === "replace" || tool.id === "extend" || tool.id === "restyle") && (
        <PromptBox
          label={{ remix: "Lyrics", replace: "Lyrics for this part", extend: "Lyrics for the new part", restyle: "Lyrics" }[tool.id] ?? "Lyrics"}
          value={lyrics}
          onChange={setLyrics}
          rows={tool.id === "remix" || tool.id === "restyle" ? 6 : 3}
          className="[&_textarea]:font-mono [&_textarea]:text-[13px]"
          placeholder={tool.id === "remix" ? "the song's lyrics" : "[verse]\n…"}
        />
      )}

      {tool.id === "restyle" && (
        <Field
          label="Melody score (ABC notation)"
          actions={
            abc != null && abc !== score.text ? (
              <Button variant="ghost" size="xs" onClick={() => setAbc(null)}>
                <RotateCcw /> Reset score
              </Button>
            ) : null
          }
        >
          <Textarea rows={8} value={abcText} onChange={(e) => setAbc(e.target.value)} placeholder={score.loading ? "loading the score …" : "no score"} className="font-mono text-xs" />
        </Field>
      )}

      <CreateButton
        count={n}
        busy={busy}
        disabled={(needsSel && !sel) || (tool.id === "restyle" && !abcText.trim())}
        onClick={go}
        label={tool.id === "vary" ? "Make variations" : tool.label}
      />
    </div>
  );
}

/** A time in seconds, typed as 1:23.4 or 83.4. */
function TimeInput({ label, value, placeholder, onChange }: { label: string; value?: number; placeholder: string; onChange: (v: number) => void }) {
  const [text, setText] = useState<string | null>(null);
  const shown = text ?? (value != null ? clock(value) : "");
  const commit = () => {
    if (text == null) return;
    const m = text.trim().match(/^(?:(\d+):)?(\d+(?:\.\d+)?)$/);
    if (m) onChange(+(Number(m[1] ?? 0) * 60 + Number(m[2])).toFixed(1));
    setText(null);
  };
  return (
    <Input
      aria-label={label}
      value={shown}
      placeholder={placeholder}
      onChange={(e) => setText(e.target.value)}
      onBlur={commit}
      onKeyDown={(e) => e.key === "Enter" && commit()}
      className="w-28 text-center tabular-nums"
    />
  );
}

/** The song's ABC melody score, fetched once. */
function useScore(url?: string) {
  const [s, setS] = useState({ url: "", text: "" });
  useEffect(() => {
    if (!url) return;
    let live = true;
    fetch(url, { credentials: "same-origin" })
      .then((r) => (r.ok ? r.text() : ""))
      .catch(() => "")
      .then((text) => live && setS({ url, text }));
    return () => {
      live = false;
    };
  }, [url]);
  return { text: s.url === url ? s.text : "", loading: !!url && s.url !== url };
}
