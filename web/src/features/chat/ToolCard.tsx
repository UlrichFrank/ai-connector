// Tool calls in the answer: a compact card per call, approvals, citations, the files made and the guard's verdict.
// Everything a tool gave back is untrusted: it is shown as plain text (React escapes it), never as HTML.

import {
  Ban,
  Check,
  ChevronRight,
  CircleAlert,
  CircleCheck,
  CircleHelp,
  Clock,
  Download,
  ExternalLink,
  File as FileIcon,
  FileSpreadsheet,
  FileText,
  Hand,
  ImageIcon,
  Loader2,
  Repeat2,
  ShieldAlert,
  ShieldCheck,
  TriangleAlert,
} from "lucide-react";
import { memo, useEffect, useState } from "react";
import { toast } from "sonner";
import type { Source, Verdict, Zone } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { bytes } from "@/lib/format";
import { useNow } from "@/lib/now";
import { navigate } from "@/lib/route";
import { cn } from "@/lib/utils";
import { ApprovalGone, approvalTimeout, approve, argsOf, callTitle, domainOf, humanize, serverAndTool, type Call } from "./tools";
import { zoneInfo } from "./zones";

// ---------- a call ----------

const stateWords: Record<string, string> = {
  approval: "Waiting for you",
  running: "Running",
  done: "Done",
  error: "Failed",
  denied: "Denied",
  blocked: "Blocked",
  held: "Held back",
};

export const ToolCard = memo(function ToolCard({ jobId, call: c, chatZone }: { jobId: string; call: Call; chatZone: Zone }) {
  if (c.state === "approval") return <ApprovalCard jobId={jobId} call={c} />;
  return <CallCard call={c} chatZone={chatZone} />;
});

function StateIcon({ state }: { state: string }) {
  switch (state) {
    case "running":
      return <Loader2 className="size-3.5 animate-spin text-primary" />;
    case "done":
      return <Check className="size-3.5 text-emerald-600 dark:text-emerald-400" />;
    case "held":
      return <ShieldAlert className="size-3.5 text-amber-600 dark:text-amber-400" />;
    case "blocked":
      return <Ban className="size-3.5 text-muted-foreground" />;
    case "denied":
      return <Hand className="size-3.5 text-muted-foreground" />;
    case "error":
      return <CircleAlert className="size-3.5 text-destructive" />;
    default:
      return <Loader2 className="size-3.5 animate-spin text-muted-foreground" />;
  }
}

