import { ImagePlus, Loader2, RefreshCw, X } from "lucide-react";
import { useEffect, useEffectEvent, useRef, useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

/**
 * An optional first frame: drop, paste or pick an image; it is uploaded at once and shown as a thumbnail. Pasting
 * an image anywhere on the page lands here too (text pastes are left alone).
 */
export function FirstFrame({ value, onChange, compact }: { value: string; onChange: (url: string) => void; compact?: boolean }) {
  const [busy, setBusy] = useState(false);
  const [over, setOver] = useState(false);
  const input = useRef<HTMLInputElement>(null);

  const take = async (file: File | undefined | null) => {
    if (!file) return;
    if (!file.type.startsWith("image/")) return toast.error("That is not an image", { description: file.name });
    setBusy(true);
    try {
      onChange((await api.upload(file)).url);
    } catch (e) {
      toast.error("Upload failed", { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };
  const onPaste = useEffectEvent((f: File) => take(f));

  useEffect(() => {
    const paste = (e: ClipboardEvent) => {
      const f = [...(e.clipboardData?.files ?? [])].find((f) => f.type.startsWith("image/"));
      if (!f) return;
      e.preventDefault();
      onPaste(f);
    };
    window.addEventListener("paste", paste);
    return () => window.removeEventListener("paste", paste);
  }, []);

  const drop = {
    onDragOver: (e: React.DragEvent) => {
      if (![...e.dataTransfer.types].includes("Files")) return;
      e.preventDefault();
      setOver(true);
    },
    onDragLeave: () => setOver(false),
    onDrop: (e: React.DragEvent) => {
      if (![...e.dataTransfer.types].includes("Files")) return;
      e.preventDefault();
      setOver(false);
      take(e.dataTransfer.files[0]);
    },
  };

  const picker = <input ref={input} type="file" accept="image/*" hidden onChange={(e) => (take(e.target.files?.[0]), (e.target.value = ""))} />;

  if (value && compact)
    return (
      <div className={cn("flex h-9 w-fit max-w-full items-center gap-1 rounded-md border bg-muted/30 py-1 pr-0.5 pl-1", over && "border-primary bg-primary/10")} {...drop}>
        <img src={value} alt="First frame" title="The movie starts from this image" className="h-7 w-11 shrink-0 rounded object-cover" />
        <Button variant="ghost" size="icon-xs" title="Replace the image" onClick={() => input.current?.click()} disabled={busy}>
          {busy ? <Loader2 className="animate-spin" /> : <RefreshCw />}
        </Button>
        <Button variant="ghost" size="icon-xs" title="Remove the image" className="text-muted-foreground" onClick={() => onChange("")}>
          <X />
        </Button>
        {picker}
      </div>
    );

  if (value)
    return (
      <div className={cn("flex items-center gap-3 rounded-lg border bg-muted/30 p-2", over && "border-primary bg-primary/10")} {...drop}>
        <img src={value} alt="First frame" className={cn("shrink-0 rounded-md object-cover", compact ? "h-10 w-16" : "h-16 w-24")} />
        <div className="min-w-0 flex-1 text-xs text-muted-foreground">
          <div className="font-medium text-foreground">Starts from this image</div>
          The video animates it; describe what happens next.
        </div>
        <div className="flex shrink-0 flex-col gap-0.5">
          <Button variant="ghost" size="xs" onClick={() => input.current?.click()} disabled={busy}>
            {busy ? <Loader2 className="animate-spin" /> : null} Replace
          </Button>
          <Button variant="ghost" size="xs" className="text-muted-foreground" onClick={() => onChange("")}>
            <X /> Remove
          </Button>
        </div>
        {picker}
      </div>
    );

  if (compact)
    return (
      <>
        <button
          type="button"
          onClick={() => input.current?.click()}
          disabled={busy}
          title="Start the movie from an image: drop, paste or pick one"
          {...drop}
          className={cn(
            "flex h-7 max-w-full items-center gap-1.5 rounded-md border border-dashed px-2 text-xs text-muted-foreground transition-colors hover:border-primary/50 hover:text-foreground",
            over && "border-primary bg-primary/10",
          )}
        >
          {busy ? <Loader2 className="size-3.5 animate-spin" /> : <ImagePlus className="size-3.5" />}
          <span className="truncate">{busy ? "Uploading …" : "First frame"}</span>
        </button>
        {picker}
      </>
    );

  return (
    <>
    <button
      type="button"
      onClick={() => input.current?.click()}
      disabled={busy}
      {...drop}
      className={cn(
        "flex w-full items-center gap-3 rounded-lg border border-dashed text-left text-xs text-muted-foreground transition-colors hover:border-primary/50 hover:bg-muted/40",
        "px-3 py-3",
        over && "border-primary bg-primary/10",
      )}
    >
      <span className="grid size-8 shrink-0 place-items-center rounded-md bg-muted text-foreground">
        {busy ? <Loader2 className="size-4 animate-spin" /> : <ImagePlus className="size-4" />}
      </span>
      <span className="min-w-0">
        <span className="block font-medium text-foreground">{busy ? "Uploading …" : "Start from an image"}</span>
        Optional · drop, paste or pick one
      </span>
    </button>
    {picker}
    </>
  );
}
