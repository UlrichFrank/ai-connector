import { useState } from "react";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { parentFolder, leaf } from "./songs";
import { renameFolder, useStudio } from "./state";

/** Asks for a name (no native prompt). */
export function NameDialog({
  open,
  onOpenChange,
  title,
  description,
  initial = "",
  ok,
  onSubmit,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  title: string;
  description?: string;
  initial?: string;
  ok: string;
  onSubmit: (name: string) => void | Promise<void>;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        {/* the content mounts when the dialog opens: the field starts from `initial` each time */}
        <NameForm title={title} description={description} initial={initial} ok={ok} onSubmit={onSubmit} close={() => onOpenChange(false)} />
      </DialogContent>
    </Dialog>
  );
}

function NameForm({ title, description, initial, ok, onSubmit, close }: { title: string; description?: string; initial: string; ok: string; onSubmit: (name: string) => void | Promise<void>; close: () => void }) {
  const [value, setValue] = useState(initial);
  const name = value.trim().replace(/\//g, "-");
  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={async (e) => {
        e.preventDefault();
        if (!name) return;
        await onSubmit(name);
        close();
      }}
    >
      <DialogHeader>
        <DialogTitle>{title}</DialogTitle>
        {description && <DialogDescription>{description}</DialogDescription>}
      </DialogHeader>
      <Input autoFocus value={value} placeholder="Name" onChange={(e) => setValue(e.target.value)} onFocus={(e) => e.target.select()} />
      <DialogFooter>
        <Button type="button" variant="ghost" onClick={close}>
          Cancel
        </Button>
        <Button type="submit" disabled={!name || name === initial}>
          {ok}
        </Button>
      </DialogFooter>
    </form>
  );
}

/** A new folder inside `into`; `then` gets its path. */
export function NewFolderDialog({ open, onOpenChange, into, then }: { open: boolean; onOpenChange: (o: boolean) => void; into: string; then?: (f: string) => void }) {
  const addFolder = useStudio((s) => s.addFolder);
  return (
    <NameDialog
      open={open}
      onOpenChange={onOpenChange}
      title="New folder"
      description={into ? `Inside “${into}”.` : "Drag songs onto it, or make new ones while it is open."}
      ok="Create"
      onSubmit={(name) => {
        const f = into ? `${into}/${name}` : name;
        addFolder(f);
        then?.(f);
      }}
    />
  );
}

export function RenameFolderDialog({ folder, open, onOpenChange }: { folder: string; open: boolean; onOpenChange: (o: boolean) => void }) {
  const setFolder = useStudio((s) => s.setFolder);
  return (
    <NameDialog
      open={open}
      onOpenChange={onOpenChange}
      title="Rename folder"
      description="Its songs and folders come along."
      initial={leaf(folder)}
      ok="Rename"
      onSubmit={async (name) => {
        const p = parentFolder(folder);
        const to = p ? `${p}/${name}` : name;
        const cur = useStudio.getState().folder;
        if (await renameFolder(folder, to)) if (cur === folder || cur.startsWith(folder + "/")) setFolder(to + cur.slice(folder.length));
      }}
    />
  );
}

export function RemoveFolderDialog({ folder, open, onOpenChange }: { folder: string; open: boolean; onOpenChange: (o: boolean) => void }) {
  const setFolder = useStudio((s) => s.setFolder);
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Remove the folder “{leaf(folder)}”?</AlertDialogTitle>
          <AlertDialogDescription>Its songs move up one level. Nothing is deleted.</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={async () => {
              const cur = useStudio.getState().folder;
              if (await renameFolder(folder, "")) if (cur === folder || cur.startsWith(folder + "/")) setFolder(parentFolder(folder));
            }}
          >
            Remove
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
