import { KeyRound, LogOut, Monitor, QrCode, Smartphone, Tablet, UserPen } from "lucide-react";
import type { Device } from "@/api/types";
import { Ago } from "@/components/connection/Ago";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";

/** A phone, a tablet or a screen, guessed from the name ("Safari on iPhone", "Philipp's iPad"). */
function DeviceIcon({ name }: { name: string }) {
  if (/iphone|android|phone/i.test(name)) return <Smartphone className="size-4" />;
  if (/ipad|tablet/i.test(name)) return <Tablet className="size-4" />;
  return <Monitor className="size-4" />;
}

/**
 * One signed-in device: how it signed in (the password: an owner; or a QR code: "invited by …"), whether it may sign in
 * other iPhones (a switch for owner viewers), and Sign out (this one) or Revoke. Owner viewers can change its user.
 */
export function DeviceRow({
  device: d,
  invitedBy,
  manage,
  onRevoke,
  onSignOut,
  onCanInvite,
  onEditUser,
}: {
  device: Device;
  /** "invited by Safari on macOS" ("" for an owner). */
  invitedBy: string;
  /** The viewer is an owner device: it may change devices. */
  manage: boolean;
  onRevoke: () => void;
  onSignOut: () => void;
  onCanInvite: (on: boolean) => void;
  onEditUser: () => void;
}) {
  const switchId = `may-invite-${d.id}`;
  return (
    <li className="flex flex-col gap-2 px-4 py-3" data-device-row={d.id}>
      <div className="flex items-center gap-3">
        <div className="grid size-9 shrink-0 place-items-center rounded-lg bg-muted text-muted-foreground">
          <DeviceIcon name={d.name} />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-center gap-2">
            <span className="truncate text-sm font-medium">{d.name || "Unnamed device"}</span>
            {d.current && (
              <Badge variant="secondary" className="bg-primary/10 text-primary max-sm:hidden">
                This device
              </Badge>
            )}
          </div>
          <div className="mt-0.5 truncate text-xs text-muted-foreground">
            {d.current ? (
              <>
                <span className="font-medium text-primary sm:hidden">This device · </span>Active now
              </>
            ) : (
              <>
                Last seen <Ago iso={d.lastSeen} />
              </>
            )}
            <span className="max-sm:hidden">
              {" "}
              · signed in <Ago iso={d.created} />
            </span>
          </div>
        </div>
        {manage && (
          <Tooltip>
            <TooltipTrigger asChild>
              <Button variant="ghost" size="icon-sm" className="text-muted-foreground" onClick={onEditUser} aria-label={`Change the user of “${d.name}”`}>
                <UserPen />
              </Button>
            </TooltipTrigger>
            <TooltipContent>Change the user</TooltipContent>
          </Tooltip>
        )}
        {d.current ? (
          <Button variant="outline" size="sm" onClick={onSignOut}>
            <LogOut /> Sign out
          </Button>
        ) : (
          <RevokeButton device={d} onRevoke={onRevoke} />
        )}
      </div>

      {/* how it signed in, and whether it may sign in others: under the name, aligned with it */}
      <div className="flex min-h-5 items-center gap-2 pl-12 text-xs text-muted-foreground">
        <span className="flex min-w-0 flex-1 items-center gap-1.5" data-device-origin={d.owner ? "owner" : "invited"}>
          {d.owner ? (
            <>
              <KeyRound className="size-3.5 shrink-0" />
              <span className="truncate">
                Owner<span className="max-sm:hidden"> · signed in with the password</span>
              </span>
            </>
          ) : (
            <>
              <QrCode className="size-3.5 shrink-0" />
              <span className="truncate first-letter:uppercase">{invitedBy || "invited with a QR code"}</span>
            </>
          )}
        </span>
        <MayInvite device={d} manage={manage} switchId={switchId} onChange={onCanInvite} />
      </div>
    </li>
  );
}

/** "May sign in other iPhones": a switch for owner viewers on an invited device; "always" for an owner. */
function MayInvite({ device: d, manage, switchId, onChange }: { device: Device; manage: boolean; switchId: string; onChange: (on: boolean) => void }) {
  const label = (
    <>
      <span className="max-sm:hidden">May sign in other iPhones</span>
      <span className="sm:hidden">Can invite</span>
    </>
  );
  if (d.owner)
    return (
      <span className="shrink-0" data-may-invite="always">
        {label}: <span className="font-medium text-foreground">always</span>
      </span>
    );
  if (!manage)
    return (
      <span className="shrink-0" data-may-invite={d.canInvite ? "on" : "off"}>
        {label}: <span className="font-medium text-foreground">{d.canInvite ? "yes" : "no"}</span>
      </span>
    );
  return (
    <label htmlFor={switchId} className="flex shrink-0 cursor-pointer items-center gap-2" data-may-invite={d.canInvite ? "on" : "off"}>
      {label}
      <Switch id={switchId} size="sm" checked={!!d.canInvite} onCheckedChange={onChange} />
    </label>
  );
}

function RevokeButton({ device: d, onRevoke }: { device: Device; onRevoke: () => void }) {
  return (
    <AlertDialog>
      <AlertDialogTrigger asChild>
        <Button variant="ghost" size="sm" className="text-muted-foreground hover:text-destructive">
          Revoke
        </Button>
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Revoke “{d.name}”?</AlertDialogTitle>
          <AlertDialogDescription>
            It is signed out at once and needs {d.owner ? "the password" : "the password or a new QR code"} to sign in again. Devices it signed in stay signed in.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction variant="destructive" onClick={onRevoke}>
            Revoke
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
