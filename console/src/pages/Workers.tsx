import { Apple, Loader2, MoreHorizontal, Pin, PinOff, Plus, Server, Trash2 } from "lucide-react";
import { useEffect, useState, type FormEvent } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Worker, WorkerCreated } from "@/api/types";
import { Ago, Code, CodeBlock, Dot, Empty, ErrorNotice, Page, PageHeader, ShownOnce } from "@/components/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { t, useLang } from "@/lib/i18n";
import { guessOS, installCommand, loginCommand, restartCommand, type InstallOS } from "@/lib/install";
import { navigate, useRoute } from "@/lib/route";
import { loadText, loadedModels, platformOf, specParts, type Platform } from "@/lib/specs";
import { cn } from "@/lib/utils";
import { useLive } from "@/store/live";

/** A small mark for the platform (lucide has an apple; the others get a terminal-ish box). */
export function PlatformIcon({ platform, className }: { platform: Platform; className?: string }) {
  if (platform === "mac") return <Apple className={cn("size-4 shrink-0 text-muted-foreground", className)} aria-label="macOS" />;
  const label = platform === "linux" ? "Linux" : platform === "windows" ? "Windows" : "";
  if (!label) return <Server className={cn("size-4 shrink-0 text-muted-foreground", className)} aria-hidden />;
  return (
    <span aria-label={label} title={label} className={cn("grid size-4 shrink-0 place-items-center rounded-[3px] bg-muted text-[9px] font-bold text-muted-foreground", className)}>
      {platform === "linux" ? "L" : "W"}
    </span>
  );
}

export function WorkersPage() {
  const { arg } = useRoute();
  const workers = useLive((s) => s.workers);
  const error = useLive((s) => s.workersError);
  const refresh = useLive((s) => s.refreshWorkers);
  // the button, or a link to #/workers/connect (the overview's "Connect a worker")
  const [clicked, setClicked] = useState(false);
  const connecting = clicked || arg === "connect";
  useEffect(() => void refresh(), [refresh]);

  // connected first, then by name
  const sorted = [...workers].sort((a, b) => Number(b.connected) - Number(a.connected) || a.name.localeCompare(b.name, undefined, { numeric: true }));

  return (
    <Page>
      <PageHeader title={t("Workers")} description={t("The machines that run the models. Each one dials out to this server with its own token.")}>
        <Button onClick={() => setClicked(true)}>
          <Plus /> {t("Connect a worker")}
        </Button>
      </PageHeader>

      {error && !workers.length ? (
        <ErrorNotice error={error} onRetry={refresh} what="The worker list" />
      ) : sorted.length ? (
        <div className="flex flex-col gap-3">
          {sorted.map((w) => (
            <WorkerCard key={w.id} w={w} highlight={arg === w.id} />
          ))}
        </div>
      ) : (
        <Empty icon={Server} title={t("No workers yet")}>
          {t("Connect a Mac, a Linux or a Windows PC: it runs the models, the server routes jobs to it.")}
        </Empty>
      )}

      <ConnectWorkerDialog
        open={connecting}
        onOpenChange={(o) => {
          setClicked(o);
          if (!o && arg === "connect") navigate("workers");
        }}
        onCreated={() => void refresh()}
      />
    </Page>
  );
}