function CallCard({ call: c, chatZone }: { call: Call; chatZone: Zone }) {
  const [open, setOpen] = useState(false);
  const z = zoneInfo(c.zone || chatZone);
  const Icon = z.icon;
  const title = callTitle(c);
  const args = argsOf(c);
  const hasArgs = Object.keys(args).length > 0;
  const held = c.state === "held";
  const blocked = c.state === "blocked";
  const denied = c.state === "denied";
  const sources = c.sources ?? [];
  const detail = hasArgs || !!c.result;
  return (
    <div
      className={cn(
        "rounded-xl border bg-card/40 text-sm",
        held && "border-amber-500/40 bg-amber-500/5",
        (blocked || denied) && "border-dashed",
      )}
    >
      <button
        type="button"
        onClick={() => detail && setOpen((o) => !o)}
        aria-expanded={detail ? open : undefined}
        className={cn("flex w-full min-w-0 items-center gap-2 px-3 py-2 text-left", detail && "cursor-pointer")}
      >
        <span className={cn("grid size-6 shrink-0 place-items-center rounded-md", z.soft, z.tone)}>
          <Icon className="size-3.5" />
        </span>
        <span className={cn("min-w-0 flex-1 truncate", (blocked || denied) && "text-muted-foreground line-through decoration-muted-foreground/40")} title={title.detail ?? title.text}>
          {title.text}
        </span>
        <span className="flex shrink-0 items-center gap-1 text-xs text-muted-foreground">
          <StateIcon state={c.state} />
          {c.state !== "done" && c.state !== "running" && <span className="max-sm:hidden">{c.decided === "deny" && denied ? "Denied by you" : stateWords[c.state]}</span>}
          {sources.length > 0 && c.state === "done" && <span className="tabular-nums">{sources.length === 1 ? "1 source" : `${sources.length} sources`}</span>}
        </span>
        {detail && <ChevronRight className={cn("size-3.5 shrink-0 text-muted-foreground transition-transform", open && "rotate-90")} />}
      </button>

      {held && (
        <div className="flex gap-2 border-t border-amber-500/20 px-3 py-2 text-xs leading-relaxed text-amber-800 dark:text-amber-200">
          <ShieldAlert className="mt-0.5 size-3.5 shrink-0" />
          <div className="min-w-0">
            The guard held this content back: it contained instructions aimed at the assistant, so the model never saw it.
            {c.verdict?.why && <span className="mt-0.5 block text-amber-800/80 dark:text-amber-200/80">Reason: {c.verdict.why}</span>}
          </div>
        </div>
      )}
      {blocked && (
        <div className="flex gap-2 border-t px-3 py-2 text-xs leading-relaxed text-muted-foreground">
          <Ban className="mt-0.5 size-3.5 shrink-0" />
          <span>{blockedWhy(c, chatZone)}</span>
        </div>
      )}
      {denied && (
        <div className="flex items-center gap-2 border-t px-3 py-1.5 text-xs text-muted-foreground">
          <Hand className="size-3.5 shrink-0" />
          {c.decided === "deny" ? "You denied it; nothing was changed." : c.decided === "allow" ? "Denied; nothing was changed." : "Not approved in time (5 minutes); nothing was changed."}
        </div>
      )}
      {(c.asked || c.decided === "allow") && !denied && !c.auto && (
        <div className="flex items-center gap-2 border-t px-3 py-1.5 text-xs text-muted-foreground">
          <Check className="size-3.5 shrink-0" />
          {c.decided === "allow" ? "You approved it." : "Approved."}
        </div>
      )}
      {c.auto && <AllowedAlways />}

      {sources.length > 0 && <Sources sources={sources} className="border-t px-3 py-2" />}

      {open && detail && (
        <div className="flex flex-col gap-2 border-t px-3 py-2.5">
          {hasArgs && <Args args={args} />}
          {c.result && (
            <div className="flex flex-col gap-1">
              <span className="text-[11px] font-medium tracking-wide text-muted-foreground uppercase">Result</span>
              {/* untrusted: plain text only */}
              <pre className="max-h-64 overflow-auto rounded-md bg-muted/60 px-2.5 py-2 font-mono text-xs leading-relaxed whitespace-pre-wrap [overflow-wrap:anywhere]">
                {c.result}
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

/** A call that ran without asking: the tool is allowed always. */
function AllowedAlways() {
  return (
    <div className="flex items-center gap-2 border-t px-3 py-1.5 text-xs text-muted-foreground">
      <Repeat2 className="size-3.5 shrink-0" />
      <span className="min-w-0 flex-1">Allowed always: it ran without asking.</span>
      <button type="button" onClick={() => navigate("tools")} className="shrink-0 underline-offset-2 hover:text-foreground hover:underline">
        Change
      </button>
    </div>
  );
}

function blockedWhy(c: Call, chatZone: Zone): string {
  const chat = zoneInfo(chatZone);
  const tool = zoneInfo(c.zone);
  if (!chatZone) return "This chat has no tools. Start a new chat with tools to use them.";
  if (c.zone && c.zone !== chatZone)
    return `${tool.name} tools aren't available in a ${chat.name} chat${chat.never ? ` (${chat.never.charAt(0).toLowerCase() + chat.never.slice(1)})` : ""}. The zones keep them apart; start a ${tool.name} chat for that.`;
  return `Refused: ${c.result || "this tool isn't available in this chat."}`;
}

/** Arguments as key: value rows (values as text). */
function Args({ args }: { args: Record<string, unknown> }) {
  return (
    <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1 text-xs">
      {Object.entries(args).map(([k, v]) => (
        <div key={k} className="contents">
          <dt className="text-muted-foreground">{humanize(k)}</dt>
          <dd className="font-mono whitespace-pre-wrap [overflow-wrap:anywhere]">{typeof v === "string" ? v : JSON.stringify(v, null, 2)}</dd>
        </div>
      ))}
    </dl>
  );
}

// ---------- an approval ----------

function ApprovalCard({ jobId, call: c }: { jobId: string; call: Call }) {
  const [busy, setBusy] = useState<"allow" | "deny" | "always" | null>(null);
  const [gone, setGone] = useState(false);
  // "Always allow" asks once more, in the card
  const [confirming, setConfirming] = useState(false);
  const [always, setAlways] = useState(false);
  const now = useNow(1000);
  const [server, tool] = serverAndTool(c.name);
  const toolName = `“${humanize(tool)}”${server ? ` (${server})` : ""}`;
  // only home tools can be allowed always (web and files tools never ask)
  const canAlways = (c.zone || "home") === "home";
  const z = zoneInfo(c.zone || "home");
  const Icon = z.icon;
  // the worker's deadline when it sent one (survives a reload), else counted from when we saw it
  const end = c.deadline ? Date.parse(c.deadline) : c.since != null ? c.since + approvalTimeout : undefined;
  const left = end != null ? Math.max(0, end - now) : undefined;
  // answered here: the card waits for the worker's next event
  const waiting = !c.decided && !gone;
  const answer = async (allow: boolean, forever = false) => {
    setBusy(forever ? "always" : allow ? "allow" : "deny");
    try {
      await approve(jobId, c.id, allow, forever);
      if (forever) {
        setAlways(true);
        toast.success(`${toolName} is allowed always`, {
          description: "It runs without asking from now on.",
          action: { label: "Settings", onClick: () => navigate("tools") },
        });
      }
    } catch (e) {
      if (e instanceof ApprovalGone) return setGone(true);
      toast.error(allow ? "Could not approve" : "Could not deny", { description: e instanceof Error ? e.message : String(e) });
    } finally {
      setBusy(null);
      setConfirming(false);
    }
  };
  const args = argsOf(c);
  return (
    <div className="rounded-xl border border-primary/40 bg-primary/5 text-sm shadow-sm" role="group" aria-label="Approval needed">
      <div className="flex items-start gap-2.5 px-3 pt-3">
        <span className={cn("grid size-7 shrink-0 place-items-center rounded-md", z.soft, z.tone)}>
          <Icon className="size-4" />
        </span>
        <div className="min-w-0 flex-1">
          <div className="font-medium">
            {humanize(tool)}
            {server && <span className="font-normal text-muted-foreground"> on {server}</span>}
          </div>
          {waiting && c.description && <p className="mt-0.5 line-clamp-3 text-xs text-foreground/80">{c.description}</p>}
          <p className="text-xs leading-relaxed text-muted-foreground">
            {waiting
              ? "This changes something. Nothing happens until you approve it."
              : gone
                ? "Already decided (on another device?) or timed out."
                : c.decided === "allow"
                  ? always
                    ? "Approved, and allowed always; it runs now."
                    : "Approved; it runs now."
                  : "Denied; nothing will change."}
          </p>
        </div>
        {waiting && left != null && (
          <span className={cn("shrink-0 rounded-full bg-muted px-2 py-0.5 text-xs tabular-nums text-muted-foreground", left < 60_000 && "text-amber-700 dark:text-amber-300")} title="Denied by itself when the time runs out">
            {clock(left)}
          </span>
        )}
      </div>
      {Object.keys(args).length > 0 && (
        <div className="mx-3 mt-2.5 rounded-lg border bg-background/60 px-2.5 py-2">
          <Args args={args} />
        </div>
      )}
      {waiting && confirming && (
        <div className="mx-3 mt-2.5 flex flex-col gap-2 rounded-lg border border-primary/30 bg-background/70 px-3 py-2.5 sm:flex-row sm:items-center" role="alertdialog" aria-label="Always allow">
          <p className="min-w-0 flex-1 text-xs leading-relaxed">
            Run {toolName} without asking from now on? You can change this in Settings → Tools.
          </p>
          <div className="flex shrink-0 justify-end gap-2">
            <Button variant="ghost" size="sm" disabled={!!busy} onClick={() => setConfirming(false)}>
              Cancel
            </Button>
            <Button size="sm" disabled={!!busy} onClick={() => answer(true, true)}>
              {busy === "always" ? <Loader2 className="animate-spin" /> : <Repeat2 />} Always allow
            </Button>
          </div>
        </div>
      )}
      <div className="flex flex-wrap items-center justify-end gap-2 px-3 py-2.5">
        {waiting ? (
          !confirming && (
            <>
              <Button variant="ghost" size="sm" disabled={!!busy} onClick={() => answer(false)}>
                {busy === "deny" && <Loader2 className="animate-spin" />} Deny
              </Button>
              {canAlways && (
                <Button variant="outline" size="sm" disabled={!!busy} onClick={() => setConfirming(true)} title="Approve this call and don't ask again for this tool">
                  <Repeat2 /> Always allow
                </Button>
              )}
              <Button size="sm" disabled={!!busy} onClick={() => answer(true)}>
                {busy === "allow" ? <Loader2 className="animate-spin" /> : <Check />} Approve
              </Button>
            </>
          )
        ) : (
          <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
            {gone ? <Clock className="size-3.5" /> : c.decided === "allow" ? <Loader2 className="size-3.5 animate-spin text-primary" /> : <Hand className="size-3.5" />}
            {gone ? "No longer waiting" : c.decided === "allow" ? (always ? "Allowed always" : "Approved") : "Denied"}
          </span>
        )}
      </div>
    </div>
  );
}

const clock = (ms: number) => {
  const s = Math.ceil(ms / 1000);
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
};

// ---------- citations ----------

export function Sources({ sources, className }: { sources: Source[]; className?: string }) {
  return (
    <div className={cn("flex flex-wrap gap-1.5", className)}>
      {sources.map((s, i) => (
        <a
          key={s.url + i}
          href={safeUrl(s.url)}
          target="_blank"
          rel="noreferrer noopener"
          title={s.title ? `${s.title}\n${s.url}` : s.url}
          className="inline-flex h-6 max-w-56 items-center gap-1.5 rounded-full border bg-background/60 px-2 text-xs text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
        >
          <span className="grid size-4 shrink-0 place-items-center rounded-full bg-muted text-[10px] font-medium tabular-nums">{i + 1}</span>
          <span className="truncate">{domainOf(s.url)}</span>
        </a>
      ))}
    </div>
  );
}

/** Only http(s) links leave the page. */
const safeUrl = (u: string) => (/^https?:\/\//i.test(u) ? u : undefined);

// ---------- files ----------

const ext = (f: string) => (f.split("?")[0].split(".").pop() ?? "").toLowerCase();
const nameOf = (f: string) => decodeURIComponent(f.split("?")[0].split("/").pop() ?? f);
const isImage = (f: string) => ["png", "svg", "jpg", "jpeg", "webp", "gif"].includes(ext(f));

const sizes = new Map<string, Promise<number | undefined>>();
function useSize(url: string) {
  const [n, setN] = useState<number | undefined>();
  useEffect(() => {
    let p = sizes.get(url);
    if (!p) {
      p = fetch(url, { method: "HEAD", credentials: "same-origin" })
        .then((r) => (r.ok ? Number(r.headers.get("Content-Length")) || undefined : undefined))
        .catch(() => undefined);
      sizes.set(url, p);
    }
    let live = true;
    p.then((v) => live && setN(v));
    return () => {
      live = false;
    };
  }, [url]);
  return n;
}

function FileKindIcon({ file, className }: { file: string; className?: string }) {
  const e = ext(file);
  if (e === "pdf") return <FileText className={cn(className, "text-red-600 dark:text-red-400")} />;
  if (["csv", "xlsx"].includes(e)) return <FileSpreadsheet className={cn(className, "text-emerald-600 dark:text-emerald-400")} />;
  if (["docx", "md", "txt"].includes(e)) return <FileText className={cn(className, "text-sky-600 dark:text-sky-400")} />;
  if (isImage(file)) return <ImageIcon className={cn(className, "text-indigo-600 dark:text-indigo-400")} />;
  return <FileIcon className={className} />;
}

export function Files({ files }: { files: string[] }) {
  if (!files.length) return null;
  const images = files.filter(isImage);
  return (
    <div className="flex flex-col gap-2">
      {images.length > 0 && (
        <div className="flex flex-wrap gap-2">
          {images.map((f) => (
            <a key={f} href={f} target="_blank" rel="noreferrer" className="block overflow-hidden rounded-xl border bg-white" title={nameOf(f)}>
              <img src={f} alt={nameOf(f)} loading="lazy" className="max-h-72 w-auto max-w-full object-contain" />
            </a>
          ))}
        </div>
      )}
      <div className="flex flex-wrap gap-2">
        {files.map((f) => (
          <FileChip key={f} file={f} />
        ))}
      </div>
    </div>
  );
}

function FileChip({ file }: { file: string }) {
  const size = useSize(file);
  const name = nameOf(file);
  return (
    <div className="flex h-12 max-w-full min-w-0 items-center gap-2.5 rounded-xl border bg-card/60 pr-1 pl-2.5 sm:max-w-80">
      <FileKindIcon file={file} className="size-5 shrink-0" />
      <a href={file} target="_blank" rel="noreferrer" className="flex min-w-0 flex-1 flex-col leading-tight hover:underline">
        <span className="truncate text-sm">{name}</span>
        <span className="text-xs text-muted-foreground">
          {ext(file).toUpperCase()}
          {size ? ` · ${bytes(size)}` : ""}
        </span>
      </a>
      <Button variant="ghost" size="icon-sm" asChild title="Open">
        <a href={file} target="_blank" rel="noreferrer">
          <ExternalLink />
        </a>
      </Button>
      <Button variant="ghost" size="icon-sm" asChild title="Download">
        <a href={file} download={name}>
          <Download />
        </a>
      </Button>
    </div>
  );
}

// ---------- the guard's grade of the answer ----------

export function VerdictBadge({ verdict: v }: { verdict: Verdict }) {
  const label = v.label;
  const pct = isFinite(v.score) ? `${Math.round(v.score * 100)}%` : "";
  const look =
    label === "good"
      ? { icon: ShieldCheck, text: "Checked", cls: "text-emerald-700/80 dark:text-emerald-400/80" }
      : label === "bad"
        ? { icon: TriangleAlert, text: "Check this", cls: "text-amber-700 dark:text-amber-300 bg-amber-500/10" }
        : { icon: CircleHelp, text: "Unsure", cls: "text-muted-foreground" };
  const Icon = label === "good" ? CircleCheck : look.icon;
  const tip =
    label === "good"
      ? "The guard found the answer answers the question and stays with its sources."
      : label === "bad"
        ? v.why || "The guard thinks the answer may not answer the question or may not stay with its sources."
        : v.why || "The guard could not tell whether the answer stays with its sources.";
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className={cn("inline-flex h-5 cursor-default items-center gap-1 rounded-full px-1.5 text-[11px] font-medium", look.cls)}>
          <Icon className="size-3" />
          {look.text}
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-72 flex-col items-start gap-0.5">
        <span>{tip}</span>
        {pct && <span className="opacity-70">Guard confidence {pct}</span>}
      </TooltipContent>
    </Tooltip>
  );
}

export function verdictOf(stats: Record<string, unknown> | undefined): Verdict | undefined {
  const v = stats?.verdict as Verdict | undefined;
  return v && typeof v === "object" && typeof v.label === "string" && ["good", "bad", "unsure"].includes(v.label) ? v : undefined;
}
