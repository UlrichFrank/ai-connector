import { KeyRound, Plus, QrCode } from "lucide-react";
import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Device, Worker } from "@/api/types";
import { ErrorNotice } from "@/components/connection/ErrorNotice";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { groupByUser, initials, invitedByText } from "@/lib/devices";
import { navigate, useRoute } from "@/lib/route";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";
import { useSession } from "@/store/session";
import { ConnectMacDialog } from "./ConnectMacDialog";
import { DeviceRow } from "./DeviceRow";
import { InviteDialog } from "./InviteDialog";
import { MacCard } from "./MacCard";
import { MacList } from "./MacList";
import { UserNameDialog } from "./UserNameDialog";

/**
 * Settings → Devices: the Macs, and every browser and phone signed in to the hub, by user. `#/devices/connect` opens
 * "Connect a Mac", `#/devices/invite` "Sign in an iPhone" (a QR code).
 */
export function DevicesPage() {
  const role = useApp((s) => s.role);
  const route = useRoute();
  const [added, setAdded] = useState<Worker | null>(null);
  // the list loads again when the QR code signed in an iPhone
  const [signedIn, setSignedIn] = useState(0);
  const canInvite = useSession((s) => !!s.info?.canInvite);
  const refresh = useSession((s) => s.refresh);
  const hub = role === "hub";
  const connecting = hub && route.arg === "connect";
  const inviting = hub && canInvite && route.arg === "invite";
  const connect = () => navigate("devices", "connect");
  const invite = () => navigate("devices", "invite");
  const onInvited = useCallback(() => setSignedIn((n) => n + 1), []);

  // an owner may have allowed (or stopped) this device signing in others since it signed in
  useEffect(() => {
    if (hub) refresh();
  }, [hub, refresh]);

  return (
    <div className="mx-auto flex max-w-2xl flex-col gap-8 px-4 py-6 sm:px-6 sm:py-8">
      {hub ? (
        <Section
          title="Macs"
          description="The computers that run the models and keep your files. With several, each job goes to the one that fits it best."
          action={
            <Button size="sm" onClick={connect}>
              <Plus /> Connect a Mac
            </Button>
          }
        >
          <MacList added={added} onConnect={connect} />
          <ConnectMacDialog open={connecting} onOpenChange={(o) => !o && navigate("devices")} onCreated={setAdded} />
        </Section>
      ) : (
        <Section title="Mac" description="The computer that runs the models and keeps your files.">
          <MacCard />
        </Section>
      )}
      <Section
        title="Signed-in devices"
        description={hub ? "Browsers and phones that can use mlxlab, by who uses them. Revoke one you no longer use." : undefined}
        action={
          hub &&
          canInvite && (
            <Button size="sm" variant="outline" onClick={invite} data-invite-button="">
              <QrCode /> Sign in an iPhone
            </Button>
          )
        }
      >
        {hub && !canInvite && <NoInvite />}
        {hub ? <DeviceList reload={signedIn} /> : <NoHub />}
        {hub && canInvite && <InviteDialog open={inviting} onOpenChange={(o) => !o && navigate("devices")} onSignedIn={onInvited} />}
      </Section>
    </div>
  );
}

function Section({ title, description, action, children }: { title: string; description?: string; action?: ReactNode; children: ReactNode }) {
  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <h2 className="text-sm font-semibold">{title}</h2>
          {description && <p className="mt-0.5 text-sm text-muted-foreground">{description}</p>}
        </div>
        {action}
      </div>
      {children}
    </section>
  );
}

/** This device may not show a QR code: why, and who can change that. */
function NoInvite() {
  return (
    <p className="flex items-start gap-2 rounded-lg bg-muted/60 px-3 py-2 text-sm text-muted-foreground" data-no-invite="">
      <QrCode className="mt-0.5 size-4 shrink-0" />
      <span>
        To sign in an iPhone with a QR code, use a device signed in with the password — or ask its owner to turn on{" "}
        <span className="font-medium text-foreground">May sign in other iPhones</span> for this one.
      </span>
    </p>
  );
}

