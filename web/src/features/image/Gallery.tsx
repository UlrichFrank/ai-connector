import { Download, ImageIcon, ImagePlus, Star, Trash2, Wand2 } from "lucide-react";
import { useMemo, useState } from "react";
import { OfflineError } from "@/api/client";
import type { Job } from "@/api/types";
import { ErrorNotice } from "@/components/connection/ErrorNotice";
import { DeleteJob } from "@/components/kit/JobBits";
import { Empty, SectionHeader } from "@/components/kit/Layout";
import { FilterChip } from "@/components/media/FilterChip";
import { Masonry } from "@/components/media/Masonry";
import { MediaCard } from "@/components/media/MediaCard";
import { MediaViewer } from "@/components/media/MediaViewer";
import { dims, download, mainFile } from "@/components/media/media";
import { Button } from "@/components/ui/button";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";
import { isActive, useApp } from "@/store/app";

type Filter = "all" | "starred";

/** The images made on this page (not songs' covers), newest first, as a masonry; a click opens the viewer. */
export function Gallery({ onUseAgain, onEdit }: { onUseAgain: (j: Job) => void; onEdit: (j: Job) => void }) {
  const jobs = useApp((s) => s.jobs);
  const connection = useApp((s) => s.connection);
  const [filter, setFilter] = useState<Filter>(() => local.get<Filter>("image.filter", "all"));
  const [open, setOpen] = useState<string | null>(null);

  const all = useMemo(
    () =>
      Object.values(jobs)
        .filter((j) => j.kind === "image" && !j.request.coverFor)
        .sort((a, b) => b.created.localeCompare(a.created)),
    [jobs],
  );
  const starred = useMemo(() => all.filter((j) => j.starred), [all]);
  const shown = filter === "starred" ? starred : all;
  const running = all.filter(isActive).length;

  const pick = (f: Filter) => (setFilter(f), local.set("image.filter", f));

  const actions = (j: Job) =>
    j.state === "done" && mainFile(j) ? (
      <>
        <Button variant="ghost" size="icon-sm" title="Use again: these settings in the composer" onClick={() => onUseAgain(j)}>
          <Wand2 />
        </Button>
        <Button variant="ghost" size="icon-sm" title="Edit: use as a reference image" onClick={() => onEdit(j)}>
          <ImagePlus />
        </Button>
        <Button variant="ghost" size="icon-sm" title="Download" onClick={() => download(mainFile(j)!)}>
          <Download />
        </Button>
        <DeleteJob
          job={j}
          trigger={(o) => (
            <Button variant="ghost" size="icon-sm" title="Delete" onClick={o}>
              <Trash2 />
            </Button>
          )}
        />
      </>
    ) : !isActive(j) ? (
      <DeleteJob
        job={j}
        trigger={(o) => (
          <Button variant="ghost" size="sm" className="text-muted-foreground hover:text-destructive" onClick={o}>
            <Trash2 /> Delete
          </Button>
        )}
      />
    ) : undefined;

  return (
    <div className="mx-auto flex max-w-[1600px] flex-col">
      {connection === "waiting" && <ErrorNotice error={new OfflineError()} className="mb-4" />}
      <SectionHeader
        title={
          <span className="flex items-baseline gap-2">
            Images
            {all.length > 0 && (
              <span className="text-xs font-normal text-muted-foreground">
                {all.length}
                {running > 0 && ` · ${running} in progress`}
              </span>
            )}
          </span>
        }
      >
        {all.length > 0 && (
          <div className="flex gap-1">
            <FilterChip on={filter === "all"} onClick={() => pick("all")}>
              All
            </FilterChip>
            <FilterChip on={filter === "starred"} onClick={() => pick("starred")} count={starred.length}>
              <Star className={cn("size-3.5", filter === "starred" && "fill-current text-amber-400")} /> Starred
            </FilterChip>
          </div>
        )}
      </SectionHeader>

      {shown.length ? (
        <Masonry
          items={shown}
          keyOf={(j) => j.id}
          aspectOf={(j) => {
            const { w, h } = dims(j);
            return w / h;
          }}
          minHeightOf={(j) => (j.state === "failed" || j.state === "canceled" ? 230 : isActive(j) ? 170 : 0)}
          column={280}
          gap={12}
          render={(j) => <MediaCard job={j} onOpen={() => setOpen(j.id)} actions={actions(j)} />}
        />
      ) : filter === "starred" && all.length ? (
        <Empty icon={<Star />} title="Nothing starred yet">
          Hover over an image and press ☆ to keep it here.
        </Empty>
      ) : (
        <Empty icon={<ImageIcon />} title="No images yet">
          Describe a picture and press Create. To change a photo instead, drop it into “Edit a picture” and say what should change.
        </Empty>
      )}

      <MediaViewer items={shown} id={open} onNavigate={setOpen} onClose={() => setOpen(null)} onUseAgain={onUseAgain} />
    </div>
  );
}
