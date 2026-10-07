import { Camera, Check, CircleCheck, Copy, Loader2, RotateCw, ScanQrCode, Timer } from "lucide-react";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { toast } from "sonner";
import { api, ApiError, errorText } from "@/api/client";
import type { Device, Invite } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { countdown, inviteDeadline } from "@/lib/devices";
import { useNow } from "@/lib/now";
import { cn } from "@/lib/utils";
import { copyText } from "@/lib/clipboard";
import { QrCode } from "./QrCode";

/** How often the open dialog looks for the iPhone that used the code. */
const POLL = 3000;

type State =
  | { step: "loading" }
  | { step: "error"; error: unknown }
  | { step: "code"; invite: Invite; deadline: number }
  | { step: "done"; device: Device };

/**
 * Devices → "Sign in an iPhone": a QR code with a one-time code (POST /api/invites) that signs in one iPhone, once —
 * with a countdown to when it runs out ("New code" then), the link to copy, and "Signed in ✓" as soon as an iPhone used
 * it (the device list is polled while the dialog is open). `onSignedIn` lets the page load its list again.
 */
export function InviteDialog({ open, onOpenChange, onSignedIn }: { open: boolean; onOpenChange: (open: boolean) => void; onSignedIn: () => void }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] grid-cols-1 overflow-y-auto sm:max-w-md [&>*]:min-w-0" data-invite-dialog="">
        {/* a fresh code every time it opens */}
        {open && <InviteBody onClose={() => onOpenChange(false)} onSignedIn={onSignedIn} />}
      </DialogContent>
    </Dialog>
  );
}

