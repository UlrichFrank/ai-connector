import { ArrowDown, ArrowUp, Bot, Loader2, MoreHorizontal, Pencil, Plus, Trash2, X } from "lucide-react";
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
import { addAgent, addGenerator, agentKind, allGenerators, createBody, defaultName, efforts, emptyForm, formOf, maxAgents, maxGenerators, moveAgent, moveGenerator, patchBody, removeAgent, removeGenerator, stateLabel, validate, type AgentRow, type ConnectorForm } from "@/lib/connectors";
import { t } from "@/lib/i18n";
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
        title={t("Connectors")}
        description={
          <>
            {t("Outbound connections to applications that expect a local AI to dial in. A vboard connector answers vboard's classify, embed and chat requests through the workers.")}
            {list && !missing && <span className="ml-1 text-xs">{live ? "· live" : `· ${t("refreshed every few seconds")}`}</span>}
          </>
        }
      >
        <Button onClick={() => setEditing("new")} disabled={missing}>
          <Plus /> {t("Add vboard")}
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
        <Empty icon={Bot} title={t("No connectors yet")}>
          {t("Add a vboard connector with a vboard personal or board token (vbp_…): this server then acts as vboard's local AI.")}
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
      toast.error(t(enabled ? "Could not enable “{name}”" : "Could not disable “{name}”", { name: c.name }), { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };
  const remove = async () => {
    try {
      await api.deleteConnector(c.id);
      toast.success(t("“{name}” deleted", { name: c.name }));
      onDeleted();
    } catch (e) {
      toast.error(t("Could not delete"), { description: errorText(e) });
    }
  };

  return (
    <div className="card p-4" data-connector={c.id} data-state={c.state}>
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
            {c.board && <span> · {t("board")} {c.board}</span>}
            {c.user && <span> · {t("as")} {c.user}</span>}
            {c.owner && <span> · {t("added by")} {c.owner}</span>}
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
            <span className="flex items-center gap-1.5">
              <Dot tone={st.tone} pulse={st.tone === "busy"} /> {t(st.text)}
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
          <AgentsLine c={c} />
          {(c.generators ?? []).length > 0 && (
            <div className="mt-1 text-xs text-muted-foreground" aria-label={t("Generators")}>
              {t("board")}: {(c.generators ?? []).map((g) => (
                <code key={g} className="mr-2 font-mono">
                  {g}
                </code>
              ))}
            </div>
          )}
          {c.state === "error" && c.error && c.enabled && <div className="mt-2 rounded-md bg-destructive/10 px-2 py-1.5 text-xs text-destructive">{c.error}</div>}
          {c.state === "connecting" && c.error && c.enabled && <div className="mt-2 text-xs text-muted-foreground">{c.error}</div>}
        </div>
        <Switch checked={c.enabled} disabled={busy} onCheckedChange={(v) => void toggle(v)} aria-label={t(c.enabled ? "Disable {name}" : "Enable {name}", { name: c.name })} />
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-sm" aria-label={t("Actions for {name}", { name: c.name })}>
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
            <AlertDialogTitle>{t("Delete “{name}”?", { name: c.name })}</AlertDialogTitle>
            <AlertDialogDescription>{t("The server disconnects from vboard and forgets the token.")}</AlertDialogDescription>
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

/** The chat agents: what vboard was told of (with where each runs), and the chosen ones not offered now. */
function AgentsLine({ c }: { c: Connector }) {
  const adv = c.advertised ?? [];
  const missing = (c.agents ?? []).filter((a) => !adv.some((x) => x.id === a.id));
  if (!adv.length && !missing.length) return null;
  return (
    <div className="mt-2 flex flex-wrap items-center gap-1.5 text-xs" aria-label={t("Chat agents")}>
      <span className="text-muted-foreground">chat:</span>
      {adv.map((a, i) => (
        <span key={a.id} className="flex items-center gap-1 rounded-md border px-1.5 py-0.5" data-advertised={a.id} title={`${a.model}${a.worker ? ` ${t("on")} ${a.worker}` : ""}${i === 0 ? ` · ${t("the default")}` : ""}`}>
          <Dot tone="ok" /> {a.name}
          <KindBadge kind={a.kind} />
          {a.worker && <span className="text-muted-foreground">· {a.worker}</span>}
        </span>
      ))}
      {missing.map((a) => (
        <span key={a.id} className="flex items-center gap-1 rounded-md border border-dashed px-1.5 py-0.5 text-muted-foreground" title={t("No connected worker runs it now (Claude Code: your own worker)")}>
          <Dot tone="off" /> {a.name || a.model} · {t("not offered now")}
        </span>
      ))}
    </div>
  );
}

/** Radix Select has no empty value: this one stands for "the server's default". */
const DEFAULT = "__default";

function ModelSelect({ id, label, kind, value, onChange, models }: { id: string; label: string; kind: string; value: string; onChange: (v: string) => void; models: Model[] }) {
  // not Claude Code as the chat model: it is an agent (Chat agents), as vboard's default it would get no MCP
  const options = models.filter((m) => m.kind === kind && agentKind(m.id) !== "agent");
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
          <SelectItem value={DEFAULT}>{t("Server default")}</SelectItem>
          {!known && <SelectItem value={value}>{value} ({t("not offered now")})</SelectItem>}
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

/** A model's kind as a small badge: a chat model (vboard's tools) or an agent (works on the board through MCP). */
function KindBadge({ kind }: { kind: "model" | "agent" }) {
  return (
    <span className={`rounded px-1.5 text-[11px] ${kind === "agent" ? "bg-primary/10 text-primary" : "bg-muted text-muted-foreground"}`} title={kind === "agent" ? t("An agent: it changes the board itself through vboard's MCP") : t("A chat model: it answers with vboard's tools")}>
      {t(kind)}
    </span>
  );
}

/** The agents vboard's chat offers, in order: chosen from the chat models (Claude Code ones are agents). */
function AgentsEditor({ value, onChange, models, error }: { value: AgentRow[]; onChange: (v: AgentRow[]) => void; models: Model[]; error?: string }) {
  const chat = models.filter((m) => m.kind === "chat" && !value.some((a) => a.model === m.id));
  const nameOf = (id: string) => models.find((m) => m.id === id)?.name ?? id;
  return (
    <div className="flex flex-col gap-1.5">
      <Label>{t("Chat agents")}</Label>
      <p className="text-xs text-muted-foreground">
        {t("What people pick from in vboard's chat panel. Claude Code runs as an agent on your own worker and changes the board through vboard's MCP. Without a list: the chat model above alone.")}
      </p>
      {value.length > 0 && (
        <ol className="flex flex-col gap-1.5">
          {value.map((a, i) => (
            <li key={a.id + i} className="flex items-center gap-2 rounded-md border px-2 py-1.5" data-agent={a.id}>
              <span className="w-4 text-right text-xs text-muted-foreground">{i + 1}</span>
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-1.5">
                  <code className="truncate font-mono text-xs">{a.model}</code>
                  <KindBadge kind={agentKind(a.model)} />
                  {i === 0 && <span className="text-[11px] text-muted-foreground">{t("default — must be a model")}</span>}
                </div>
                <Input
                  className="mt-1 h-7 text-xs"
                  value={a.name}
                  placeholder={nameOf(a.model)}
                  maxLength={80}
                  aria-label={t("Name of {name}", { name: a.model })}
                  onChange={(e) => onChange(value.map((x, k) => (k === i ? { ...x, name: e.target.value } : x)))}
                />
                {agentKind(a.model) === "agent" && (
                  <Select
                    value={a.effort || "default"}
                    onValueChange={(e) => onChange(value.map((x, k) => (k === i ? { ...x, effort: e === "default" ? "" : e } : x)))}
                  >
                    <SelectTrigger className="mt-1 h-7 w-44 text-xs" aria-label={t("Effort of {name}", { name: a.model })}>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="default">{t("Effort: the worker's default")}</SelectItem>
                      {efforts.map((e) => (
                        <SelectItem key={e} value={e}>
                          {t("Effort:")} {e}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              </div>
              <Button type="button" variant="ghost" size="icon-sm" aria-label={t("Move up")} disabled={i === 0} onClick={() => onChange(moveAgent(value, i, -1))}>
                <ArrowUp />
              </Button>
              <Button type="button" variant="ghost" size="icon-sm" aria-label={t("Move down")} disabled={i === value.length - 1} onClick={() => onChange(moveAgent(value, i, 1))}>
                <ArrowDown />
              </Button>
              <Button type="button" variant="ghost" size="icon-sm" aria-label={t("Remove {name}", { name: a.model })} onClick={() => onChange(removeAgent(value, i))}>
                <X />
              </Button>
            </li>
          ))}
        </ol>
      )}
      {value.length < maxAgents && (
        <Select value="" onValueChange={(m) => onChange(addAgent(value, m))}>
          <SelectTrigger className="w-full" aria-label={t("Add an agent")}>
            <SelectValue placeholder={chat.length ? t("Add a chat model or agent…") : t("No other chat model is offered now")} />
          </SelectTrigger>
          <SelectContent>
            {chat.map((m) => (
              <SelectItem key={m.id} value={m.id}>
                {m.id} {agentKind(m.id) === "agent" ? `(${t("agent")})` : ""}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
      {error && <p className="text-xs text-destructive">{t(error)}</p>}
    </div>
  );
}

/** A model the connector knows how to ask: images (FLUX, Qwen-Image, SeedVR2 through mflux; BiRefNet cutouts), videos (LTX), and the music, sound and speech models the worker describes. */
const isGenerator = (m: Model) =>
  (m.kind === "image" && (m.backend === "mflux" || m.backend === "cutout")) ||
  (m.kind === "video" && m.backend === "ltx") ||
  ((m.kind === "music" || m.kind === "sound") && !!m.audio) ||
  (m.kind === "speech" && !!m.audio?.maxChars);

/** On a worker's disk (only those are offered to vboard: nothing is downloaded for a board). */
const onDisk = (m: Model) => !!m.here || !!m.workers?.some((w) => w.here);

/** The models vboard may make pictures, videos, songs, sounds and speech on the board with, in order (the first of each kind is the default). */
function GeneratorsEditor({ value, onChange, models }: { value: string[]; onChange: (v: string[]) => void; models: Model[] }) {
  const options = models.filter((m) => isGenerator(m) && !value.includes(m.id));
  const model = (id: string) => models.find((m) => m.id === id);
  const all = value.includes(allGenerators);
  return (
    <div className="flex flex-col gap-1.5">
      <Label>{t("Generators on the board")}</Label>
      <p className="text-xs text-muted-foreground">
        {t("The models vboard may make pictures, videos, songs, sounds and speech with (the first of each kind is the default). Offered only while a connected worker has the model on disk. “All on disk” adds every one there, also those downloaded later from vboard, after the ones named before it. Without a list: none.")}
      </p>
      {value.length > 0 && (
        <ol className="flex flex-col gap-1.5">
          {value.map((id, i) => {
            const m = model(id);
            return (
              <li key={id} className="flex items-center gap-2 rounded-md border px-2 py-1.5" data-generator={id}>
                <span className="w-4 text-right text-xs text-muted-foreground">{i + 1}</span>
                <div className="flex min-w-0 flex-1 flex-wrap items-center gap-1.5">
                  {id === allGenerators ? <span className="text-xs">{t("All on disk")}</span> : <code className="truncate font-mono text-xs">{id}</code>}
                  {m && <span className="rounded bg-muted px-1.5 text-[11px] text-muted-foreground">{m.kind}</span>}
                  {m?.nonCommercial && <span className="text-[11px] text-muted-foreground">{t("non-commercial")}</span>}
                  {id !== allGenerators && (!m || !onDisk(m)) && <span className="text-[11px] text-muted-foreground">{t("not on a worker's disk now")}</span>}
                </div>
                <Button type="button" variant="ghost" size="icon-sm" aria-label={t("Move up")} disabled={i === 0} onClick={() => onChange(moveGenerator(value, i, -1))}>
                  <ArrowUp />
                </Button>
                <Button type="button" variant="ghost" size="icon-sm" aria-label={t("Move down")} disabled={i === value.length - 1} onClick={() => onChange(moveGenerator(value, i, 1))}>
                  <ArrowDown />
                </Button>
                <Button type="button" variant="ghost" size="icon-sm" aria-label={t("Remove {name}", { name: id })} onClick={() => onChange(removeGenerator(value, i))}>
                  <X />
                </Button>
              </li>
            );
          })}
        </ol>
      )}
      {value.length < maxGenerators && (
        <Select value="" onValueChange={(m) => onChange(addGenerator(value, m))}>
          <SelectTrigger className="w-full" aria-label={t("Add a generator model")}>
            <SelectValue placeholder={options.length || !all ? t("Add a generator model…") : t("No other generator model is listed")} />
          </SelectTrigger>
          <SelectContent>
            {!all && <SelectItem value={allGenerators}>{t("All on disk")}</SelectItem>}
            {options.map((m) => (
              <SelectItem key={m.id} value={m.id}>
                {m.id} ({m.kind}){onDisk(m) ? "" : ` · ${t("not on disk")}`}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
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

  const set = (k: Exclude<keyof ConnectorForm, "agents" | "generators">) => (v: string) => setForm((f) => ({ ...f, [k]: v }));

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
      toast.success(editing ? t("Saved") : t("Connector added"), { description: t("It connects in the background; the state shows here.") });
    } catch (err) {
      setFailure(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  const field = (k: Exclude<keyof ConnectorForm, "agents" | "generators">, label: string, props: React.ComponentProps<typeof Input> = {}, hint?: string) => (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={`connector-${k}`}>{label}</Label>
      <Input id={`connector-${k}`} value={form[k]} onChange={(e) => set(k)(e.target.value)} aria-invalid={!!errors[k] || undefined} {...props} />
      {errors[k] ? <p className="text-xs text-destructive">{t(errors[k])}</p> : hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );

  return (
    <Dialog open={!!target} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg">
        <form onSubmit={submit} className="contents">
          <DialogHeader>
            <DialogTitle>{editing ? t("Edit “{name}”", { name: editing.name }) : t("Add a vboard connector")}</DialogTitle>
            <DialogDescription>{t("The server dials vboard's classifier bridge with the token and answers as vboard's local AI.")}</DialogDescription>
          </DialogHeader>
          <div className="flex flex-col gap-3">
            {field("name", t("Name"), { placeholder: defaultName, autoFocus: true, maxLength: 80 }, t("vboard shows it as the local AI's name; blank: “ai-connector”."))}
            {field("url", t("vboard address"), { placeholder: "https://vboard.example.com", inputMode: "url", autoComplete: "off" })}
            {field(
              "token",
              "Token",
              { type: "password", placeholder: editing ? t("unchanged") : "vbp_…", autoComplete: "off" },
              editing ? t("Leave blank to keep the stored token.") : t("A vboard personal or board token. It is stored on the server and never shown again."),
            )}
            {field("board", t("Board id (optional)"), { placeholder: "b_…", autoComplete: "off" }, t("Only for this board; empty for every board the token may see."))}
            <div className="grid gap-3 sm:grid-cols-3">
              <ModelSelect id="connector-classify" label={t("Classify")} kind="classify" value={form.classify} onChange={set("classify")} models={models} />
              <ModelSelect id="connector-embed" label={t("Embed")} kind="embed" value={form.embed} onChange={set("embed")} models={models} />
              <ModelSelect id="connector-chat" label="Chat" kind="chat" value={form.chat} onChange={set("chat")} models={models} />
            </div>
            {errors.chat && <p className="text-xs text-destructive">{t(errors.chat)}</p>}
            <AgentsEditor value={form.agents} onChange={(agents) => setForm((f) => ({ ...f, agents }))} models={models} error={errors.agents} />
            <GeneratorsEditor value={form.generators} onChange={(generators) => setForm((f) => ({ ...f, generators }))} models={models} />
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
