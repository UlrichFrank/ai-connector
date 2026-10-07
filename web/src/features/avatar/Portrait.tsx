import { ImageIcon, ImagePlus, Images, Loader2, X } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import { mainFile } from "@/components/media/media";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";

const isImage = (f: File) => f.type.startsWith("image/");

/** The size of a picture, once loaded. */
function measure(url: string): Promise<{ w: number; h: number }> {
  return new Promise((resolve) => {
    const i = new Image();
    i.onload = () => resolve({ w: i.naturalWidth, h: i.naturalHeight });
    i.onerror = () => resolve({ w: 0, h: 0 });
    i.src = url;
  });
}

/**
 * The face that speaks: drop, paste (anywhere on the page) or choose a picture, or pick one made on the Image page.
 * A clear, front-facing portrait works best.
 */
export function Portrait({ url, onChange }: { url: string; onChange: (url: string, w: number, h: number) => void }) {
  const [busy, setBusy] = useState(false);
  const [over, setOver] = useState(false);
  const input = useRef<HTMLInputElement>(null);

  const set = async (u: string) => {
    const { w, h } = await measure(u);
    onChange(u, w, h);
  };

  const add = async (files: Iterable<File>) => {
    const f = [...files].find(isImage);
    if (!f) return;
    setBusy(true);
    try {
      const { url: u } = await api.upload(f);
      await set(u);
    } catch (e) {
      toast.error("Could not upload the picture", { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };

  useEffect(() => {
    const paste = (e: ClipboardEvent) => {
      const files = [...(e.clipboardData?.files ?? [])].filter(isImage);
      if (!files.length) return;
      e.preventDefault();
      void add(files);
    };
    window.addEventListener("paste", paste);
    return () => window.removeEventListener("paste", paste);
  });

  const drop = {
    onDragOver: (e: React.DragEvent) => (e.preventDefault(), setOver(true)),
    onDragLeave: () => setOver(false),
    onDrop: (e: React.DragEvent) => (e.preventDefault(), setOver(false), void add(e.dataTransfer.files)),
  };

  return (
    <div className="flex flex-col gap-2" {...drop}>
      <input ref={input} type="file" accept="image/*" hidden onChange={(e) => (void add(e.target.files ?? []), (e.target.value = ""))} />
      {url ? (
        <div className={cn("group/portrait relative flex justify-center overflow-hidden rounded-lg bg-muted/60 ring-1 ring-foreground/10", over && "ring-2 ring-primary")}>
          <img src={url} alt="Portrait" className="max-h-56 w-auto object-contain" />
          {busy && (
            <div className="absolute inset-0 grid place-items-center bg-background/60">
              <Loader2 className="size-5 animate-spin" />
            </div>
          )}
          <div className="absolute right-1.5 bottom-1.5 flex gap-1">
            <Button size="xs" variant="secondary" className="bg-background/80 backdrop-blur" onClick={() => input.current?.click()}>
              <ImagePlus /> Replace
            </Button>
            <PickImage onPick={(u) => void set(u)} small />
          </div>
          <button
            type="button"
            title="Remove"
            onClick={() => onChange("", 0, 0)}
            className="absolute top-1.5 right-1.5 grid size-6 place-items-center rounded-full bg-black/60 text-white hover:bg-black/80"
          >
            <X className="size-3.5" />
          </button>
        </div>
      ) : (
        <div className="grid grid-cols-[1fr_auto] gap-2">
          <button
            type="button"
            onClick={() => input.current?.click()}
            className={cn(
              "flex items-center gap-3 rounded-lg border border-dashed px-3 py-4 text-left text-sm text-muted-foreground transition-colors hover:border-primary/50 hover:bg-primary/5 hover:text-foreground",
              over && "border-primary bg-primary/10 text-foreground",
            )}
          >
            {busy ? <Loader2 className="size-5 shrink-0 animate-spin" /> : <ImageIcon className="size-5 shrink-0" />}
            <span className="min-w-0">
              <span className="block font-medium text-foreground">Add a portrait</span>
              <span className="block text-xs">Drop, paste or choose — a face, looking at the camera</span>
            </span>
          </button>
          <PickImage onPick={(u) => void set(u)} />
        </div>
      )}
    </div>
  );
}

/** A picture made on the Image page. */
function PickImage({ onPick, small }: { onPick: (url: string) => void; small?: boolean }) {
  const jobs = useApp((s) => s.jobs);
  const [open, setOpen] = useState(false);
  const images = useMemo(
    () =>
      Object.values(jobs)
        .filter((j) => j.kind === "image" && j.state === "done" && !j.request.coverFor && mainFile(j))
        .sort((a, b) => Date.parse(b.created) - Date.parse(a.created))
        .slice(0, 60),
    [jobs],
  );
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        {small ? (
          <Button size="xs" variant="secondary" className="bg-background/80 backdrop-blur">
            <Images /> Your images
          </Button>
        ) : (
          <Button variant="outline" className="h-auto flex-col gap-1 px-3 py-2 text-xs" title="Pick one of your images">
            <Images className="size-5" /> Your images
          </Button>
        )}
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80 p-2">
        {images.length ? (
          <div className="grid max-h-80 grid-cols-4 gap-1.5 overflow-y-auto">
            {images.map((j) => (
              <button key={j.id} type="button" title={j.request.prompt} onClick={() => (onPick(mainFile(j)!), setOpen(false))} className="aspect-square overflow-hidden rounded-md ring-1 ring-foreground/10 hover:ring-2 hover:ring-primary">
                <img src={mainFile(j)} alt="" loading="lazy" className="size-full object-cover" />
              </button>
            ))}
          </div>
        ) : (
          <p className="p-2 text-sm text-muted-foreground">
            No images yet. Make a portrait on the{" "}
            <a href="#/image" className="text-primary hover:underline">
              Image
            </a>{" "}
            page.
          </p>
        )}
      </PopoverContent>
    </Popover>
  );
}