function InviteBody({ onClose, onSignedIn }: { onClose: () => void; onSignedIn: () => void }) {
  const [state, setState] = useState<State>({ step: "loading" });
  // the devices before the code was made: a new one invited by this device is the iPhone that used it
  const known = useRef<{ me: string; ids: Set<string> } | null>(null);
  const now = useNow(1000);

  const fetchCode = useCallback(async () => {
    try {
      const [invite, devices] = await Promise.all([api.createInvite(), api.devices()]);
      known.current = { me: devices.find((d) => d.current)?.id ?? "", ids: new Set(devices.map((d) => d.id)) };
      setState({ step: "code", invite, deadline: inviteDeadline(invite.expires) });
    } catch (e) {
      setState({ step: "error", error: e });
    }
  }, []);
  useEffect(() => {
    fetchCode();
  }, [fetchCode]);
  const make = () => {
    setState({ step: "loading" });
    fetchCode();
  };

  const expired = state.step === "code" && now >= state.deadline;
  const waiting = state.step === "code" && !expired;

  // while the code works: has an iPhone used it?
  useEffect(() => {
    if (!waiting) return;
    let live = true;
    const t = setInterval(async () => {
      const k = known.current;
      const list = await api.devices().catch(() => null);
      const fresh = k && list?.find((d) => !k.ids.has(d.id) && d.invitedBy === k.me);
      if (live && fresh) {
        setState({ step: "done", device: fresh });
        onSignedIn();
      }
    }, POLL);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, [waiting, onSignedIn]);

  return (
    <>
      <DialogHeader>
        <DialogTitle>Sign in an iPhone</DialogTitle>
        <DialogDescription>This code signs in one iPhone, once — no password needed. It works for 10 minutes.</DialogDescription>
      </DialogHeader>

      {state.step === "done" ? (
        <Done device={state.device} />
      ) : state.step === "error" ? (
        <Failed error={state.error} onRetry={make} />
      ) : (
        <div className="flex flex-col items-center gap-3">
          <div className="relative w-full max-w-60">
            {state.step === "code" ? (
              <QrCode value={state.invite.url} label="QR code that signs in an iPhone" blurred={expired} />
            ) : (
              <Skeleton className="aspect-square w-full rounded-xl" />
            )}
            {expired && (
              <div className="absolute inset-0 grid place-items-center">
                <Button onClick={make} data-new-code="">
                  <RotateCw /> New code
                </Button>
              </div>
            )}
          </div>
          <div
            role="timer"
            aria-live="off"
            className={cn("flex h-5 items-center gap-1.5 text-sm tabular-nums", expired ? "text-destructive" : "text-muted-foreground")}
            data-invite-state={state.step === "loading" ? "loading" : expired ? "expired" : "waiting"}
          >
            {state.step === "loading" ? (
              <>
                <Loader2 className="size-3.5 animate-spin" /> Making a code …
              </>
            ) : expired ? (
              <>
                <Timer className="size-3.5" /> This code ran out. Make a new one.
              </>
            ) : (
              state.step === "code" && (
                <>
                  <Timer className="size-3.5" /> Works for <span className="font-medium text-foreground">{countdown(state.deadline - now)}</span>
                </>
              )
            )}
          </div>
        </div>
      )}

      {state.step !== "done" && state.step !== "error" && (
        <>
          <ol className="flex flex-col gap-2">
            <Step icon={Camera}>
              Open the <span className="font-medium">Camera</span> on the iPhone and point it at the code, then tap the mlxlab banner.
            </Step>
            <Step icon={ScanQrCode}>
              Or scan it in the mlxlab app: <span className="font-medium">Sign in → Scan QR code</span>.
            </Step>
          </ol>
          {state.step === "code" && !expired && <LinkBlock url={state.invite.url} />}
        </>
      )}

      <DialogFooter className="items-center sm:justify-between">
        <p className="text-xs text-muted-foreground max-sm:text-center">
          {state.step === "done" ? "It appears under its user in the list." : "The iPhone may not sign in others until an owner allows it."}
        </p>
        {state.step === "done" ? (
          <div className="flex gap-2 max-sm:flex-col-reverse">
            <Button variant="outline" onClick={make}>
              Sign in another
            </Button>
            <Button onClick={onClose}>Done</Button>
          </div>
        ) : (
          <Button variant="outline" onClick={onClose}>
            Close
          </Button>
        )}
      </DialogFooter>
    </>
  );
}

function Step({ icon: Icon, children }: { icon: typeof Camera; children: ReactNode }) {
  return (
    <li className="flex items-start gap-2.5 text-sm">
      <span className="grid size-6 shrink-0 place-items-center rounded-md bg-muted text-muted-foreground">
        <Icon className="size-3.5" />
      </span>
      <span className="pt-0.5 text-muted-foreground [&_.font-medium]:text-foreground">{children}</span>
    </li>
  );
}

/** The fallback: the same link, to send to the iPhone (AirDrop, Messages) and open there. */
function LinkBlock({ url }: { url: string }) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(t);
  }, [copied]);
  const copy = async () => {
    try {
      await copyText(url);
      setCopied(true);
    } catch (e) {
      toast.error("Could not copy", { description: e instanceof Error ? e.message : String(e) });
    }
  };
  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <div className="text-xs text-muted-foreground">No camera? Send this link to the iPhone and open it there:</div>
      <div className="flex items-center gap-1 rounded-lg border bg-muted/50 py-1 pr-1 pl-3">
        <code className="min-w-0 flex-1 truncate font-mono text-xs select-all" title={url} data-invite-url="">
          {url}
        </code>
        <Button variant={copied ? "secondary" : "ghost"} size="xs" onClick={copy}>
          {copied ? <Check /> : <Copy />} {copied ? "Copied" : "Copy link"}
        </Button>
      </div>
    </div>
  );
}

function Done({ device }: { device: Device }) {
  return (
    <div role="status" className="flex flex-col items-center gap-2 py-6 text-center" data-invite-state="done">
      <CircleCheck className="size-10 text-emerald-500" />
      <div className="font-medium">Signed in ✓</div>
      <p className="text-sm text-muted-foreground">
        <span className="font-medium text-foreground">{device.name}</span>
        {device.user ? ` of ${device.user}` : ""} can use mlxlab now. The code is used up.
      </p>
    </div>
  );
}

function Failed({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  const forbidden = error instanceof ApiError && error.status === 403;
  return (
    <div role="alert" className="flex flex-col items-center gap-3 py-4 text-center">
      <p className="text-sm text-muted-foreground">
        {forbidden ? "This device may not sign in other iPhones. A device signed in with the password can allow it on the Devices page." : errorText(error)}
      </p>
      {!forbidden && (
        <Button variant="outline" size="sm" onClick={onRetry}>
          <RotateCw /> Try again
        </Button>
      )}
    </div>
  );
}
