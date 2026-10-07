import { KeyRound, Loader2 } from "lucide-react";
import { useState, type FormEvent } from "react";
import { api, errorText } from "@/api/client";
import type { Worker, WorkerCreated } from "@/api/types";
import { Code } from "@/components/connection/ErrorNotice";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { connectCommand, MAC_BIN } from "@/lib/connect";
import { useApp } from "@/store/app";
import { CommandBlock } from "./CommandBlock";

/**
 * Devices → "Connect a Mac": a name, then the Mac's own token in one command to paste in its Terminal (shown this
 * once), and a live state that turns to "Connected ✓" when that Mac is on the tunnel (/api/worker/events).
 */
export function ConnectMacDialog({ open, onOpenChange, onCreated }: { open: boolean; onOpenChange: (open: boolean) => void; onCreated: (w: Worker) => void }) {
  const [created, setCreated] = useState<WorkerCreated | null>(null);
  // a fresh dialog each time it opens (the token is not shown again)
  const change = (o: boolean) => {
    onOpenChange(o);
    if (!o) setTimeout(() => setCreated(null), 200); // after the closing animation
  };
  return (
    <Dialog open={open} onOpenChange={change}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-xl" data-connect-dialog={created ? "command" : "name"}>
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
  const [name, setName] = useState("Mac");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      onCreated(await api.createWorker(name.trim() || "Mac"));
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="contents">
      <DialogHeader>
        <DialogTitle>Connect a Mac</DialogTitle>
        <DialogDescription>The Mac runs the models and keeps your files. Name it, then paste one command in its Terminal.</DialogDescription>
      </DialogHeader>
      <div className="flex flex-col gap-2">
        <Label htmlFor="mac-name">Name</Label>
        <Input id="mac-name" value={name} onChange={(e) => setName(e.target.value)} onFocus={(e) => e.target.select()} autoFocus maxLength={80} placeholder="Mac Studio" />
        {error && <p className="text-sm text-destructive">{error}</p>}
      </div>
      <DialogFooter>
        <Button type="button" variant="outline" onClick={onCancel}>
          Cancel
        </Button>
        <Button type="submit" disabled={busy}>
          {busy && <Loader2 className="animate-spin" />} Create
        </Button>
      </DialogFooter>
    </form>
  );
}

function Command({ created: { worker, token }, onDone }: { created: WorkerCreated; onDone: () => void }) {
  const current = useApp((s) => s.worker);
  const connected = !!current?.connected && current.id === worker.id;
  // another Mac on the tunnel now: this one takes its place when it connects
  const other = current?.connected && current.id !== worker.id ? current.name || "Another Mac" : "";
  const command = connectCommand(location.origin, token);

  return (
    <>
      <DialogHeader>
        <DialogTitle>Connect “{worker.name}”</DialogTitle>
        <DialogDescription>Open Terminal on the Mac, paste this command and press Return.</DialogDescription>
      </DialogHeader>

      <CommandBlock text={command} secret={token} />

      <div role="note" className="flex items-start gap-2.5 rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2.5">
        <KeyRound className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-400" />
        <p>
          <span className="font-medium">The token is shown only now.</span> <span className="text-muted-foreground">It works like a password: copy the command now and do not share it. Lost it? Revoke this Mac and connect it again.</span>
        </p>
      </div>

      <ul className="flex list-disc flex-col gap-1 pl-5 text-muted-foreground marker:text-muted-foreground/50">
        <li>
          Downloads mlxlab from this hub into <Code>~/.config/mlxlab/bin</Code> and starts it as a worker.
        </li>
        <li>Needs a Mac with Apple Silicon (M1 or later) and macOS.</li>
        <li>
          Keep the Terminal window open while you use mlxlab. To start it again later: <Code>{`${MAC_BIN} worker`}</Code>
        </li>
        <li>
          One Mac at a time:{" "}
          {other ? (
            <>
              <span className="font-medium text-foreground">{other}</span> is connected now; this Mac replaces it when it connects.
            </>
          ) : (
            "a Mac that connects later replaces this one."
          )}
        </li>
      </ul>

      <DialogFooter className="flex-row items-center justify-between max-sm:flex-row sm:justify-between">
        <div role="status" aria-live="polite" className="flex min-w-0 items-center gap-2" data-connect-state={connected ? "connected" : "waiting"}>
          {connected ? (
            <>
              <span className="size-2 shrink-0 rounded-full bg-emerald-500" />
              <span className="font-medium text-emerald-700 dark:text-emerald-400">Connected ✓</span>
              <span className="text-muted-foreground max-sm:hidden">· it runs your jobs now</span>
            </>
          ) : (
            <>
              <Loader2 className="size-4 animate-spin text-muted-foreground" />
              <span className="text-muted-foreground">Waiting for the Mac …</span>
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