function WorkerCard({ w, highlight }: { w: Worker; highlight: boolean }) {
  const refresh = useLive((s) => s.refreshWorkers);
  const [revoking, setRevoking] = useState(false);
  const parts = specParts(w.specs);
  const loaded = loadedModels(w);
  const busy = !!w.state?.running;

  const pin = async (on: boolean) => {
    try {
      await (on ? api.makePrimary(w.id) : api.autoPrimary(w.id));
      await refresh();
    } catch (e) {
      toast.error(t("Could not change the primary worker"), { description: errorText(e) });
    }
  };

  return (
    <div id={`worker-${w.id}`} className={cn("card p-4", highlight && "ring-2 ring-primary/40")} data-worker={w.id} data-connected={w.connected}>
      <div className="flex items-start gap-3">
        <PlatformIcon platform={platformOf(w.specs)} className="mt-0.5" />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="font-medium">{w.name}</span>
            {w.primary && (
              <span className="rounded bg-primary/15 px-1.5 text-[11px] font-medium text-[var(--teal-ink)]" title={t("Serves the per-worker libraries of the mlxlab app (prompts, tools, …)")}>
                {t("primary")}
                {w.pinned ? ` ${t("(pinned)")}` : ""}
              </span>
            )}
            {w.legacy && <span className="rounded bg-muted px-1.5 text-[11px] text-muted-foreground">{t("shared token")}</span>}
            {w.specs?.version && <span className="text-xs text-muted-foreground">v{w.specs.version}</span>}
          </div>
          {parts.length > 0 ? (
            <div className="mt-1 flex flex-wrap gap-1.5">
              {parts.map((p) => (
                <span key={p} className="rounded-md bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">
                  {p}
                </span>
              ))}
            </div>
          ) : (
            <div className="mt-1 text-xs text-muted-foreground">{w.connected ? t("Specs not known yet (an older worker?)") : t("Specs show once it has connected.")}</div>
          )}
          <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
            <span className="flex items-center gap-1.5">
              <Dot tone={w.connected ? (busy ? "busy" : "ok") : "off"} pulse={busy} />
              {loadText(w)}
            </span>
            {w.connected ? (
              <span className="text-muted-foreground">
                {t("connected")} <Ago iso={w.since} never="" />
              </span>
            ) : (
              <span className="text-muted-foreground">
                {t("last seen")} <Ago iso={w.lastSeen} />
              </span>
            )}
            {loaded.length > 0 && <span className="min-w-0 truncate text-muted-foreground">{t("in memory:")} {loaded.join(", ")}</span>}
          </div>
        </div>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-sm" aria-label={t("Actions for {name}", { name: w.name })}>
              <MoreHorizontal />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            {w.pinned ? (
              <DropdownMenuItem onSelect={() => void pin(false)}>
                <PinOff /> {t("Unpin (primary by memory)")}
              </DropdownMenuItem>
            ) : (
              <DropdownMenuItem onSelect={() => void pin(true)} disabled={w.legacy}>
                <Pin /> {t("Make primary")}
              </DropdownMenuItem>
            )}
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive" disabled={w.legacy} onSelect={() => setRevoking(true)}>
              <Trash2 /> {t("Revoke…")}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
      <RevokeWorker w={w} open={revoking} onOpenChange={setRevoking} />
    </div>
  );
}

function RevokeWorker({ w, open, onOpenChange }: { w: Worker; open: boolean; onOpenChange: (o: boolean) => void }) {
  const refresh = useLive((s) => s.refreshWorkers);
  const revoke = async () => {
    try {
      await api.revokeWorker(w.id);
      toast.success(t("{name} revoked", { name: w.name }));
      await refresh();
    } catch (e) {
      toast.error(t("Could not revoke"), { description: errorText(e) });
    }
  };
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t("Revoke “{name}”?", { name: w.name })}</AlertDialogTitle>
          <AlertDialogDescription>
            {w.connected ? t("Its token stops working and it is disconnected now. To use it again, connect it as a new worker.") : t("Its token stops working. To use it again, connect it as a new worker.")}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>{t("Cancel")}</AlertDialogCancel>
          <AlertDialogAction variant="destructive" onClick={() => void revoke()}>
            {t("Revoke")}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

/**
 * "Connect a worker": a name, then the worker's own token in one paste per OS (shown this once), and a live state
 * that turns to "Connected" when it is on the tunnel (/api/worker/events).
 */
