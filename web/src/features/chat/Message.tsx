import { t } from "@/lib/i18n";
import { Brain, Check, ChevronLeft, ChevronRight, ChevronsDownUp, ChevronsUpDown, Copy, Loader2, RotateCw } from "lucide-react";
import { memo, useEffect, useMemo, useRef, useState } from "react";
import type { Job } from "@/api/types";
import { JobError } from "@/components/kit/JobBits";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { duration, took } from "@/lib/format";
import { useNow } from "@/lib/now";
import { shapeOf, stepsOf, thoughtSeconds, wordsOf, type Step } from "@/lib/thinkSteps";
import { routeOfJob } from "@/lib/files";
import { cn } from "@/lib/utils";
import { isActive, useApp } from "@/store/app";
import { Markdown } from "./Markdown";
import { Files, ToolCard, VerdictBadge, verdictOf } from "./ToolCard";
import { filesOf, segmentsOf, useTools, type Call, type Segment } from "./tools";
import { zoneInfo } from "./zones";
import { OnMac } from "@/components/kit/MacTag";

export const Question = memo(function Question({ text }: { text: string }) {
  return (
    <div className="flex justify-end">
      <div className="max-w-[85%] rounded-2xl rounded-br-md bg-muted px-4 py-2.5 text-[0.9375rem] leading-relaxed whitespace-pre-wrap [overflow-wrap:anywhere]">
        {text}
      </div>
    </div>
  );
});

/** An answer that came with the history (no job of ours). */
export function StaticAnswer({ text }: { text: string }) {
  return <Markdown text={wordsOf(text)} />;
}

/**
 * An answer as its steps (lib/thinkSteps.ts): Thinking → tool call → Thinking → … → the words, live and once it
 * ended. Thoughts are quiet folded blocks; "Show steps" unfolds them all.
 */
export const Answer = memo(function Answer({
  job: j,
  variants,
  onPick,
  onRegenerate,
  showModel,
}: {
  job: Job;
  variants?: Job[];
  onPick?: (id: string) => void;
  onRegenerate?: (j: Job) => void;
  showModel?: boolean;
}) {
  const active = isActive(j);
  const ended = !active;

  // chat with tools: the text in pieces, the calls between them; a plain chat: one piece
  const zone = j.request.zone ?? "";
  const live = useTools((s) => s.jobs[j.id]);
  const segments = useMemo<Segment[]>(
    () => (zone || live || j.tools || j.result?.messages ? segmentsOf(j, live) : j.text ? [{ kind: "text", key: "t0", text: j.text }] : []),
    [j, live, zone],
  );
  const steps = useMemo(() => stepsOf(segments), [segments]);
  const shape = shapeOf(steps);
  const files = useMemo(() => filesOf(j, segments), [j, segments]);
  // all the model wrote (for the thoughts' share of the time)
  const allChars = useMemo(() => segments.reduce((n, s) => n + (s.kind === "text" ? s.text.length : 0), 0), [segments]);
  const failed = j.state === "failed" || (j.state === "canceled" && steps.length === 0);
  const answer = steps.filter((s) => s.kind === "text").map((s) => s.text).join("\n\n");

  // "Show steps": every thought unfolded (n counts the clicks, so a thought folded by hand can be unfolded again)
  const [expand, setExpand] = useState({ open: false, n: 0 });
  const last = steps[steps.length - 1];

  return (
    <div className="group/answer flex min-w-0 flex-col gap-2.5">
      {shape.multi && (
        <StepsHeader
          thoughts={shape.thoughts}
          tools={shape.tools}
          open={expand.open}
          onToggle={() => setExpand((e) => ({ open: !e.open, n: e.n + 1 }))}
        />
      )}
      {steps.map((s, i) => {
        const isLast = i === steps.length - 1;
        if (s.kind === "tool") return <ToolCard key={s.key} jobId={j.id} call={s.call} chatZone={zone} />;
        if (s.kind === "think")
          return (
            <Thought
              key={s.key}
              job={j}
              step={s}
              live={s.open && active && isLast}
              cut={s.open && !(active && isLast)}
              allChars={allChars}
              expand={expand}
            />
          );
        return <Markdown key={s.key} text={s.text} streaming={active && isLast} route={routeOfJob(j.id)} />;
      })}
      {active && <Pending job={j} last={last} />}
      {ended && !failed && last?.kind === "think" && (
        <p className="text-sm text-muted-foreground">
          {last.open ? t("It ran out of tokens while thinking. Raise Max tokens (the sliders by Think) and answer again.") : t("No answer after the thinking.")}
        </p>
      )}
      {failed && <Failure job={j} />}
      {j.state === "canceled" && !failed && <p className="text-xs text-muted-foreground">{t("Stopped.")}</p>}
      <Files files={files} />
      {ended && !failed && <Footer job={j} answer={answer} variants={variants} onPick={onPick} onRegenerate={onRegenerate} showModel={showModel} />}
    </div>
  );
});

