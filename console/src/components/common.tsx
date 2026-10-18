import { Check, CloudOff, Copy, Eye, EyeOff, KeyRound, Plug, RotateCw, TriangleAlert } from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { toast } from "sonner";
import { errorText, isNotAvailable } from "@/api/client";
import { Button } from "@/components/ui/button";
import { copyText } from "@/lib/clipboard";
import { ago } from "@/lib/format";
import { maskToken } from "@/lib/install";
import { cn } from "@/lib/utils";
import { useNow } from "@/lib/now";
import { dateTime, t } from "@/lib/i18n";

/** Inline code. */
export function Code({ children }: { children: ReactNode }) {
  return <code className="rounded bg-muted px-1 py-0.5 font-mono text-[0.85em]">{children}</code>;
}

/** A page's heading row: title, one line about it, actions on the right. */
export function PageHeader({ title, description, children }: { title: string; description?: ReactNode; children?: ReactNode }) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
      <div className="min-w-0">
        <h2 className="text-2xl font-semibold tracking-[-0.03em]">{title}</h2>
        {description && <p className="mt-1 max-w-2xl text-sm text-muted-foreground">{description}</p>}
      </div>
      {children && <div className="flex shrink-0 items-center gap-2">{children}</div>}
    </div>
  );
}

/** A page's content column. */
export function Page({ children, wide }: { children: ReactNode; wide?: boolean }) {
  return <div className={cn("mx-auto w-full px-4 py-6 sm:px-6", wide ? "max-w-6xl" : "max-w-5xl")}>{children}</div>;
}

export type Tone = "ok" | "busy" | "bad" | "off";

/** A coloured dot for a state. */
export function Dot({ tone, pulse, className }: { tone: Tone; pulse?: boolean; className?: string }) {
  return (
    <span aria-hidden className={cn("dot inline-block", tone === "busy" && "warn", tone === "bad" && "bad", tone === "off" && "idle", pulse && "live", className)} />
  );
}

/** "5 min ago", kept up to date, the exact time on hover. */
export function Ago({ iso, never }: { iso: string | undefined; never?: string }) {
  useNow();
  const text = ago(iso);
  if (!iso || text === ago(undefined)) return <span className="text-muted-foreground">{never ?? t("never")}</span>;
  return (
    <time dateTime={iso} title={dateTime(iso)}>
      {text}
    </time>
  );
}

/** Nothing here yet: an icon, a sentence, maybe an action. */
export function Empty({ icon: Icon = Plug, title, children }: { icon?: React.ComponentType<{ className?: string }>; title: string; children?: ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-2 rounded-[var(--r-lg)] border border-dashed border-[var(--line-2)] px-6 py-10 text-center">
      <span className="ico mb-1" style={{ "--c": "var(--idle)" } as React.CSSProperties}>
        <Icon />
      </span>
      <div className="font-medium">{title}</div>
      {children && <div className="max-w-md text-sm text-muted-foreground">{children}</div>}
    </div>
  );
}

/**
 * What a page shows when loading failed: a calm notice when the server does not have the API yet (other phases add
 * it), amber when no worker is connected, red for anything else.
 */
export function ErrorNotice({ error, onRetry, what, className }: { error: unknown; onRetry?: () => void; what?: string; className?: string }) {
  const missing = isNotAvailable(error);
  const offline = !missing && (error as { code?: string })?.code === "offline";
  const Icon = missing ? Plug : offline ? CloudOff : TriangleAlert;
  return (
    <div
      role="status"
      data-error-kind={missing ? "unavailable" : offline ? "offline" : "error"}
      className={cn(
        "flex items-start gap-3 rounded-xl border p-4 text-sm",
        missing ? "bg-muted/40" : offline ? "border-amber-500/30 bg-amber-500/10" : "border-destructive/30 bg-destructive/10",
        className,
      )}
    >
      <Icon className={cn("mt-0.5 size-4 shrink-0", missing ? "text-muted-foreground" : offline ? "text-amber-600 dark:text-amber-400" : "text-destructive")} />
      <div className="min-w-0 flex-1">
        <div className="font-medium">{missing ? t("{what} is not available on this server yet", { what: what ? t(what) : t("This") }) : offline ? t("No worker is connected") : t("Something went wrong")}</div>
        <div className="mt-0.5 text-muted-foreground">
          {missing ? t("The server answered that it does not know this API. Update the server to a version that has it.") : offline ? t("This comes back when a worker connects (Workers → Connect a worker).") : errorText(error)}
        </div>
      </div>
      {onRetry && (
        <Button size="sm" variant="outline" onClick={onRetry}>
          <RotateCw /> {t("Retry")}
        </Button>
      )}
    </div>
  );
}

/** Copies on click, says so for two seconds. */
function useCopy(text: string) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(t);
  }, [copied]);
  const copy = async () => {
    try {
      await copyText(text);
      setCopied(true);
    } catch (e) {
      toast.error(t("Could not copy"), { description: e instanceof Error ? e.message : String(e) });
    }
  };
  return { copied, setCopied, copy };
}

export function CopyButton({ text, label, className }: { text: string; label?: string; className?: string }) {
  const { copied, copy } = useCopy(text);
  return (
    <Button variant={copied ? "secondary" : "outline"} size="xs" onClick={copy} className={className}>
      {copied ? <Check /> : <Copy />} {copied ? t("Copied") : (label ?? t("Copy"))}
    </Button>
  );
}

/** A command or snippet to paste: monospace, one Copy button. A secret in it is masked on screen until shown. */
export function CodeBlock({ text, secret, label }: { text: string; secret?: string; label: string }) {
  const { copied, setCopied, copy } = useCopy(text);
  const [shown, setShown] = useState(false);
  const masked = !!secret && !shown;
  return (
    <div className="overflow-hidden rounded-[var(--r-md)] border border-[var(--line-2)] bg-[var(--sunken)] shadow-[var(--inset-track)]">
      <div className="flex items-center gap-1 border-b px-2 py-1">
        <span className="flex-1 truncate pl-1 text-xs text-muted-foreground">{label}</span>
        {secret && (
          <Button variant="ghost" size="xs" onClick={() => setShown((v) => !v)} aria-pressed={shown}>
            {shown ? <EyeOff /> : <Eye />} {shown ? t("Hide token") : t("Show token")}
          </Button>
        )}
        <Button variant={copied ? "secondary" : "default"} size="xs" onClick={copy} data-copy="">
          {copied ? <Check /> : <Copy />} {copied ? t("Copied") : t("Copy")}
        </Button>
      </div>
      <pre
        className="overflow-x-auto px-3 py-2.5 font-mono text-xs leading-relaxed whitespace-pre select-all"
        // copied by hand while masked: the real text, not the dots
        onCopy={(e) => {
          if (!masked) return;
          e.preventDefault();
          e.clipboardData.setData("text/plain", text);
          setCopied(true);
        }}
      >
        <code>{masked ? maskToken(text, secret ?? "") : text}</code>
      </pre>
    </div>
  );
}

/** The amber "shown only now" note under a fresh token. */
export function ShownOnce({ children }: { children: ReactNode }) {
  return (
    <div role="note" className="flex items-start gap-2.5 rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2.5 text-sm">
      <KeyRound className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-400" />
      <p>{children}</p>
    </div>
  );
}
