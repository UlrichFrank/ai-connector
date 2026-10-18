import { CloudOff, RotateCw, TriangleAlert } from "lucide-react";
import { errorText, isOffline } from "@/api/client";
import { Button } from "@/components/ui/button";
import { navigate } from "@/lib/route";
import { t } from "@/lib/i18n";
import { cn } from "@/lib/utils";

/** What a page shows when loading failed: calm and amber when the Mac is away, red for anything else. */
export function ErrorNotice({ error, onRetry, className }: { error: unknown; onRetry?: () => void; className?: string }) {
  const offline = isOffline(error);
  const Icon = offline ? CloudOff : TriangleAlert;
  return (
    <div
      role="status"
      className={cn(
        "flex items-start gap-3 rounded-xl border p-4 text-sm",
        offline ? "border-amber-500/30 bg-amber-500/10" : "border-destructive/30 bg-destructive/10",
        className,
      )}
    >
      <Icon className={cn("mt-0.5 size-4 shrink-0", offline ? "text-amber-600 dark:text-amber-400" : "text-destructive")} />
      <div className="min-w-0 flex-1">
        <div className="font-medium">{offline ? t("The Mac is offline") : t("Something went wrong")}</div>
        <div className="mt-0.5 text-muted-foreground">
          {offline ? (
            <>
              {t("This comes back when it reconnects. Start it with")} <Code>mlxlab worker</Code>
              {t(", or")}{" "}
              <ConnectLink />.
            </>
          ) : (
            errorText(error)
          )}
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

/** A command to type, inline. */
export function Code({ children, className }: { children: string; className?: string }) {
  return <code className={cn("rounded bg-muted px-1 py-0.5 font-mono text-[0.85em] text-foreground", className)}>{children}</code>;
}

/** "connect a Mac": Devices with the Connect dialog open — the way forward when no Mac is there. */
export function ConnectLink({ className }: { className?: string }) {
  return (
    <button type="button" onClick={() => navigate("devices", "connect")} className={cn("font-medium text-[var(--teal-ink)] underline-offset-4 hover:underline", className)}>
      {t("connect a Mac")}
    </button>
  );
}