/** Above an answer of many steps: how many, and Show steps / Hide steps. */
function StepsHeader({ thoughts, tools, open, onToggle }: { thoughts: number; tools: number; open: boolean; onToggle: () => void }) {
  const Icon = open ? ChevronsDownUp : ChevronsUpDown;
  return (
    <div className="-mb-0.5 flex items-center gap-2 text-xs text-muted-foreground">
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={open}
        title={open ? t("Fold every thought") : t("Unfold every thought")}
        className="-ml-1.5 inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 font-medium hover:bg-muted hover:text-foreground"
      >
        <Icon className="size-3.5" />
        {open ? t("Hide steps") : t("Show steps")}
      </button>
      <span className="tabular-nums opacity-80">
        {[thoughts && `${thoughts} ${thoughts === 1 ? "thought" : "thoughts"}`, tools && `${tools} ${tools === 1 ? "tool call" : "tool calls"}`].filter(Boolean).join(" · ")}
      </span>
    </div>
  );
}

/** While the turn runs and nothing streams: what it waits for. */
function Pending({ job: j, last }: { job: Job; last?: Step<Call> }) {
  if (!last) return <Waiting job={j} />;
  if (last.kind !== "tool" || last.call.state === "approval") return null;
  return (
    <div className="flex items-center gap-2 py-0.5 text-sm text-muted-foreground">
      <Loader2 className="size-4 animate-spin text-primary" /> {last.call.state === "running" ? t("Waiting for the tool …") : t("Reading the results …")}
    </div>
  );
}

/** A failed turn; a zone this Mac has no tools for yet is said calmly (it is not an error to retry). */
function Failure({ job: j }: { job: Job }) {
  const why = [j.error, j.problem?.title, j.problem?.hint].find((s) => s && /not available on this mac yet/i.test(s));
  if (!why) return <JobError job={j} compact />;
  const z = zoneInfo(j.request.zone);
  const Icon = z.icon;
  return (
    <div className="flex items-start gap-2.5 rounded-xl border bg-card/40 px-3 py-2.5 text-sm">
      <span className={cn("grid size-6 shrink-0 place-items-center rounded-md", z.soft, z.tone)}>
        <Icon className="size-3.5" />
      </span>
      <div className="min-w-0">
        <div>{capital(why.trim())}</div>
        <div className="text-xs text-muted-foreground">{t("Its tools aren't set up on the Mac (docs/chat-tools.md). A plain chat works meanwhile.")}</div>
      </div>
    </div>
  );
}

function Waiting({ job: j }: { job: Job }) {
  useNow(1000);
  const loading = /loading|download|fetch|verif/i.test(j.status ?? "");
  const text = j.state === "queued" ? t("Waiting for the GPU …") : j.status && j.status !== "answering …" ? capital(j.status) : t("Reading the conversation …");
  return (
    <div className="flex items-center gap-2 py-1 text-sm text-muted-foreground">
      <Loader2 className="size-4 animate-spin text-primary" />
      <span className="min-w-0 truncate">{text}</span>
      {j.started && <span className="text-xs tabular-nums opacity-70">{duration(took(j))}</span>}
      {loading && <span className="hidden text-xs opacity-70 sm:inline">· {t("the first answer of a model takes a moment")}</span>}
    </div>
  );
}