function DeviceList({ reload }: { reload: number }) {
  const signOut = useSession((s) => s.signOut);
  const manage = useSession((s) => !!s.info?.owner);
  const setMyUser = useSession((s) => s.setUser);
  const [devices, setDevices] = useState<Device[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [editing, setEditing] = useState<Device | null>(null);

  const load = useCallback(
    () =>
      api.devices().then(
        (list) => {
          setDevices(list);
          setError(null);
        },
        (e: unknown) => setError(e),
      ),
    [],
  );
  useEffect(() => {
    load();
  }, [load, reload]);

  const groups = useMemo(() => groupByUser(devices ?? []), [devices]);
  const names = useMemo(() => groups.map((g) => g.user).filter(Boolean), [groups]);

  /** Puts one device's new fields in the list (the answer of a PATCH does not say `current`). */
  const merge = (id: string, change: Partial<Device>) =>
    setDevices((list) => list?.map((x) => (x.id === id ? { ...x, ...change, current: x.current } : x)) ?? null);

  const revoke = async (d: Device) => {
    try {
      await api.revoke(d.id);
      setDevices((list) => list?.filter((x) => x.id !== d.id) ?? null);
      toast.success(`“${d.name}” is signed out`);
    } catch (e) {
      toast.error("Could not revoke the device", { description: errorText(e) });
    }
  };

  // the switch moves at once; back again when the hub said no
  const setCanInvite = async (d: Device, on: boolean) => {
    merge(d.id, { canInvite: on });
    try {
      merge(d.id, await api.patchDevice(d.id, { canInvite: on }));
    } catch (e) {
      merge(d.id, { canInvite: d.canInvite });
      toast.error(on ? `“${d.name}” may not sign in others yet` : `“${d.name}” may still sign in others`, { description: errorText(e) });
    }
  };

  const setUser = async (d: Device, user: string) => {
    const changed = await api.patchDevice(d.id, { user });
    merge(d.id, changed);
    if (d.current) {
      setMyUser(changed.user ?? user);
      local.set("user", changed.user ?? user);
    }
  };

  if (error && !devices) return <ErrorNotice error={error} onRetry={load} />;
  if (!devices) return <ListSkeleton />;

  return (
    <div className="flex flex-col gap-5" data-device-groups="">
      {groups.map((g) => (
        <div key={g.user || "(no name)"} className="flex flex-col gap-2" data-user-group={g.user}>
          <div className="flex items-center gap-2 px-1">
            <span
              className={cn("grid size-6 shrink-0 place-items-center rounded-full text-[11px] font-semibold", g.user ? "bg-primary/15 text-primary" : "bg-muted text-muted-foreground")}
              aria-hidden
            >
              {initials(g.user)}
            </span>
            <h3 className={cn("truncate text-sm font-medium", !g.user && "text-muted-foreground")}>{g.user || "(no name)"}</h3>
            {g.devices.some((d) => d.current) && <span className="text-xs text-primary">you</span>}
            <span className="ml-auto shrink-0 text-xs text-muted-foreground">
              {g.devices.length} {g.devices.length === 1 ? "device" : "devices"}
            </span>
          </div>
          {!g.user && (
            <p className="px-1 text-xs text-muted-foreground">Signed in before user names.{manage ? " Set who uses each one with its person button." : ""}</p>
          )}
          <Card className="gap-0 py-0">
            <ul className="divide-y">
              {g.devices.map((d) => (
                <DeviceRow
                  key={d.id}
                  device={d}
                  invitedBy={invitedByText(d, devices)}
                  manage={manage}
                  onRevoke={() => revoke(d)}
                  onSignOut={signOut}
                  onCanInvite={(on) => setCanInvite(d, on)}
                  onEditUser={() => setEditing(d)}
                />
              ))}
            </ul>
          </Card>
        </div>
      ))}
      <UserNameDialog device={editing} names={names} onClose={() => setEditing(null)} onSave={setUser} />
    </div>
  );
}

function ListSkeleton() {
  return (
    <Card className="gap-0 py-0">
      {[0, 1, 2].map((i) => (
        <div key={i} className="flex items-center gap-3 border-b px-4 py-3 last:border-0">
          <Skeleton className="size-9 rounded-lg" />
          <div className="flex-1 space-y-1.5">
            <Skeleton className="h-3.5 w-40" />
            <Skeleton className="h-3 w-28" />
          </div>
        </div>
      ))}
    </Card>
  );
}

/** Against `mlxlab serve`: nobody signs in here. */
function NoHub() {
  return (
    <Card className="flex-row items-start gap-3 px-4 text-muted-foreground">
      <KeyRound className="mt-0.5 size-4 shrink-0" />
      <p>
        Sign-in is managed by the hub. This page is served by <span className="font-medium text-foreground">mlxlab serve</span> on this Mac, which only
        listens on this computer and needs no password. Open mlxlab through the hub to see and revoke signed-in devices.
      </p>
    </Card>
  );
}
