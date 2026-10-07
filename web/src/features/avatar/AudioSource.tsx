import { AudioLines, ListMusic, Loader2, X } from "lucide-react";
import { useMemo, useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import { mainFile } from "@/components/media/media";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { ago } from "@/lib/format";
import { useApp } from "@/store/app";
import { isSpoken, PreviewButton, Recorder } from "@/features/voice";

/** The speech for the avatar as audio: recorded, uploaded, or one made on the Voice page. */
export function AudioSource({ url, name, onChange }: { url: string; name: string; onChange: (url: string, name: string) => void }) {
  const [busy, setBusy] = useState(false);

  const upload = async (f: File) => {
    setBusy(true);
    try {
      const { url: u } = await api.upload(f);
      onChange(u, f.name);
    } catch (e) {
      toast.error("Could not upload the audio", { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };

  if (busy)
    return (
      <div className="flex items-center gap-2 rounded-lg border px-3 py-3 text-sm text-muted-foreground">
        <Loader2 className="size-4 animate-spin" /> Uploading …
      </div>
    );

  if (url)
    return (
      <div className="flex flex-col gap-1.5 rounded-lg border p-2.5">
        <div className="flex items-center gap-2 text-sm">
          <AudioLines className="size-4 shrink-0 text-muted-foreground" />
          <span className="min-w-0 flex-1 truncate">{name || url.split("/").pop()}</span>
          <Button size="icon-xs" variant="ghost" title="Remove" onClick={() => onChange("", "")}>
            <X />
          </Button>
        </div>
        <audio src={url} controls className="h-9 w-full" />
      </div>
    );

  return (
    <div className="flex flex-col gap-2">
      <Recorder onFile={(f) => void upload(f)} min={1} ideal={20} max={60} />
      <PickSpeech onPick={(u, n) => onChange(u, n)} />
    </div>
  );
}

function PickSpeech({ onPick }: { onPick: (url: string, name: string) => void }) {
  const jobs = useApp((s) => s.jobs);
  const [open, setOpen] = useState(false);
  const list = useMemo(
    () =>
      Object.values(jobs)
        .filter((j) => isSpoken(j) && j.state === "done" && mainFile(j))
        .sort((a, b) => Date.parse(b.created) - Date.parse(a.created))
        .slice(0, 40),
    [jobs],
  );
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button variant="outline" className="w-full">
          <ListMusic /> Use speech from the Voice page
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-(--radix-popover-trigger-width) min-w-72 p-1">
        {list.length ? (
          <div className="max-h-80 overflow-y-auto">
            {list.map((j) => (
              <div key={j.id} className="flex items-center gap-1 rounded-md hover:bg-accent">
                <PreviewButton url={mainFile(j)!} />
                <button type="button" className="min-w-0 flex-1 py-1.5 pr-2 text-left" onClick={() => (onPick(mainFile(j)!, (j.request.text ?? "").slice(0, 60) || "Speech"), setOpen(false))}>
                  <span className="line-clamp-1 text-sm">{j.request.text}</span>
                  <span className="text-xs text-muted-foreground">{ago(j.created)}</span>
                </button>
              </div>
            ))}
          </div>
        ) : (
          <p className="p-2 text-sm text-muted-foreground">
            Nothing yet. Make speech on the{" "}
            <a href="#/voice" className="text-primary hover:underline">
              Voice
            </a>{" "}
            page.
          </p>
        )}
      </PopoverContent>
    </Popover>
  );
}
