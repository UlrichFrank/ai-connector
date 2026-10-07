import { ImageIcon, ImagePlus, Loader2, Sparkles, Trash2 } from "lucide-react";
import { useRef, useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Job } from "@/api/types";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { progress } from "@/lib/format";
import { helperModel, runChat } from "@/lib/jobs";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";
import { SongArt } from "./Art";
import { songTitle } from "./songs";
import { setCover, useCoverJob } from "./state";

/** The image model for covers: the one picked on the Image page, else one on disk. */
function imageModel(): string | undefined {
  const models = useApp.getState().models.filter((m) => m.kind === "image");
  const picked = local.get<string[]>("models.image", []);
  return models.find((m) => picked.includes(m.id))?.id ?? models.find((m) => m.here)?.id ?? models[0]?.id;
}

/**
 * An AI cover: a chat model turns the song into an image prompt (the "cover-art" prompt), an image model paints it;
 * the server makes the picture the song's cover when it is done.
 */
async function generateCover(j: Job) {
  const p = useApp.getState().prompts.find((x) => x.use === "cover-art");
  const chat = helperModel();
  const img = imageModel();
  if (!p || !chat || !img) throw new Error("Needs a chat model, an image model and the “Album cover” prompt.");
  const r = j.request;
  const about = [`Title: ${songTitle(j)}`, r.prompt && `Style: ${r.prompt}`, r.lyrics && `Lyrics:\n${r.lyrics}`].filter(Boolean).join("\n");
  const text = await runChat(chat, p.text, about, { temperature: 0.8, maxTokens: 400 });
  const c = await api.submit({ model: img, prompt: text, width: 1024, height: 1024, coverFor: j.id });
  useApp.getState().put(c);
}

export function Cover({ job: j, base, className }: { job: Job; base?: Job; className?: string }) {
  const cj = useCoverJob(j.id);
  const [writing, setWriting] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const p = cj ? progress(cj) : undefined;
  const making = writing || !!cj;

  const ai = async () => {
    setWriting(true);
    try {
      await generateCover(j);
    } catch (e) {
      toast.error("Cover", { description: errorText(e) });
    } finally {
      setWriting(false);
    }
  };

  return (
    <div className={cn("group relative aspect-square", className)}>
      <SongArt job={j} base={base} className="size-full rounded-xl shadow-xl shadow-black/30 ring-1 ring-white/10" />
      {making && (
        <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 rounded-xl bg-black/55 px-4 text-center text-xs text-white backdrop-blur-[2px]">
          <Loader2 className="size-5 animate-spin" />
          <span>{writing ? "Writing the picture …" : cj!.state === "queued" ? "Cover waits for the GPU" : `Painting the cover${p != null ? ` · ${Math.round(p * 100)}%` : " …"}`}</span>
          {p != null && (
            <div className="h-1 w-3/5 overflow-hidden rounded-full bg-white/25">
              <div className="h-full bg-white transition-[width]" style={{ width: `${p * 100}%` }} />
            </div>
          )}
        </div>
      )}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            size="xs"
            variant="secondary"
            className="absolute right-2 bottom-2 bg-black/55 text-white opacity-0 shadow backdrop-blur group-hover:opacity-100 hover:bg-black/70 focus-visible:opacity-100 data-[state=open]:opacity-100 max-lg:opacity-100"
          >
            <ImageIcon /> Cover
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-48">
          <DropdownMenuItem disabled={making} onSelect={ai}>
            <Sparkles /> {j.cover ? "New AI cover" : "AI cover"}
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => input.current?.click()}>
            <ImagePlus /> Use an image …
          </DropdownMenuItem>
          {j.cover && (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem onSelect={() => setCover(j, "")}>
                <Trash2 /> Remove cover
              </DropdownMenuItem>
            </>
          )}
        </DropdownMenuContent>
      </DropdownMenu>
      <input
        ref={input}
        type="file"
        accept="image/*"
        hidden
        onChange={async (e) => {
          const f = e.target.files?.[0];
          e.target.value = "";
          if (!f) return;
          try {
            await setCover(j, (await api.upload(f)).url);
          } catch (err) {
            toast.error("Cover", { description: errorText(err) });
          }
        }}
      />
    </div>
  );
}
