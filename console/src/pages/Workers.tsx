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
import { guessOS, installCommand, restartCommand, type InstallOS } from "@/lib/install";
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
      <PageHeader title="Workers" description="The machines that run the models. Each one dials out to this server with its own token.">
        <Button onClick={() => setClicked(true)}>
          <Plus /> Connect a worker
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
        <Empty icon={Server} title="No workers yet">
          Connect a Mac, a Linux or a Windows PC: it runs the models, the server routes jobs to it.
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
      toast.error("Could not change the primary worker", { description: errorText(e) });
    }
  };

  return (
    <div id={`worker-${w.id}`} className={cn("rounded-xl border bg-card p-4", highlight && "ring-2 ring-primary/40")} data-worker={w.id} data-connected={w.connected}>
      <div className="flex items-start gap-3">
        <PlatformIcon platform={platformOf(w.specs)} className="mt-0.5" />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="font-medium">{w.name}</span>
            {w.primary && (
              <span className="rounded bg-primary/15 px-1.5 text-[11px] font-medium text-primary" title="Serves the per-worker libraries of the mlxlab app (prompts, tools, …)">
                primary{w.pinned ? " (pinned)" : ""}
              </span>
            )}
            {w.legacy && <span className="rounded bg-muted px-1.5 text-[11px] text-muted-foreground">shared token</span>}
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
            <div className="mt-1 text-xs text-muted-foreground">{w.connected ? "Specs not known yet (an older worker?)" : "Specs show once it has connected."}</div>
          )}
          <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
            <span className="flex items-center gap-1.5">
              <Dot tone={w.connected ? (busy ? "busy" : "ok") : "off"} pulse={busy} />
              {loadText(w)}
            </span>
            {w.connected ? (
              <span className="text-muted-foreground">
                connected <Ago iso={w.since} never="" />
              </span>
            ) : (
              <span className="text-muted-foreground">
                last seen <Ago iso={w.lastSeen} />
              </span>
            )}
            {loaded.length > 0 && <span className="min-w-0 truncate text-muted-foreground">in memory: {loaded.join(", ")}</span>}
          </div>
        </div>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${w.name}`}>
              <MoreHorizontal />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            {w.pinned ? (
              <DropdownMenuItem onSelect={() => void pin(false)}>
                <PinOff /> Unpin (primary by memory)
              </DropdownMenuItem>
            ) : (
              <DropdownMenuItem onSelect={() => void pin(true)} disabled={w.legacy}>
                <Pin /> Make primary
              </DropdownMenuItem>
            )}
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive" disabled={w.legacy} onSelect={() => setRevoking(true)}>
              <Trash2 /> Revoke…
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
      toast.success(`${w.name} revoked`);
      await refresh();
    } catch (e) {
      toast.error("Could not revoke", { description: errorText(e) });
    }
  };
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Revoke “{w.name}”?</AlertDialogTitle>
          <AlertDialogDescription>Its token stops working{w.connected ? " and it is disconnected now" : ""}. To use it again, connect it as a new worker.</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction variant="destructive" onClick={() => void revoke()}>
            Revoke
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
        <DialogTitle>Connect a worker</DialogTitle>
        <DialogDescription>A Mac, a Linux or a Windows PC runs models for this server. Name it, then paste one command on it.</DialogDescription>
      </DialogHeader>
      <div className="flex flex-col gap-2">
        <Label htmlFor="worker-name">Name</Label>
        <Input id="worker-name" value={name} onChange={(e) => setName(e.target.value)} autoFocus maxLength={80} placeholder="Mac Studio, gaming PC, …" />
        {error && <p className="text-sm text-destructive">{error}</p>}
      </div>
      <DialogFooter>
        <Button type="button" variant="outline" onClick={onCancel}>
          Cancel
        </Button>
        <Button type="submit" disabled={busy}>
          {busy && <Loader2 className="animate-spin" />} Create token
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
  const tab = osTabs.find((t) => t.os === os)!;
  const command = installCommand(os, location.origin, token);

  return (
    <>
      <DialogHeader>
        <DialogTitle>Connect “{worker.name}”</DialogTitle>
        <DialogDescription>Paste this on the worker and press Return. It downloads ai-connector from this server and starts it as a worker.</DialogDescription>
      </DialogHeader>

      <div role="tablist" className="flex w-fit rounded-lg bg-muted p-0.5 text-sm">
        {osTabs.map((t) => (
          <button key={t.os} role="tab" aria-selected={os === t.os} onClick={() => setOS(t.os)} className={cn("rounded-md px-3 py-1 text-muted-foreground", os === t.os && "bg-background text-foreground shadow-sm")}>
            {t.label}
          </button>
        ))}
      </div>

      <CodeBlock label={tab.where} text={command} secret={token} />

      <ShownOnce>
        <span className="font-medium">The token is shown only now.</span>{" "}
        <span className="text-muted-foreground">It works like a password: copy the command now and do not share it. Lost it? Revoke this worker and connect it again.</span>
      </ShownOnce>

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
      </ul>

      <DialogFooter className="flex-row items-center justify-between max-sm:flex-row sm:justify-between">
        <div role="status" aria-live="polite" className="flex min-w-0 items-center gap-2 text-sm" data-connect-state={connected ? "connected" : "waiting"}>
          {connected ? (
            <>
              <Dot tone="ok" />
              <span className="font-medium text-emerald-700 dark:text-emerald-400">Connected ✓</span>
              <span className="text-muted-foreground max-sm:hidden">· it takes jobs now</span>
            </>
          ) : (
            <>
              <Loader2 className="size-4 animate-spin text-muted-foreground" />
              <span className="text-muted-foreground">Waiting for the worker …</span>
            </>
          )}
        </div>
        <Button variant={connected ? "default" : "outline"} onClick={onDone}>
          {connected ? "Done" : "Close"}
        </Button>
      </DialogFooter>
    </>
  );
}

