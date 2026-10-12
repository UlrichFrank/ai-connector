import { CircleCheck, CircleX, Link2, Loader2 } from "lucide-react";
import { useEffect, useState, type ComponentType, type FormEvent, type ReactNode } from "react";
import { toast } from "sonner";
import { ApiError, api, errorText } from "@/api/client";
import type { Client, LinkRequest } from "@/api/types";
import { Page, PageHeader } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { navigate, useRoute } from "@/lib/route";
import { useSession } from "@/store/session";

/** The digits of a PIN as typed ("123 456", "123-456"): the server keeps the digits, so does the URL. */
const digits = (s: string) => s.replace(/\D/g, "");

type State =
  | { kind: "enter" }
  | { kind: "loading" }
  | { kind: "found"; req: LinkRequest }
  | { kind: "missing"; message: string }
  | { kind: "approved"; client: Client }
  | { kind: "denied"; name: string };

/**
 * An app asks for a client token (docs/api.md, "Linking an app"): it shows a PIN and <hub>/pin, which leads here
 * (#/link/<PIN>). Approving makes the client, acting for the signed-in user; the app picks the token up itself.
 */
export function LinkPage() {
  const { arg } = useRoute();
  return <LinkFlow key={arg} arg={arg} />;
}

/** One PIN's flow (a new PIN in the URL starts it over). */
function LinkFlow({ arg }: { arg: string }) {
  const code = digits(arg);
  const user = useSession((s) => s.info?.user);
  const [state, setState] = useState<State>({ kind: code ? "loading" : "enter" });
  const [attempt, setAttempt] = useState(0); // the same PIN typed again looks it up again

  useEffect(() => {
    if (!code) return;
    let current = true;
    api.linkRequest(code).then(
      (req) => current && setState({ kind: "found", req }),
      (e: unknown) => current && setState({ kind: "missing", message: missingText(e) }),
    );
    return () => void (current = false);
  }, [code, attempt]);

  const lookUp = (c: string) => {
    if (c !== code) return navigate("link", c);
    setState({ kind: "loading" });
    setAttempt((n) => n + 1);
  };

  return (
    <Page>
      <PageHeader title="Link a device" description="An app shows a PIN and asks for its own client token. Approve it here and the app picks the token up by itself; nobody copies it." />
      <div className="max-w-md">
        {state.kind === "enter" || state.kind === "missing" ? (
          <EnterCode initial={arg} error={state.kind === "missing" ? state.message : ""} onSubmit={lookUp} />
        ) : state.kind === "loading" ? (
          <div className="flex justify-center py-10">
            <Loader2 className="size-5 animate-spin text-muted-foreground" aria-label="Loading" />
          </div>
        ) : state.kind === "found" ? (
          <Decide code={code} req={state.req} user={user} onDone={setState} />
        ) : state.kind === "approved" ? (
          <Outcome icon={CircleCheck} tone="ok" title={`“${state.client.name}” is linked`}>
            It picks its token up within a few seconds. You find it under Clients, where it can be revoked.
            <div className="mt-4 flex gap-2">
              <Button variant="outline" onClick={() => navigate("clients")}>
                Show clients
              </Button>
              <Button variant="ghost" onClick={() => navigate("link")}>
                Link another
              </Button>
            </div>
          </Outcome>
        ) : (
          <Outcome icon={CircleX} tone="bad" title={`“${state.name}” was denied`}>
            The app gets no token. It can ask again with a new PIN.
            <div className="mt-4">
              <Button variant="ghost" onClick={() => navigate("link")}>
                Link another
              </Button>
            </div>
          </Outcome>
        )}
      </div>
    </Page>
  );
}

function missingText(e: unknown): string {
  if (e instanceof ApiError && e.status === 404) return "No app is waiting with this PIN. It may have expired (a PIN lasts 10 minutes) or been answered already.";
  if (e instanceof ApiError && e.status === 429) return "Too many PINs tried. Wait a minute and try again.";
  return errorText(e);
}

function EnterCode({ initial, error, onSubmit }: { initial: string; error: string; onSubmit: (code: string) => void }) {
  const [value, setValue] = useState(initial);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const c = digits(value);
    if (c) onSubmit(c);
  };
  return (
    <form onSubmit={submit} className="rounded-xl border p-5">
      <div className="grid gap-2">
        <Label htmlFor="link-code">PIN shown on the device</Label>
        <Input
          id="link-code"
          inputMode="numeric"
          autoComplete="one-time-code"
          placeholder="123 456"
          maxLength={12}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          aria-invalid={!!error || undefined}
          className="h-10 font-mono text-lg tracking-widest"
          autoFocus
        />
        {error && <p className="text-sm text-destructive">{error}</p>}
      </div>
      <Button type="submit" className="mt-4" disabled={!digits(value)}>
        <Link2 /> Look up
      </Button>
    </form>
  );
}

function Decide({ code, req, user, onDone }: { code: string; req: LinkRequest; user?: string; onDone: (s: State) => void }) {
  const [busy, setBusy] = useState<"" | "approve" | "deny">("");
  const decide = async (approve: boolean) => {
    setBusy(approve ? "approve" : "deny");
    try {
      if (approve) onDone({ kind: "approved", client: (await api.approveLink(code)).client });
      else {
        await api.denyLink(code);
        onDone({ kind: "denied", name: req.name });
      }
    } catch (e) {
      toast.error(approve ? "Could not approve" : "Could not deny", { description: missingText(e) });
      setBusy("");
    }
  };
  return (
    <div className="rounded-xl border p-5">
      <div className="mb-1 font-mono text-sm text-muted-foreground tabular-nums">{req.userCode}</div>
      <p>
        <span className="font-medium">“{req.name}”</span> asks for a client token. It will act as <span className="font-medium">{user || "you"}</span>: its Claude Code jobs run on your workers.
      </p>
      <div className="mt-5 flex gap-2">
        <Button onClick={() => void decide(true)} disabled={!!busy}>
          {busy === "approve" && <Loader2 className="animate-spin" />} Approve
        </Button>
        <Button variant="outline" onClick={() => void decide(false)} disabled={!!busy}>
          {busy === "deny" && <Loader2 className="animate-spin" />} Deny
        </Button>
      </div>
    </div>
  );
}

function Outcome({ icon: Icon, tone, title, children }: { icon: ComponentType<{ className?: string }>; tone: "ok" | "bad"; title: string; children: ReactNode }) {
  return (
    <div className="flex items-start gap-3 rounded-xl border p-5">
      <Icon className={tone === "ok" ? "mt-0.5 size-5 shrink-0 text-emerald-500" : "mt-0.5 size-5 shrink-0 text-muted-foreground"} />
      <div className="min-w-0 text-sm text-muted-foreground">
        <div className="mb-1 text-base font-medium text-foreground">{title}</div>
        {children}
      </div>
    </div>
  );
}
