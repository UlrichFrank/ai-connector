import { KeyRound, Loader2, Plus, Trash2 } from "lucide-react";
import { useCallback, useEffect, useState, type FormEvent } from "react";
import { toast } from "sonner";
import { api, errorText, isNotAvailable } from "@/api/client";
import type { Client, ClientCreated } from "@/api/types";
import { Ago, CodeBlock, Empty, ErrorNotice, Page, PageHeader, ShownOnce } from "@/components/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { DEFAULT_CHAT_MODEL, DEFAULT_EMBED_MODEL, pickModel, snippets, TOKEN_PLACEHOLDER, type SnippetGroup } from "@/lib/snippets";
import { cn } from "@/lib/utils";
import { useLive } from "@/store/live";

export function ClientsPage() {
  const [list, setList] = useState<Client[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [creating, setCreating] = useState(false);
  // the token made last on this page, for the snippets (it is not shown again after a reload)
  const [fresh, setFresh] = useState<ClientCreated | null>(null);

  const load = useCallback(async () => {
    try {
      setList(await api.clients());
      setError(null);
    } catch (e) {
      setError(e);
    }
  }, []);
  useEffect(() => void load(), [load]);

  const missing = isNotAvailable(error);

  return (
    <Page>
      <PageHeader title="Clients" description="API tokens for applications (vboard, docscanner, scripts): they may use the OpenAI, TEI, Laya and jobs APIs, nothing else.">
        <Button onClick={() => setCreating(true)} disabled={missing}>
          <Plus /> New token
        </Button>
      </PageHeader>

      {error && !list ? (
        <ErrorNotice error={error} onRetry={load} what="Client tokens" />
      ) : list === null ? (
        <div className="flex justify-center py-10">
          <Loader2 className="size-5 animate-spin text-muted-foreground" />
        </div>
      ) : list.length ? (
        <div className="overflow-x-auto rounded-xl border">
          <table className="w-full text-sm">
            <thead className="bg-muted/40 text-left text-xs text-muted-foreground">
              <tr>
                <th className="px-3 py-2 font-medium">Name</th>
                <th className="px-3 py-2 font-medium max-sm:hidden">Owner</th>
                <th className="px-3 py-2 font-medium max-sm:hidden">Created</th>
                <th className="px-3 py-2 font-medium">Last used</th>
                <th className="w-0 px-2 py-2" />
              </tr>
            </thead>
            <tbody>
              {list.map((c) => (
                <ClientRow key={c.id} c={c} onRevoked={load} />
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <Empty icon={KeyRound} title="No client tokens yet">
          Create one per application, so each can be revoked alone and its jobs show its name.
        </Empty>
      )}

      <Usage token={fresh?.token} tokenName={fresh?.client.name} />

      <CreateClientDialog
        open={creating}
        onOpenChange={setCreating}
        onCreated={(c) => {
          setFresh(c);
          void load();
        }}
      />
    </Page>
  );
}

function ClientRow({ c, onRevoked }: { c: Client; onRevoked: () => void }) {
  const [confirm, setConfirm] = useState(false);
  const revoke = async () => {
    try {
      await api.revokeClient(c.id);
      toast.success(`“${c.name}” revoked`);
      onRevoked();
    } catch (e) {
      toast.error("Could not revoke", { description: errorText(e) });
    }
  };
  return (
    <tr className="border-t" data-client={c.id}>
      <td className="px-3 py-2 font-medium">{c.name}</td>
      <td className="px-3 py-2 text-muted-foreground max-sm:hidden">{c.owner || "—"}</td>
      <td className="px-3 py-2 text-muted-foreground max-sm:hidden">
        <Ago iso={c.created} />
      </td>
      <td className="px-3 py-2 text-muted-foreground">
        <Ago iso={c.lastSeen} />
      </td>
      <td className="px-2 py-1.5">
        <Button variant="ghost" size="xs" onClick={() => setConfirm(true)} aria-label={`Revoke ${c.name}`}>
          <Trash2 /> Revoke
        </Button>
        <AlertDialog open={confirm} onOpenChange={setConfirm}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>Revoke “{c.name}”?</AlertDialogTitle>
              <AlertDialogDescription>Applications using this token get 401 from now on. This cannot be undone.</AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>Cancel</AlertDialogCancel>
              <AlertDialogAction variant="destructive" onClick={() => void revoke()}>
                Revoke
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </td>
    </tr>
  );
}

function CreateClientDialog({ open, onOpenChange, onCreated }: { open: boolean; onOpenChange: (o: boolean) => void; onCreated: (c: ClientCreated) => void }) {
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [created, setCreated] = useState<ClientCreated | null>(null);
  const change = (o: boolean) => {
    onOpenChange(o);
    if (!o)
      setTimeout(() => {
        setCreated(null);
        setName("");
        setError("");
      }, 200);
  };
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!name.trim()) return setError("A name is needed, e.g. the application's.");
    setBusy(true);
    setError("");
    try {
      const c = await api.createClient(name.trim());
      setCreated(c);
      onCreated(c);
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open={open} onOpenChange={change}>
      <DialogContent className="sm:max-w-lg">
        {created ? (
          <>
            <DialogHeader>
              <DialogTitle>Token for “{created.client.name}”</DialogTitle>
              <DialogDescription>Send it as <code className="font-mono">Authorization: Bearer …</code>. The snippets below now have it filled in.</DialogDescription>
            </DialogHeader>
            <CodeBlock label="Client token" text={created.token} />
            <ShownOnce>
              <span className="font-medium">The token is shown only now.</span> <span className="text-muted-foreground">Store it in the application's settings. Lost it? Revoke it and create a new one.</span>
            </ShownOnce>
            <DialogFooter>
              <Button onClick={() => change(false)}>Done</Button>
            </DialogFooter>
          </>
        ) : (
          <form onSubmit={submit} className="contents">
            <DialogHeader>
              <DialogTitle>New client token</DialogTitle>
              <DialogDescription>One token per application. Its name shows on the jobs it submits.</DialogDescription>
            </DialogHeader>
            <div className="flex flex-col gap-2">
              <Label htmlFor="client-name">Name</Label>
              <Input id="client-name" value={name} onChange={(e) => setName(e.target.value)} autoFocus maxLength={80} placeholder="docscanner, vboard, scripts …" />
              {error && <p className="text-sm text-destructive">{error}</p>}
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => change(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={busy}>
                {busy && <Loader2 className="animate-spin" />} Create
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}

/** Copy-paste snippets with this server's address (and the token just created) filled in. */
function Usage({ token, tokenName }: { token?: string; tokenName?: string }) {
  const models = useLive((s) => s.models);
  const [group, setGroup] = useState<SnippetGroup>("openai");
  const all = snippets({
    origin: location.origin,
    token,
    chatModel: pickModel(models, "chat", DEFAULT_CHAT_MODEL),
    embedModel: models.some((m) => m.id === DEFAULT_EMBED_MODEL) ? DEFAULT_EMBED_MODEL : pickModel(models, "embed", DEFAULT_EMBED_MODEL),
  });
  const shown = all.filter((s) => s.group === group);
  return (
    <section className="mt-10">
      <div className="mb-3 flex flex-wrap items-end justify-between gap-2">
        <div>
          <h3 className="font-semibold">Use it</h3>
          <p className="text-sm text-muted-foreground">
            {token ? (
              <>
                With the token of <span className="font-medium text-foreground">{tokenName}</span> filled in.
              </>
            ) : (
              <>
                Replace <code className="font-mono">{TOKEN_PLACEHOLDER}</code> with a client token (create one above and it is filled in).
              </>
            )}
          </p>
        </div>
        <div role="tablist" className="flex rounded-lg bg-muted p-0.5 text-sm">
          {(
            [
              ["openai", "OpenAI API"],
              ["apps", "vboard & docscanner"],
            ] as const
          ).map(([g, label]) => (
            <button key={g} role="tab" aria-selected={group === g} onClick={() => setGroup(g)} className={cn("rounded-md px-3 py-1 text-muted-foreground", group === g && "bg-background text-foreground shadow-sm")}>
              {label}
            </button>
          ))}
        </div>
      </div>
      <div className="flex flex-col gap-4">
        {shown.map((s) => (
          <div key={s.id}>
            <div className="mb-1.5 text-sm">
              <span className="font-medium">{s.label}</span> <span className="text-muted-foreground">— {s.hint}</span>
            </div>
            <CodeBlock label={s.lang === "env" ? "Environment" : s.lang === "python" ? "Python" : "Shell"} text={s.text} secret={token} />
          </div>
        ))}
      </div>
      {group === "apps" && (
        <p className="mt-3 text-xs text-muted-foreground">
          vboard can also be reached the other way round, without changing its settings: add a vboard connector on the Connectors page, and this server dials vboard instead.
        </p>
      )}
    </section>
  );
}
