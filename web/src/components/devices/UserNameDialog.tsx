import { Loader2 } from "lucide-react";
import { useState, type FormEvent } from "react";
import type { Device } from "@/api/types";
import { errorText } from "@/api/client";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

/** Owner viewers: who uses a device ("Philipp"). The names already in the list are offered. */
export function UserNameDialog({
  device,
  names,
  onClose,
  onSave,
}: {
  device: Device | null;
  names: string[];
  onClose: () => void;
  /** Throws when the hub refused it. */
  onSave: (d: Device, user: string) => Promise<void>;
}) {
  return (
    <Dialog open={!!device} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-sm">
        {device && <NameForm key={device.id} device={device} names={names} onClose={onClose} onSave={onSave} />}
      </DialogContent>
    </Dialog>
  );
}

function NameForm({
  device,
  names,
  onClose,
  onSave,
}: {
  device: Device;
  names: string[];
  onClose: () => void;
  onSave: (d: Device, user: string) => Promise<void>;
}) {
  const [user, setUser] = useState(device.user ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!user.trim() || busy) return;
    setBusy(true);
    setError("");
    try {
      await onSave(device, user.trim());
      onClose();
    } catch (err) {
      setError(errorText(err));
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="contents">
      <DialogHeader>
        <DialogTitle>Who uses “{device.name}”?</DialogTitle>
        <DialogDescription>The Devices page groups the devices by this name.</DialogDescription>
      </DialogHeader>
      <div className="flex flex-col gap-2">
        <Label htmlFor="device-user">User name</Label>
        <Input
          id="device-user"
          list="device-users"
          value={user}
          onChange={(e) => setUser(e.target.value)}
          onFocus={(e) => e.target.select()}
          autoFocus
          maxLength={40}
          placeholder="e.g. Anna"
          aria-invalid={!!error || undefined}
        />
        <datalist id="device-users">
          {names.map((n) => (
            <option key={n} value={n} />
          ))}
        </datalist>
        {error && <p className="text-sm text-destructive">{error}</p>}
      </div>
      <DialogFooter>
        <Button type="button" variant="outline" onClick={onClose}>
          Cancel
        </Button>
        <Button type="submit" disabled={busy || !user.trim()}>
          {busy && <Loader2 className="animate-spin" />} Save
        </Button>
      </DialogFooter>
    </form>
  );
}
