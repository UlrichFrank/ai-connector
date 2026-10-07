import { Eye, Loader2, PlugZap, RefreshCw, Repeat2, Server, ShieldQuestion } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { HomeTool, ToolsResponse } from "@/api/types";
import { Code, ErrorNotice } from "@/components/connection/ErrorNotice";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { cn } from "@/lib/utils";

/** "smarthome__set_light" → ["smarthome", "set_light"]. */
function split(name: string): [string | undefined, string] {
  const i = name.indexOf("__");
  return i > 0 ? [name.slice(0, i), name.slice(i + 2)] : [undefined, name];
}

/** "set_light" → "Set light". */
function humanize(s: string) {
  const t = s.replace(/[_-]+/g, " ").replace(/([a-z])([A-Z])/g, "$1 $2").trim().toLowerCase();
  return t.charAt(0).toUpperCase() + t.slice(1);
}

/**
 * Settings → Tools: the tools of home chats (the MCP servers in mcp.json), and which of those that change
 * something run without asking ("Always allow", kept on the Mac in approvals.json, docs/chat-tools.md).
 */
export function ToolsPage(_: { arg: string }) {
  const [data, setData] = useState<ToolsResponse | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<Record<string, boolean>>({});

  const load = useCallback(() => {
    api.tools().then(
      (r) => {
        setData({ ...r, always: r.always ?? [], servers: r.servers ?? [] });
        setError(null);
        setLoading(false);
      },
      (e: unknown) => {
        setError(e);
        setLoading(false);
      },
    );
  }, []);
  useEffect(load, [load]);
  // asking the servers again takes a moment (each one is connected to)
  const reload = () => {
    setLoading(true);
    load();
  };

  const byName = useMemo(() => {
    const m = new Map<string, HomeTool>();
    for (const s of data?.servers ?? []) for (const t of s.tools) m.set(t.name, t);
    return m;
  }, [data]);

  const set = async (name: string, on: boolean) => {
    setBusy((b) => ({ ...b, [name]: true }));
    try {
      const always = on ? await api.allowAlways(name) : await api.askAgain(name);
      setData((d) => d && { ...d, always, servers: d.servers.map((s) => ({ ...s, tools: s.tools.map((t) => ({ ...t, always: always.includes(t.name) })) })) });
      const [, tool] = split(name);
      toast.success(on ? `“${humanize(tool)}” is allowed always` : `“${humanize(tool)}” asks again`, {
        description: on ? "It runs without an approval card from now on." : "Each call waits for your OK again.",
      });
    } catch (e) {
      toast.error(on ? "Could not allow it always" : "Could not change it", { description: errorText(e) });
    } finally {
      setBusy((b) => ({ ...b, [name]: false }));
    }
  };

  const always = data?.always ?? [];
  const servers = data?.servers ?? [];

  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-8 px-4 py-6 sm:px-6 sm:py-8">
      <header className="flex items-start gap-3">
        <div className="flex min-w-0 flex-1 flex-col gap-1">
          <h1 className="text-lg font-semibold">Tools</h1>
          <p className="text-sm text-muted-foreground">
            What home chats can use: the tools of your MCP servers. Tools that change something wait for your OK on each call — unless you allow them
            always. Read-only tools never ask.
          </p>
        </div>
        <Button variant="ghost" size="icon-sm" className="shrink-0 text-muted-foreground" onClick={reload} disabled={loading} title="Ask the servers again">
          <RefreshCw className={cn(loading && "animate-spin")} />
        </Button>
      </header>

      {error != null && <ErrorNotice error={error} onRetry={reload} />}

      <section className="flex flex-col gap-3">
        <div>
          <h2 className="text-sm font-semibold">
            Allowed always <span className="font-normal text-muted-foreground">· {always.length}</span>
          </h2>
          <p className="mt-0.5 text-sm text-muted-foreground">These run without an approval card, in every chat and on every device.</p>
        </div>
        {data && always.length === 0 && (
          <div className="flex items-center gap-3 rounded-xl border border-dashed px-4 py-4 text-sm text-muted-foreground">
            <ShieldQuestion className="size-4 shrink-0" />
            <span>
              Nothing yet: every change asks first. Choose <span className="font-medium text-foreground">Always allow</span> on an approval in a chat, or switch a
              tool on below.
            </span>
          </div>
        )}
        {always.length > 0 && (
          <ul className="flex flex-col divide-y overflow-hidden rounded-xl border bg-card/40">
            {always.map((name) => {
              const [server, tool] = split(name);
              const known = byName.get(name);
              return (
                <li key={name} className="flex items-center gap-3 px-4 py-3">
                  <span className="grid size-7 shrink-0 place-items-center rounded-md bg-primary/10 text-primary">
                    <Repeat2 className="size-4" />
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-sm font-medium">
                      {humanize(tool)}
                      {server && <span className="font-normal text-muted-foreground"> · {server}</span>}
                    </div>
                    <div className="truncate text-xs text-muted-foreground">
                      {known?.description || (data && !known ? "Not offered right now (its server isn't answering, or the tool is gone)." : name)}
                    </div>
                  </div>
                  <Button variant="outline" size="sm" disabled={busy[name]} onClick={() => set(name, false)} title="Each call waits for your OK again">
                    {busy[name] && <Loader2 className="animate-spin" />} Ask again
                  </Button>
                </li>
              );
            })}
          </ul>
        )}
        {!data && loading && <Skeleton className="h-16 rounded-xl" />}
      </section>

      {data?.error && (
        <div className="rounded-xl border border-amber-500/30 bg-amber-500/10 px-4 py-3 text-sm">
          <div className="font-medium">The MCP servers can't be read</div>
          <div className="mt-0.5 text-muted-foreground">{data.error}</div>
        </div>
      )}

      {data && !data.error && servers.length === 0 && (
        <div className="flex flex-col items-center gap-2 rounded-xl border border-dashed px-6 py-10 text-center">
          <PlugZap className="size-6 text-muted-foreground" />
          <div className="font-medium">No MCP servers</div>
          <p className="max-w-md text-sm text-muted-foreground">
            Home chats get their tools from the servers in <Code>~/.config/mlxlab/mcp.json</Code> on the Mac (see docs/chat-tools.md).
          </p>
        </div>
      )}

      {servers.map((s) => {
        const changes = s.tools.filter((t) => t.changes).length;
        return (
          <section key={s.name} className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Server className="size-4 text-muted-foreground" />
              <h2 className="text-sm font-semibold">
                {s.name}{" "}
                {!s.error && (
                  <span className="font-normal text-muted-foreground">
                    · {s.tools.length} {s.tools.length === 1 ? "tool" : "tools"}
                    {changes > 0 && `, ${changes} that change something`}
                  </span>
                )}
              </h2>
            </div>
            {s.error ? (
              <div className="rounded-xl border border-amber-500/30 bg-amber-500/10 px-4 py-3 text-sm">
                <div className="font-medium">Not answering right now</div>
                <div className="mt-0.5 line-clamp-3 break-words text-muted-foreground">{s.error}</div>
              </div>
            ) : (
              <ul className="flex flex-col divide-y overflow-hidden rounded-xl border bg-card/40">
                {s.tools.map((t) => (
                  <ToolRow key={t.name} tool={t} busy={!!busy[t.name]} onChange={(on) => set(t.name, on)} />
                ))}
              </ul>
            )}
          </section>
        );
      })}

      {!data && loading && (
        <div className="flex flex-col gap-3">
          <Skeleton className="h-4 w-40" />
          <Skeleton className="h-40 rounded-xl" />
        </div>
      )}
    </div>
  );
}

