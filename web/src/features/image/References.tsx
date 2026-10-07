import { ImagePlus, Loader2, X } from "lucide-react";
import { forwardRef, useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import { cn } from "@/lib/utils";
import { maxRefs } from "./form";

const images = (files: Iterable<File>) => [...files].filter((f) => f.type.startsWith("image/"));

/** Uploads pictures and hands each url to `onAdd`; tells how many are on their way. */
function useUpload(count: number, onAdd: (url: string) => void) {
  const [pending, setPending] = useState(0);
  const add = async (files: File[]) => {
    const room = maxRefs - count - pending;
    if (!files.length) return;
    if (room <= 0) return void toast.info(`Up to ${maxRefs} reference images`);
    const take = files.slice(0, room);
    setPending((n) => n + take.length);
    await Promise.all(
      take.map(async (f) => {
        try {
          const { url } = await api.upload(f);
          onAdd(url);
        } catch (e) {
          toast.error(`Could not upload ${f.name || "the image"}`, { description: errorText(e) });
        } finally {
          setPending((n) => n - 1);
        }
      }),
    );
  };
  return { pending, add };
}

/**
 * Reference images: drop, paste (anywhere on the page) or pick; each shows as a thumbnail with ×. With references,
 * the model edits them following the prompt.
 */
export const References = forwardRef<HTMLDivElement, { refs: string[]; onChange: (refs: string[]) => void; onAdd: (url: string) => void }>(function References(
  { refs, onChange, onAdd },
  ref,
) {
  const { pending, add } = useUpload(refs.length, onAdd);
  const [over, setOver] = useState(false);
  const input = useRef<HTMLInputElement>(null);

  // paste a picture anywhere on the page
  useEffect(() => {
    const paste = (e: ClipboardEvent) => {
      const files = images(e.clipboardData?.files ?? []);
      if (!files.length) return;
      e.preventDefault();
      void add(files);
    };
    window.addEventListener("paste", paste);
    return () => window.removeEventListener("paste", paste);
  });

  const full = refs.length + pending >= maxRefs;
  const dropProps = {
    onDragOver: (e: React.DragEvent) => (e.preventDefault(), setOver(true)),
    onDragLeave: () => setOver(false),
    onDrop: (e: React.DragEvent) => {
      e.preventDefault();
      setOver(false);
      void add(images(e.dataTransfer.files));
    },
  };

  return (
    <div ref={ref} className="flex flex-col gap-2" {...dropProps}>
      <input ref={input} type="file" accept="image/*" multiple hidden onChange={(e) => (void add(images(e.target.files ?? [])), (e.target.value = ""))} />
      {refs.length + pending === 0 ? (
        <button
          type="button"
          onClick={() => input.current?.click()}
          className={cn(
            "flex items-center gap-3 rounded-lg border border-dashed px-3 py-3 text-left text-sm text-muted-foreground transition-colors hover:border-primary/50 hover:bg-primary/5 hover:text-foreground",
            over && "border-primary bg-primary/10 text-foreground",
          )}
        >
          <ImagePlus className="size-5 shrink-0" />
          <span className="min-w-0">
            <span className="block font-medium text-foreground">Edit a picture</span>
            <span className="block text-xs">Drop, paste or choose up to {maxRefs} images</span>
          </span>
        </button>
      ) : (
        <div className={cn("flex flex-wrap gap-2 rounded-lg p-0.5", over && "bg-primary/10 ring-2 ring-primary")}>
          {refs.map((r) => (
            <div key={r} className="group/ref relative size-16 overflow-hidden rounded-md ring-1 ring-foreground/10">
              <img src={r} alt="Reference" className="size-full object-cover" />
              <button
                type="button"
                title="Remove"
                onClick={() => onChange(refs.filter((x) => x !== r))}
                className="absolute top-0.5 right-0.5 grid size-5 place-items-center rounded-full bg-black/60 text-white opacity-90 transition hover:bg-black/80 sm:opacity-0 sm:group-hover/ref:opacity-100"
              >
                <X className="size-3" />
              </button>
            </div>
          ))}
          {Array.from({ length: pending }, (_, i) => (
            <div key={`p${i}`} className="grid size-16 place-items-center rounded-md bg-muted">
              <Loader2 className="size-4 animate-spin text-muted-foreground" />
            </div>
          ))}
          {!full && (
            <button
              type="button"
              title="Add a reference image"
              onClick={() => input.current?.click()}
              className="grid size-16 place-items-center rounded-md border border-dashed text-muted-foreground transition-colors hover:border-primary/50 hover:text-foreground"
            >
              <ImagePlus className="size-5" />
            </button>
          )}
        </div>
      )}
    </div>
  );
});
