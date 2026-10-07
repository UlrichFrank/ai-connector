import { Check, Copy, Loader2, RotateCw, Star, Trash2, TriangleAlert } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Job } from "@/api/types";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Progress } from "@/components/ui/progress";
import { duration, noun, progress, took } from "@/lib/format";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";

/** "Create" or "Create 2 versions", with what happens in a line below. */
export function CreateButton({ count, busy, disabled, onClick, label = "Create" }: { count: number; busy?: boolean; disabled?: boolean; onClick: () => void; label?: string }) {
  return (
    <div className="flex flex-col gap-1.5">
      <Button size="lg" className="h-10 w-full text-sm" disabled={disabled || busy || count === 0} onClick={onClick}>
        {busy && <Loader2 className="animate-spin" />}
        {count > 1 ? `${label} ${count} versions` : label}
      </Button>
      {count > 1 && <p className="text-center text-xs text-muted-foreground">One with each ticked model, one after the other.</p>}
    </div>
  );
}

/** Where a running job is: its status, a progress bar, and how long it has taken. */
export function JobProgress({ job: j, className }: { job: Job; className?: string }) {
  const p = progress(j);
  return (
    <div className={cn("flex flex-col gap-1.5", className)}>
      <div className="flex items-center gap-2 text-xs text-muted-foreground">
        <Loader2 className="size-3.5 animate-spin text-primary" />
        <span className="truncate">{j.state === "queued" ? "waiting for the GPU" : (j.status ?? "starting …")}</span>
        <span className="ml-auto tabular-nums">{j.state === "running" ? duration(took(j)) : ""}</span>
      </div>
      <Progress value={p != null ? p * 100 : undefined} className="h-1" />
    </div>
  );
}

/** A failed job: what went wrong in words, what to do, Retry and Copy. */
export function JobError({ job: j, compact }: { job: Job; compact?: boolean }) {
  const [copied, setCopied] = useState(false);
  const put = useApp((s) => s.put);
  return (
    <div className="flex flex-col gap-2 rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm">
      <div className="flex items-start gap-2">
        <TriangleAlert className="mt-0.5 size-4 shrink-0 text-destructive" />
        <div className="min-w-0">
          <div className="font-medium">{j.state === "canceled" ? "Canceled" : (j.problem?.title ?? "Failed")}</div>
          {!compact && j.problem?.hint && <div className="mt-0.5 text-muted-foreground">{j.problem.hint}</div>}
        </div>
      </div>
      <div className="flex gap-1.5">
        <Button
          size="sm"
          variant="outline"
          onClick={async () => {
            try {
              put(await api.retry(j.id));
            } catch (e) {
              toast.error("Retry failed", { description: errorText(e) });
            }
          }}
        >
          <RotateCw /> Retry
        </Button>
        {j.error && (
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              navigator.clipboard.writeText(j.error!);
              setCopied(true);
              setTimeout(() => setCopied(false), 1500);
            }}
          >
            {copied ? <Check /> : <Copy />} {copied ? "Copied" : "Copy error"}
          </Button>
        )}
      </div>
    </div>
  );
}

/** ☆ / ★. Acts on press, so a list that redraws under the pointer cannot swallow it. */
export function StarButton({ job: j, className }: { job: Job; className?: string }) {
  const put = useApp((s) => s.put);
  const toggle = async () => {
    put({ ...j, starred: !j.starred }); // at once; the server's answer follows
    try {
      put(await api.star(j.id, !j.starred));
    } catch (e) {
      put(j);
      toast.error("Could not star", { description: errorText(e) });
    }
  };
  return (
    <Button
      variant="ghost"
      size="icon-sm"
      title={j.starred ? "Unstar" : "Star"}
      className={cn(j.starred ? "text-amber-400 hover:text-amber-300" : "text-muted-foreground", className)}
      onPointerDown={(e) => (e.preventDefault(), e.stopPropagation(), toggle())}
      onClick={(e) => e.stopPropagation()}
    >
      <Star className={cn(j.starred && "fill-current")} />
    </Button>
  );
}

/** Delete a job and its files, after asking. `trigger` renders the button; `after` runs once it is gone. */
export function DeleteJob({ job: j, after, trigger }: { job: Job; after?: () => void; trigger?: (open: () => void) => React.ReactNode }) {
  const [open, setOpen] = useState(false);
  const drop = useApp((s) => s.drop);
  const what = noun(j);
  return (
    <>
      {trigger ? (
        trigger(() => setOpen(true))
      ) : (
        <Button variant="ghost" size="sm" className="text-muted-foreground hover:text-destructive" onClick={() => setOpen(true)}>
          <Trash2 /> Delete
        </Button>
      )}
      <AlertDialog open={open} onOpenChange={setOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Delete this {what}
              {j.files?.length ? " and its file" : ""}?
            </AlertDialogTitle>
            <AlertDialogDescription>{j.starred ? "It is starred. " : ""}This cannot be undone.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              onClick={async () => {
                try {
                  await api.remove(j.id);
                  drop(j.id);
                  after?.();
                } catch (e) {
                  toast.error("Delete failed", { description: errorText(e) });
                }
              }}
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