const capital = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

// ---------- a thought ----------

// when each thought began and ended, as seen live (ms), by `${job}:${n}`
const seen = new Map<string, { start?: number; end?: number }>();

/**
 * One thought: "Thinking · 3 s" with a light sweeping over it and its newest lines while it streams, "Thought for
 * 3 s" after (folded). Unfolded: the thought as quiet markdown.
 */
function Thought({
  job: j,
  step,
  live,
  cut,
  allChars,
  expand,
}: {
  job: Job;
  step: Extract<Step<Call>, { kind: "think" }>;
  live: boolean;
  cut: boolean;
  allChars: number;
  expand: { open: boolean; n: number };
}) {
  // folded by hand since the last Show/Hide steps, else as that says
  const [mine, setMine] = useState<{ open: boolean; at: number } | null>(null);
  const open = mine && mine.at === expand.n ? mine.open : expand.open;
  const setOpen = (f: (o: boolean) => boolean) => setMine({ open: f(open), at: expand.n });
  const now = useNow(live ? 1000 : 3_600_000);

  // its times as seen here: from when it began (seen empty); a thought found under way began, for all we know, with
  // the job if it is the first (the prompt opened it), else at an unknown time (estimated once it ended)
  const id = `${j.id}:${step.n}`;
  const begun = step.text === "";
  useEffect(() => {
    const t = seen.get(id) ?? {};
    if (live && t.start == null) t.start = begun ? Date.now() : step.n === 0 && j.started ? Date.parse(j.started) : -1;
    if (!live && t.start != null && t.start >= 0 && t.end == null) t.end = Date.now();
    seen.set(id, t);
  }, [id, live, begun, step.n, j.started]);

  const times = seen.get(id);
  const start = times?.start != null && times.start >= 0 ? times.start : undefined;
  const s = j.stats ?? {};
  const secs = live
    ? start != null
      ? (now - start) / 1000
      : undefined
    : times?.start != null && times.end != null
      ? (times.end - times.start) / 1000
      : thoughtSeconds({
          chars: step.text.length,
          allChars,
          tokens: num(s.tokens),
          tokensPerSecond: num(s.tokensPerSecond),
          total: took(j),
          firstToken: num(s.firstTokenSeconds),
          first: step.n === 0,
        });

  const scroller = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (open && live && scroller.current) scroller.current.scrollTop = scroller.current.scrollHeight;
  }, [step.text, open, live]);

  const time = secs != null && secs >= 0.05 ? duration(secs) : "";
  const label = live ? (
    <>
      <span className="think-live">{t("Thinking")}</span>
      {time && <span className="tabular-nums opacity-80">· {time}</span>}
    </>
  ) : cut ? (
    <span>Thinking cut off{time && <span className="tabular-nums"> · {time}</span>}</span>
  ) : (
    <span>Thought{time && <span className="tabular-nums"> for {time}</span>}</span>
  );
  // folded while it streams: its newest lines
  const tail = live && !open && step.text ? step.text.slice(-400) : "";

  return (
    <div className="min-w-0">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        className="-ml-1 flex items-center gap-1.5 rounded-md px-1 py-0.5 text-sm text-muted-foreground transition-colors hover:text-foreground"
      >
        <Brain className={cn("size-4 shrink-0", live ? "animate-pulse text-primary" : "opacity-80")} />
        {label}
        <ChevronRight className={cn("size-3.5 shrink-0 opacity-70 transition-transform", open && "rotate-90")} />
      </button>
      {open && (
        <div ref={scroller} className="mt-1 ml-[7px] max-h-96 overflow-y-auto border-l-2 pr-1 pl-3.5">
          {step.text ? <Markdown text={step.text} thought streaming={live} route={routeOfJob(j.id)} /> : <p className="text-[0.8125rem] text-muted-foreground">…</p>}
        </div>
      )}
      {tail && (
        <div
          aria-hidden
          className="think-tail mt-1 ml-[7px] flex max-h-[4.1em] flex-col justify-end overflow-hidden border-l-2 pl-3.5 text-[0.8125rem] leading-[1.6] whitespace-pre-wrap text-muted-foreground/80 [overflow-wrap:anywhere]"
        >
          {tail}
        </div>
      )}
    </div>
  );
}