function ToolRow({ tool: t, busy, onChange }: { tool: HomeTool; busy: boolean; onChange: (on: boolean) => void }) {
  const [, name] = split(t.name);
  const id = `always-${t.name}`;
  return (
    <li className="flex items-start gap-3 px-4 py-3">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span className="text-sm font-medium">{humanize(name)}</span>
          <span className="font-mono text-[11px] text-muted-foreground">{t.name}</span>
        </div>
        {t.description && <p className="mt-0.5 line-clamp-2 text-xs text-muted-foreground">{t.description}</p>}
      </div>
      {t.changes ? (
        <label htmlFor={id} className="flex shrink-0 cursor-pointer items-center gap-2 pt-0.5 text-xs text-muted-foreground">
          {busy && <Loader2 className="size-3.5 animate-spin" />}
          <span className={cn(t.always && "text-foreground")}>{t.always ? "Always allowed" : "Always allow"}</span>
          <Switch id={id} checked={t.always} disabled={busy} onCheckedChange={onChange} />
        </label>
      ) : (
        <Badge variant="outline" className="shrink-0 gap-1 font-normal text-muted-foreground" title="Read-only: it changes nothing, so it never asks">
          <Eye className="size-3" /> never asks
        </Badge>
      )}
    </li>
  );
}
