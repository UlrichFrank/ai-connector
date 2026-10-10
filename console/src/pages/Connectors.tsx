import { Bot, Loader2, MoreHorizontal, Pencil, Plus, Trash2 } from "lucide-react";
import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { toast } from "sonner";
import { api, errorText, isNotAvailable } from "@/api/client";
import type { Connector, Model } from "@/api/types";
import { Ago, Dot, Empty, ErrorNotice, Page, PageHeader } from "@/components/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { createBody, emptyForm, formOf, patchBody, stateLabel, validate, type ConnectorForm } from "@/lib/connectors";
import { useLive } from "@/store/live";

/** How often the list is read when the event stream is not there. */
const POLL = 5000;

/**
 * The connectors, live: GET /api/connectors/events (SSE: the whole list at once and on every change); when the stream
 * fails, GET /api/connectors every few seconds (and the stream is tried again now and then).
 */
function useConnectors() {
  const [list, setList] = useState<Connector[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [live, setLive] = useState(false);
  const stopped = useRef(false);

  const load = useCallback(async () => {
    try {
      const l = await api.connectors();
      setList(l);
      setError(null);
      return true;
    } catch (e) {
      setError(e);
      return !isNotAvailable(e);
    }
  }, []);

  useEffect(() => {
    stopped.current = false;
    let es: EventSource | null = null;
    let poll: ReturnType<typeof setInterval> | undefined;
    let retry: ReturnType<typeof setTimeout> | undefined;

    const stream = () => {
      if (stopped.current) return;
      es = new EventSource("/api/connectors/events");
      es.onmessage = (e: MessageEvent<string>) => {
        try {
          const l = JSON.parse(e.data) as Connector[];
          setList(Array.isArray(l) ? l : []);
          setError(null);
          setLive(true);
          clearInterval(poll);
          poll = undefined;
        } catch {
          // not ours
        }
      };
      es.onerror = async () => {
        es?.close();
        es = null;
        setLive(false);
        if (stopped.current) return;
        // the plain list tells whether the API is there at all (and whether we are signed in)
        const there = await load();
        if (!there || stopped.current) return;
        poll ??= setInterval(() => void load(), POLL);
        retry = setTimeout(stream, 30000);
      };
    };
    stream();
    return () => {
      stopped.current = true;
      es?.close();
      clearInterval(poll);
      clearTimeout(retry);
    };
  }, [load]);

  return { list, error, live, load, setList };
}

export function ConnectorsPage() {
  const { list, error, live, load, setList } = useConnectors();
  const [editing, setEditing] = useState<Connector | "new" | null>(null);
  const missing = isNotAvailable(error);
  const put = (c: Connector) => setList((l) => (l ? (l.some((x) => x.id === c.id) ? l.map((x) => (x.id === c.id ? c : x)) : [...l, c]) : [c]));

  return (
    <Page>
      <PageHeader
        title="Connectors"
        description={
          <>
            Outbound connections to applications that expect a local AI to dial in. A vboard connector answers vboard's classify, embed and chat requests through the workers.
            {list && !missing && <span className="ml-1 text-xs">{live ? "· live" : "· refreshed every few seconds"}</span>}
          </>
        }
      >
        <Button onClick={() => setEditing("new")} disabled={missing}>
          <Plus /> Add vboard
        </Button>
      </PageHeader>

      {error && !list ? (
        <ErrorNotice error={error} onRetry={() => void load()} what="Connectors" />
      ) : list === null ? (
        <div className="flex justify-center py-10">
          <Loader2 className="size-5 animate-spin text-muted-foreground" />
        </div>
      ) : list.length ? (
        <div className="flex flex-col gap-3">
          {list.map((c) => (
            <ConnectorCard key={c.id} c={c} onEdit={() => setEditing(c)} onChanged={put} onDeleted={() => setList((l) => l?.filter((x) => x.id !== c.id) ?? null)} />
          ))}
        </div>
      ) : (
        <Empty icon={Bot} title="No connectors yet">
          Add a vboard connector with a vboard personal or board token (<code className="font-mono">vbp_…</code>): this server then acts as vboard's local AI.
        </Empty>
      )}

      <ConnectorDialog
        key={editing === null ? "closed" : editing === "new" ? "new" : editing.id}
        target={editing}
        onOpenChange={(o) => !o && setEditing(null)}
        onSaved={(c) => {
          put(c);
          setEditing(null);
        }}
      />
    </Page>
  );
}

function ConnectorCard({ c, onEdit, onChanged, onDeleted }: { c: Connector; onEdit: () => void; onChanged: (c: Connector) => void; onDeleted: () => void }) {
  const [busy, setBusy] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const st = stateLabel(c.state, c.enabled);
  const models = Object.entries(c.models ?? {}).filter(([, v]) => v);

  const toggle = async (enabled: boolean) => {
    setBusy(true);
    try {
      onChanged(await api.patchConnector(c.id, { enabled }));
    } catch (e) {
      toast.error(`Could not ${enabled ? "enable" : "disable"} “${c.name}”`, { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };
  const remove = async () => {
    try {
      await api.deleteConnector(c.id);
      toast.success(`“${c.name}” deleted`);
      onDeleted();
    } catch (e) {
      toast.error("Could not delete", { description: errorText(e) });
    }
  };

  return (
    <div className="rounded-xl border bg-card p-4" data-connector={c.id} data-state={c.state}>
      <div className="flex items-start gap-3">
        <Bot className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="font-medium">{c.name}</span>
            <span className="rounded bg-muted px-1.5 text-[11px] text-muted-foreground">{c.kind}</span>
          </div>
          <div className="mt-0.5 truncate text-xs text-muted-foreground">
            <a href={c.url} target="_blank" rel="noreferrer" className="hover:underline">
              {c.url}
            </a>
            {c.board && <span> · board {c.board}</span>}
            {c.user && <span> · as {c.user}</span>}
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
            <span className="flex items-center gap-1.5">
              <Dot tone={st.tone} pulse={st.tone === "busy"} /> {st.text}
              {c.since && c.enabled && (
                <span className="text-muted-foreground">
                  · <Ago iso={c.since} never="" />
                </span>
              )}
            </span>
            {models.length > 0 && (
              <span className="text-muted-foreground">
                {models.map(([k, v]) => (
                  <span key={k} className="mr-2">
                    {k}: <code className="font-mono">{v}</code>
                  </span>
                ))}
              </span>
            )}
          </div>
          {c.state === "error" && c.error && c.enabled && <div className="mt-2 rounded-md bg-destructive/10 px-2 py-1.5 text-xs text-destructive">{c.error}</div>}
        </div>
        <Switch checked={c.enabled} disabled={busy} onCheckedChange={(v) => void toggle(v)} aria-label={c.enabled ? `Disable ${c.name}` : `Enable ${c.name}`} />
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${c.name}`}>
              <MoreHorizontal />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onSelect={onEdit}>
              <Pencil /> Edit…
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive" onSelect={() => setDeleting(true)}>
              <Trash2 /> Delete…
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
      <AlertDialog open={deleting} onOpenChange={setDeleting}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete “{c.name}”?</AlertDialogTitle>
            <AlertDialogDescription>The server disconnects from vboard and forgets the token.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={() => void remove()}>
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

/** Radix Select has no empty value: this one stands for "the server's default". */
const DEFAULT = "__default";

function ModelSelect({ id, label, kind, value, onChange, models }: { id: string; label: string; kind: string; value: string; onChange: (v: string) => void; models: Model[] }) {
  const options = models.filter((m) => m.kind === kind);
  // a model chosen before that no connected worker lists now stays selectable
  const known = !value || options.some((m) => m.id === value);
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <Select value={value || DEFAULT} onValueChange={(v) => onChange(v === DEFAULT ? "" : v)}>
        <SelectTrigger id={id} className="w-full">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={DEFAULT}>Server default</SelectItem>
          {!known && <SelectItem value={value}>{value} (not offered now)</SelectItem>}
          {options.map((m) => (
            <SelectItem key={m.id} value={m.id}>
              {m.id}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}

function ConnectorDialog({ target, onOpenChange, onSaved }: { target: Connector | "new" | null; onOpenChange: (o: boolean) => void; onSaved: (c: Connector) => void }) {
  const models = useLive((s) => s.models);
  const editing = target && target !== "new" ? target : null;
  // a fresh form for each target (the page keys this dialog by it)
  const [form, setForm] = useState<ConnectorForm>(() => (editing ? formOf(editing) : emptyForm));
  const [errors, setErrors] = useState<Partial<Record<keyof ConnectorForm, string>>>({});
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState("");

  const set = (k: keyof ConnectorForm) => (v: string) => setForm((f) => ({ ...f, [k]: v }));

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const errs = validate(form, !!editing);
    setErrors(errs);
    if (Object.keys(errs).length) return;
    setBusy(true);
    setFailure("");
    try {
      if (editing) {
        const p = patchBody(editing, form);
        onSaved(Object.keys(p).length ? await api.patchConnector(editing.id, p) : editing);
      } else {
        onSaved(await api.createConnector(createBody(form)));
      }
      toast.success(editing ? "Saved" : "Connector added", { description: "It connects in the background; the state shows here." });
    } catch (err) {
      setFailure(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  const field = (k: keyof ConnectorForm, label: string, props: React.ComponentProps<typeof Input> = {}, hint?: string) => (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={`connector-${k}`}>{label}</Label>
      <Input id={`connector-${k}`} value={form[k]} onChange={(e) => set(k)(e.target.value)} aria-invalid={!!errors[k] || undefined} {...props} />
      {errors[k] ? <p className="text-xs text-destructive">{errors[k]}</p> : hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );

  return (
    <Dialog open={!!target} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg">
        <form onSubmit={submit} className="contents">
          <DialogHeader>
            <DialogTitle>{editing ? `Edit “${editing.name}”` : "Add a vboard connector"}</DialogTitle>
            <DialogDescription>The server dials vboard's classifier bridge with the token and answers as vboard's local AI.</DialogDescription>
          </DialogHeader>
          <div className="flex flex-col gap-3">
            {field("name", "Name", { placeholder: "vboard", autoFocus: true, maxLength: 80 })}
            {field("url", "vboard address", { placeholder: "https://vboard.example.com", inputMode: "url", autoComplete: "off" })}
            {field(
              "token",
              "Token",
              { type: "password", placeholder: editing ? "unchanged" : "vbp_…", autoComplete: "off" },
              editing ? "Leave blank to keep the stored token." : "A vboard personal or board token. It is stored on the server and never shown again.",
            )}
            {field("board", "Board id (optional)", { placeholder: "b_…", autoComplete: "off" }, "Only for this board; empty for every board the token may see.")}
            <div className="grid gap-3 sm:grid-cols-3">
              <ModelSelect id="connector-classify" label="Classify" kind="classify" value={form.classify} onChange={set("classify")} models={models} />
              <ModelSelect id="connector-embed" label="Embed" kind="embed" value={form.embed} onChange={set("embed")} models={models} />
              <ModelSelect id="connector-chat" label="Chat" kind="chat" value={form.chat} onChange={set("chat")} models={models} />
            </div>
            {failure && <p className="text-sm text-destructive">{failure}</p>}
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy}>
              {busy && <Loader2 className="animate-spin" />} {editing ? "Save" : "Add"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