export function ConnectWorkerDialog({ open, onOpenChange, onCreated }: { open: boolean; onOpenChange: (open: boolean) => void; onCreated: (w: Worker) => void }) {
  const [created, setCreated] = useState<WorkerCreated | null>(null);
  const change = (o: boolean) => {
    onOpenChange(o);
    if (!o) setTimeout(() => setCreated(null), 200); // after the closing animation: a fresh dialog next time
  };
  return (
    <Dialog open={open} onOpenChange={change}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-2xl" data-connect-dialog={created ? "command" : "name"}>
        {created ? (
          <Command created={created} onDone={() => change(false)} />
        ) : (
          <NameStep
            onCancel={() => change(false)}
            onCreated={(c) => {
              setCreated(c);
              onCreated(c.worker);
            }}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

function NameStep({ onCancel, onCreated }: { onCancel: () => void; onCreated: (c: WorkerCreated) => void }) {
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      onCreated(await api.createWorker(name.trim() || "Worker"));
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form onSubmit={submit} className="contents">
      <DialogHeader>
        <DialogTitle>{t("Connect a worker")}</DialogTitle>
        <DialogDescription>{t("A Mac, a Linux or a Windows PC runs models for this server. Name it, then paste one command on it.")}</DialogDescription>
      </DialogHeader>
      <div className="flex flex-col gap-2">
        <Label htmlFor="worker-name">{t("Name")}</Label>
        <Input id="worker-name" value={name} onChange={(e) => setName(e.target.value)} autoFocus maxLength={80} placeholder={t("Mac Studio, gaming PC, …")} />
        {error && <p className="text-sm text-destructive">{error}</p>}
      </div>
      <DialogFooter>
        <Button type="button" variant="outline" onClick={onCancel}>
          {t("Cancel")}
        </Button>
        <Button type="submit" disabled={busy}>
          {busy && <Loader2 className="animate-spin" />} {t("Create token")}
        </Button>
      </DialogFooter>
    </form>
  );
}

const osTabs: { os: InstallOS; label: string; where: string }[] = [
  { os: "unix", label: "macOS / Linux", where: "Terminal on the worker" },
  { os: "windows", label: "Windows", where: "PowerShell on the worker" },
];

function Command({ created: { worker, token }, onDone }: { created: WorkerCreated; onDone: () => void }) {
  const connected = useLive((s) => s.connected.some((m) => m.id === worker.id));
  const [os, setOS] = useState<InstallOS>(() => guessOS(navigator.userAgent));
  const tab = osTabs.find((x) => x.os === os)!;
  const de = useLang() === "de";
  const command = installCommand(os, location.origin, token);

  return (
    <>
      <DialogHeader>
        <DialogTitle>{t("Connect “{name}”", { name: worker.name })}</DialogTitle>
        <DialogDescription>{t("Paste this on the worker and press Return. It downloads ai-connector from this server and starts it as a worker.")}</DialogDescription>
      </DialogHeader>

      <div role="tablist" className="seg w-fit">
        {osTabs.map((x) => (
          <button key={x.os} role="tab" aria-selected={os === x.os} aria-pressed={os === x.os} onClick={() => setOS(x.os)}>
            {x.label}
          </button>
        ))}
      </div>

      <CodeBlock label={t(tab.where)} text={command} secret={token} />

      <ShownOnce>
        <span className="font-medium">{t("The token is shown only now.")}</span>{" "}
        <span className="text-muted-foreground">{t("It works like a password: copy the command now and do not share it. Lost it? Revoke this worker and connect it again.")}</span>
      </ShownOnce>

      {de ? (
        <ul className="flex list-disc flex-col gap-1 pl-5 text-sm text-muted-foreground marker:text-muted-foreground/50">
          {os === "unix" ? (
            <>
              <li>
                Installiert nach <Code>~/.config/mlxlab/bin/ai-connector</Code>. Später wieder starten mit <Code>{restartCommand("unix")}</Code>.
              </li>
              <li>macOS braucht Apple Silicon (M1 oder neuer). Linux (x86-64 oder arm64) braucht glibc: Ubuntu, Debian, Fedora – nicht Alpine.</li>
            </>
          ) : (
            <>
              <li>
                Installiert nach <Code>%LOCALAPPDATA%\ai-connector\ai-connector.exe</Code> (x86-64). Später wieder starten mit <Code>{restartCommand("windows")}</Code>.
              </li>
              <li>Windows-Worker sind neu: Sie bauen, wurden aber noch wenig genutzt.</li>
            </>
          )}
          <li>
            Lokale Laufzeiten auf dem Worker – Ollama, LM Studio, llama.cpp, vLLM – werden automatisch gefunden und ihre Modelle angeboten; <Code>MLXLAB_RUNTIMES</Code> stellt sie ein (<Code>none</Code> schaltet es ab).
          </li>
          <li>
            Was wo läuft: Jeder Worker führt den Embedder, den Klassifikator und die Modelle seiner lokalen Laufzeiten aus; Chat mit Werkzeugen sowie Bild-, Video-, Musik-, Sprach- und Avatar-Modelle laufen auf einem Mac (MLX).
          </li>
          <li>Lass ihn laufen, solange du ihn nutzt; der Server schickt einen Auftrag nur an einen Worker, der das Modell hat.</li>
          <li>
            Damit er bei jeder Anmeldung startet (und gleich jetzt im Hintergrund), führe <Code>{loginCommand(os)}</Code> aus, sobald er verbunden ist ({os === "windows" ? "eine geplante Aufgabe" : "ein launchd-Agent unter macOS, eine systemd-User-Unit unter Linux"}; <Code>--uninstall</Code> entfernt ihn). Das Token bleibt in seiner Einstellungsdatei, nicht im Dienst.
          </li>
        </ul>
      ) : (
        <ul className="flex list-disc flex-col gap-1 pl-5 text-sm text-muted-foreground marker:text-muted-foreground/50">
          {os === "unix" ? (
            <>
              <li>
                Installs to <Code>~/.config/mlxlab/bin/ai-connector</Code>. Start it again later with <Code>{restartCommand("unix")}</Code>.
              </li>
              <li>macOS needs Apple silicon (M1 or later). Linux (x86-64 or arm64) needs glibc: Ubuntu, Debian, Fedora — not Alpine.</li>
            </>
          ) : (
            <>
              <li>
                Installs to <Code>%LOCALAPPDATA%\ai-connector\ai-connector.exe</Code> (x86-64). Start it again later with <Code>{restartCommand("windows")}</Code>.
              </li>
              <li>Windows workers are new: they build, but have seen little use yet.</li>
            </>
          )}
          <li>
            Local runtimes on the worker — Ollama, LM Studio, llama.cpp, vLLM — are found automatically and their models offered; <Code>MLXLAB_RUNTIMES</Code> configures them (<Code>none</Code> turns it off).
          </li>
          <li>
            What runs where: every worker runs the embedder, the classifier and the models of its local runtimes; chat with tools and image, video, music, speech and avatar models run on a Mac (MLX).
          </li>
          <li>Keep it running while you use it; the server routes a job only to a worker that has its model.</li>
          <li>
            To start it at every login (and now, in the background), run <Code>{loginCommand(os)}</Code> once it has connected ({os === "windows" ? "a scheduled task" : "a launchd agent on macOS, a systemd user unit on Linux"}; <Code>--uninstall</Code> removes it). The token stays in its settings file, not in the service.
          </li>
        </ul>
      )}

      <DialogFooter className="flex-row items-center justify-between max-sm:flex-row sm:justify-between">
        <div role="status" aria-live="polite" className="flex min-w-0 items-center gap-2 text-sm" data-connect-state={connected ? "connected" : "waiting"}>
          {connected ? (
            <>
              <Dot tone="ok" />
              <span className="font-medium text-emerald-700 dark:text-emerald-400">{t("Connected ✓")}</span>
              <span className="text-muted-foreground max-sm:hidden">· {t("it takes jobs now")}</span>
            </>
          ) : (
            <>
              <Loader2 className="size-4 animate-spin text-muted-foreground" />
              <span className="text-muted-foreground">{t("Waiting for the worker …")}</span>
            </>
          )}
        </div>
        <Button variant={connected ? "default" : "outline"} onClick={onDone}>
          {connected ? t("Done") : t("Close")}
        </Button>
      </DialogFooter>
    </>
  );
}