const num = (v: unknown) => (typeof v === "number" && isFinite(v) ? v : undefined);

function Footer({
  job: j,
  answer,
  variants,
  onPick,
  onRegenerate,
  showModel,
}: {
  job: Job;
  answer: string;
  variants?: Job[];
  onPick?: (id: string) => void;
  onRegenerate?: (j: Job) => void;
  showModel?: boolean;
}) {
  const [copied, setCopied] = useState(false);
  const name = useApp((s) => s.models.find((m) => m.id === j.model)?.name ?? j.name);
  const s = j.stats ?? {};
  const tps = num(s.tokensPerSecond);
  const tokens = num(s.tokens);
  const first = num(s.firstTokenSeconds);
  // the budget the answer ran with: the request's, else the model's default (the stats report it)
  const budget = j.request.maxTokens ?? num(s.maxTokens);
  const capped = tokens != null && budget != null && tokens >= budget;
  const i = variants ? variants.findIndex((v) => v.id === j.id) : -1;
  const detail = [
    num(s.promptTokens) != null && `${s.promptTokens} prompt tokens${num(s.cachedTokens) ? ` (${s.cachedTokens} cached)` : ""}`,
    num(s.prefillPerSecond) != null && `prefill ${Math.round(s.prefillPerSecond as number)} tok/s`,
    num(s.peakGB) != null && `peak ${(s.peakGB as number).toFixed(1)} GB`,
    took(j) != null && `${duration(took(j))} in all`,
  ].filter(Boolean);

  const verdict = verdictOf(j.stats);
  return (
    <div className="-ml-2 flex flex-wrap items-center gap-x-1 gap-y-1 text-xs text-muted-foreground">
      {verdict && <VerdictBadge verdict={verdict} />}
      {variants && variants.length > 1 && i >= 0 && (
        <div className="flex items-center">
          <Button variant="ghost" size="icon-xs" disabled={i === 0} onClick={() => onPick?.(variants[i - 1].id)} title={t("Previous answer")}>
            <ChevronLeft />
          </Button>
          <span className="tabular-nums">
            {i + 1}/{variants.length}
          </span>
          <Button variant="ghost" size="icon-xs" disabled={i === variants.length - 1} onClick={() => onPick?.(variants[i + 1].id)} title={t("Next answer")}>
            <ChevronRight />
          </Button>
        </div>
      )}
      <div className="flex items-center opacity-100 transition-opacity md:opacity-60 md:group-hover/answer:opacity-100 md:focus-within:opacity-100">
        {answer && (
          <Button
            variant="ghost"
            size="icon-xs"
            title={t("Copy answer")}
            onClick={() => {
              navigator.clipboard.writeText(answer);
              setCopied(true);
              setTimeout(() => setCopied(false), 1500);
            }}
          >
            {copied ? <Check /> : <Copy />}
          </Button>
        )}
        {onRegenerate && (
          <Button variant="ghost" size="icon-xs" title={t("Answer again")} onClick={() => onRegenerate(j)}>
            <RotateCw />
          </Button>
        )}
      </div>
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="ml-1 cursor-default truncate tabular-nums">
            {[showModel && name, tps != null && `${tps.toFixed(1)} tok/s`, tokens != null && `${tokens} tokens`, first != null && `${duration(first)} to first token`, capped && "cut off at the token limit"]
              .filter(Boolean)
              .join(" · ")}
            <OnMac job={j} plain prefix={showModel || tps != null || tokens != null ? " · " : ""} />
          </span>
        </TooltipTrigger>
        {detail.length > 0 && <TooltipContent>{detail.join(" · ")}</TooltipContent>}
      </Tooltip>
    </div>
  );
}
