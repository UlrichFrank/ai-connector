import { Cable, Loader2, MoreHorizontal, Pencil, Plus, Trash2 } from "lucide-react";
import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { toast } from "sonner";
import { api, errorText, isNotAvailable } from "@/api/client";
import type { Uplink } from "@/api/types";
import { Ago, Dot, Empty, ErrorNotice, Page, PageHeader } from "@/components/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { t } from "@/lib/i18n";
import { createBody, emptyForm, formOf, patchBody, stateLabel, validate, type UplinkForm } from "@/lib/uplinks";

/** How often the list is read when the event stream is not there. */
const POLL = 5000;

/**
 * The uplinks, live: GET /api/uplinks/events (SSE: the whole list at once and on every change); when the stream fails,
 * GET /api/uplinks every few seconds (and the stream is tried again now and then).
 */
function useUplinks() {
  const [list, setList] = useState<Uplink[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [live, setLive] = useState(false);
  const stopped = useRef(false);

  const load = useCallback(async () => {
    try {
      setList(await api.uplinks());
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
      es = new EventSource("/api/uplinks/events");
      es.onmessage = (e: MessageEvent<string>) => {
        try {
          const l = JSON.parse(e.data) as Uplink[];
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

export function UplinksPage() {
  const { list, error, live, load, setList } = useUplinks();
  const [editing, setEditing] = useState<Uplink | "new" | null>(null);
  const missing = isNotAvailable(error);
  const put = (u: Uplink) => setList((l) => (l ? (l.some((x) => x.id === u.id) ? l.map((x) => (x.id === u.id ? u : x)) : [...l, u]) : [u]));

  return (
    <Page>
      <PageHeader
        title={t("Uplinks")}
        description={
          <>
            {t("Connections this server makes to applications that cannot reach it: the application then uses this server's API through the connection, as a client.")}
            {list && !missing && <span className="ml-1 text-xs">{live ? "· live" : `· ${t("refreshed every few seconds")}`}</span>}
          </>
        }
      >
        <Button onClick={() => setEditing("new")} disabled={missing}>
          <Plus /> {t("Add uplink")}
        </Button>
      </PageHeader>

      {error && !list ? (
        <ErrorNotice error={error} onRetry={() => void load()} what="Uplinks" />
      ) : list === null ? (
        <div className="flex justify-center py-10">
          <Loader2 className="size-5 animate-spin text-muted-foreground" />
        </div>
      ) : list.length ? (
        <div className="flex flex-col gap-3">
          {list.map((u) => (
            <UplinkCard key={u.id} u={u} onEdit={() => setEditing(u)} onChanged={put} onDeleted={() => setList((l) => l?.filter((x) => x.id !== u.id) ?? null)} />
          ))}
        </div>
      ) : (
        <Empty icon={Cable} title={t("No uplinks yet")}>
          {t("Add one with the application's endpoint and the token it gives you for it. The application describes the address.")}
        </Empty>
      )}

      <UplinkDialog
        key={editing === null ? "closed" : editing === "new" ? "new" : editing.id}
        target={editing}
        onOpenChange={(o) => !o && setEditing(null)}
        onSaved={(u) => {
          put(u);
          setEditing(null);
        }}
      />
    </Page>
  );
}

function UplinkCard({ u, onEdit, onChanged, onDeleted }: { u: Uplink; onEdit: () => void; onChanged: (u: Uplink) => void; onDeleted: () => void }) {
  const [busy, setBusy] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const st = stateLabel(u.state, u.enabled);

  const toggle = async (enabled: boolean) => {
    setBusy(true);
    try {
      onChanged(await api.patchUplink(u.id, { enabled }));
    } catch (e) {
      toast.error(t(enabled ? "Could not enable “{name}”" : "Could not disable “{name}”", { name: u.name }), { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };
  const remove = async () => {
    try {
      await api.deleteUplink(u.id);
      toast.success(t("“{name}” deleted", { name: u.name }));
      onDeleted();
    } catch (e) {
      toast.error(t("Could not delete"), { description: errorText(e) });
    }
  };

  return (
    <div className="card p-4" data-uplink={u.id} data-state={u.state}>
      <div className="flex items-start gap-3">
        <Cable className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
        <div className="min-w-0 flex-1">
          <div className="font-medium">{u.name}</div>
          <div className="mt-0.5 truncate text-xs text-muted-foreground">
            <span className="font-mono">{u.url}</span>
            {u.owner && <span> · {t("acts for")} {u.owner}</span>}
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
            <span className="flex items-center gap-1.5">
              <Dot tone={st.tone} pulse={st.tone === "busy"} /> {t(st.text)}
              {u.since && u.enabled && (
                <span className="text-muted-foreground">
                  · <Ago iso={u.since} never="" />
                </span>
              )}
            </span>
          </div>
          {u.state === "error" && u.error && u.enabled && <div className="mt-2 rounded-md bg-destructive/10 px-2 py-1.5 text-xs text-destructive">{u.error}</div>}
          {u.state === "connecting" && u.error && u.enabled && <div className="mt-2 text-xs text-muted-foreground">{u.error}</div>}
        </div>
        <Switch checked={u.enabled} disabled={busy} onCheckedChange={(v) => void toggle(v)} aria-label={t(u.enabled ? "Disable {name}" : "Enable {name}", { name: u.name })} />
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-sm" aria-label={t("Actions for {name}", { name: u.name })}>
              <MoreHorizontal />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onSelect={onEdit}>
              <Pencil /> {t("Edit…")}
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive" onSelect={() => setDeleting(true)}>
              <Trash2 /> {t("Delete…")}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
      <AlertDialog open={deleting} onOpenChange={setDeleting}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("Delete “{name}”?", { name: u.name })}</AlertDialogTitle>
            <AlertDialogDescription>{t("The server disconnects, forgets the token and revokes the uplink's client.")}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("Cancel")}</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={() => void remove()}>
              {t("Delete")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function UplinkDialog({ target, onOpenChange, onSaved }: { target: Uplink | "new" | null; onOpenChange: (o: boolean) => void; onSaved: (u: Uplink) => void }) {
  const editing = target && target !== "new" ? target : null;
  // a fresh form for each target (the page keys this dialog by it)
  const [form, setForm] = useState<UplinkForm>(() => (editing ? formOf(editing) : emptyForm));
  const [errors, setErrors] = useState<Partial<Record<keyof UplinkForm, string>>>({});
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState("");

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
        onSaved(Object.keys(p).length ? await api.patchUplink(editing.id, p) : editing);
      } else {
        onSaved(await api.createUplink(createBody(form)));
      }
      toast.success(editing ? t("Saved") : t("Uplink added"), { description: t("It connects in the background; the state shows here.") });
    } catch (err) {
      setFailure(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  const field = (k: keyof UplinkForm, label: string, props: React.ComponentProps<typeof Input> = {}, hint?: string) => (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={`uplink-${k}`}>{label}</Label>
      <Input id={`uplink-${k}`} value={form[k]} onChange={(e) => setForm((f) => ({ ...f, [k]: e.target.value }))} aria-invalid={!!errors[k] || undefined} {...props} />
      {errors[k] ? <p className="text-xs text-destructive">{t(errors[k])}</p> : hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );

  return (
    <Dialog open={!!target} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg">
        <form onSubmit={submit} className="contents">
          <DialogHeader>
            <DialogTitle>{editing ? t("Edit “{name}”", { name: editing.name }) : t("Add an uplink")}</DialogTitle>
            <DialogDescription>
              {t("The server dials the application's endpoint with its token. The application's requests then act as a client of yours: your models, your Claude Code, your downloads.")}
            </DialogDescription>
          </DialogHeader>
          <div className="flex flex-col gap-3">
            {field("name", t("Name"), { placeholder: "uplink", autoFocus: true, maxLength: 80 }, t("The application may show it; blank: “uplink”."))}
            {field("url", t("Endpoint"), { placeholder: "https://app.example.com/…", inputMode: "url", autoComplete: "off" }, t("The address the application gives for an uplink."))}
            {field(
              "token",
              "Token",
              { type: "password", placeholder: editing ? t("unchanged") : "", autoComplete: "off" },
              editing ? t("Leave blank to keep the stored token.") : t("The application's token for it. It is stored on the server and never shown again."),
            )}
            {failure && <p className="text-sm text-destructive">{failure}</p>}
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              {t("Cancel")}
            </Button>
            <Button type="submit" disabled={busy}>
              {busy && <Loader2 className="animate-spin" />} {editing ? t("Save") : t("Add")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
