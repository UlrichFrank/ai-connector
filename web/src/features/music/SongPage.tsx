import { ArrowLeft, Check, ChevronDown, Copy, Download, Folder, MoreHorizontal, Pencil, RotateCcw, Square, Trash2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Job } from "@/api/types";
import { DeleteJob, JobError, JobProgress, StarButton } from "@/components/kit/JobBits";
import { Empty } from "@/components/kit/Layout";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { ago } from "@/lib/format";
import { navigate } from "@/lib/route";
import { cn } from "@/lib/utils";
import { isActive, useApp } from "@/store/app";
import { Cover } from "./Cover";
import { NewFolderDialog } from "./Dialogs";
import { FolderItems, PlayButton } from "./Library";
import { DEFAULT_LENGTH, folderOf, isUpload, rootOf, leaf, mmss, songFile, songLen, songSeed, songTitle, versionLabel } from "./songs";
import { moveSong, useFolders, useSongs, useStudio } from "./state";
import { Tools } from "./Tools";
import { Versions } from "./Versions";
import { Waveform } from "./Waveform";
import { OnMac } from "@/components/kit/MacTag";

/** One song: its cover and facts, the waveform, the studio tools, its versions and lyrics. */
export function SongPage({ id }: { id: string }) {
  const { songs, jobs } = useSongs();
  const connection = useApp((s) => s.connection);
  const j = jobs[id];
  if (!j || j.kind !== "music") {
    if (!Object.keys(jobs).length && connection !== "live")
      return (
        <div className="flex flex-col gap-6 p-6">
          <div className="flex gap-6">
            <Skeleton className="size-56 rounded-xl max-sm:size-36" />
            <div className="flex flex-1 flex-col gap-3 pt-4">
              <Skeleton className="h-8 w-2/3" />
              <Skeleton className="h-4 w-1/2" />
            </div>
          </div>
          <Skeleton className="h-28 rounded-xl" />
        </div>
      );
    return (
      <div className="p-6">
        <Empty title="This song is not here">
          It may have been deleted.{" "}
          <button className="text-primary hover:underline" onClick={() => navigate("music")}>
            Make a new one
          </button>
          .
        </Empty>
      </div>
    );
  }
  return <Song job={j} songs={songs} jobs={jobs} />;
}

function Song({ job: j, songs, jobs }: { job: Job; songs: Job[]; jobs: Record<string, Job> }) {
  const models = useApp((s) => s.models);
  const r = j.request;
  const file = songFile(j);
  const len = songLen(j);
  const parent = r.parent ? jobs[r.parent] : undefined;
  const busy = isActive(j);
  const failed = j.state === "failed" || j.state === "canceled";

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6 p-6 max-sm:gap-5 max-sm:p-4">
      <button className="flex w-fit items-center gap-1 text-xs text-muted-foreground hover:text-foreground lg:hidden" onClick={() => navigate("music")}>
        <ArrowLeft className="size-3.5" /> New song
      </button>

      {/* the head: cover, title, facts, actions */}
      <header className="flex gap-6 max-sm:flex-col max-sm:items-center max-sm:gap-4">
        <Cover job={j} base={rootOf(j, jobs)} className="w-56 shrink-0 max-md:w-44 max-sm:w-56" />
        <div className="flex min-w-0 flex-1 flex-col gap-3 pt-1 max-sm:w-full">
          <div className="flex flex-col gap-1">
            <div className="flex items-center gap-2 text-xs font-medium tracking-wide text-muted-foreground uppercase">
              {versionLabel(j)}
              {parent && (
                <>
                  <span className="normal-case">of</span>
                  <button className="truncate text-primary normal-case hover:underline" onClick={() => navigate("music", parent.id)}>
                    {songTitle(parent)}
                  </button>
                </>
              )}
            </div>
            <Title job={j} />
          </div>
          {r.prompt && <Style text={r.prompt} />}
          <Facts job={j} len={len} />
          <div className="mt-auto flex flex-wrap items-center gap-1.5 pt-1">
            {file && <PlayButton job={j} variant="default" size="icon-lg" className="mr-1 shadow-lg shadow-primary/30 [&_svg]:size-4" />}
            {file && <StarButton job={j} className="size-9" />}
            <FolderMenu job={j} />
            {!isUpload(j) && (
              <Button variant="ghost" size="sm" className="h-9" onClick={() => loadIntoForm(j)} title="Fill the Custom form with this song: change the seed, the model, the lyrics …">
                <RotateCcw /> Load<span className="max-sm:hidden"> into the form</span>
              </Button>
            )}
            <More job={j} file={file} />
          </div>
        </div>
      </header>

      {busy && (
        <div className="flex flex-col gap-3 rounded-xl border bg-card/50 p-4">
          <JobProgress job={j} />
          <div className="flex items-center gap-2 text-xs text-muted-foreground">
            The new version appears here when it is done; you can leave this page meanwhile.
            <div className="flex-1" />
            <Button
              size="sm"
              variant="outline"
              onPointerDown={(e) => {
                e.preventDefault();
                api.cancel(j.id).catch((err) => toast.error("Could not cancel", { description: errorText(err) }));
              }}
            >
              <Square className="fill-current" /> Cancel
            </Button>
          </div>
        </div>
      )}
      {failed && <JobError job={j} />}
      {file && <Waveform job={j} file={file} />}

      <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_320px]">
        <div className="flex min-w-0 flex-col gap-6">{file && <Tools job={j} file={file} length={len} />}</div>
        <aside className="flex min-w-0 flex-col gap-6">
          <Versions job={j} songs={songs} jobs={jobs} />
          {r.lyrics && !r.instrumental && (
            <Lyrics
              text={r.lyrics}
              sent={typeof j.stats?.lyricsSent === "string" ? j.stats.lyricsSent : undefined}
              styleSent={typeof j.stats?.styleSent === "string" ? j.stats.styleSent : undefined}
              model={models.find((m) => m.id === j.model)?.name ?? j.model}
            />
          )}
        </aside>
      </div>
    </div>
  );
}

/** Fills the Custom form with a song's request (its model and the seed it used) to make it again, changed. */
function loadIntoForm(j: Job) {
  const r = j.request;
  useStudio.getState().load(
    {
      title: songTitle(j),
      style: r.prompt ?? "",
      lyrics: r.lyrics ?? "",
      instrumental: !!r.instrumental,
      duration: Math.round(r.duration || songLen(j)) || DEFAULT_LENGTH,
      seed: String(songSeed(j) ?? ""),
    },
    j.model,
  );
  navigate("music");
}

/** The title; click (or ✎) to rename: Enter or leaving saves, Escape cancels. */
function Title({ job: j }: { job: Job }) {
  const [edit, setEdit] = useState<string | null>(null);
  const put = useApp((s) => s.put);
  const save = async () => {
    const t = edit?.trim();
    setEdit(null);
    if (!t || t === songTitle(j)) return;
    put({ ...j, request: { ...j.request, title: t } });
    try {
      put(await api.rename(j.id, t));
    } catch (e) {
      put(j);
      toast.error("Could not rename", { description: errorText(e) });
    }
  };
  if (edit != null)
    return (
      <Input
        autoFocus
        value={edit}
        onChange={(e) => setEdit(e.target.value)}
        onFocus={(e) => e.target.select()}
        onBlur={save}
        onKeyDown={(e) => {
          if (e.key === "Enter") save();
          if (e.key === "Escape") setEdit(null);
        }}
        className="h-auto py-1 text-3xl font-bold tracking-tight md:text-3xl"
      />
    );
  return (
    <h1 className="group flex min-w-0 items-start gap-2 text-3xl font-bold tracking-tight text-balance max-sm:text-2xl">
      <button className="text-left break-words hover:opacity-80" onClick={() => setEdit(songTitle(j))} title="Rename">
        {songTitle(j)}
      </button>
      <Button variant="ghost" size="icon-sm" className="mt-1 shrink-0 text-muted-foreground opacity-0 group-hover:opacity-100 max-lg:opacity-100" onClick={() => setEdit(songTitle(j))} title="Rename">
        <Pencil />
      </Button>
    </h1>
  );
}

function Style({ text }: { text: string }) {
  const [open, setOpen] = useState(false);
  return (
    <button onClick={() => setOpen(!open)} className={cn("text-left text-sm text-muted-foreground", !open && "line-clamp-2")} title={open ? undefined : "Show all"}>
      {text}
    </button>
  );
}

function Facts({ job: j, len }: { job: Job; len: number }) {
  const seed = songSeed(j);
  const [copied, setCopied] = useState(false);
  const items: React.ReactNode[] = [
    <span key="m" className="font-medium text-foreground/90">
      {isUpload(j) ? "Imported" : j.name}
    </span>,
    len ? <span key="l">{mmss(len)}</span> : null,
    seed && !isUpload(j) ? (
      <button
        key="s"
        className="inline-flex items-center gap-1 hover:text-foreground"
        title="Copy the seed"
        onClick={() => {
          navigator.clipboard?.writeText(String(seed));
          setCopied(true);
          setTimeout(() => setCopied(false), 1200);
        }}
      >
        seed {seed} {copied ? <Check className="size-3" /> : <Copy className="size-3 opacity-50" />}
      </button>
    ) : null,
    <span key="a">{ago(j.created)}</span>,
  ].filter(Boolean);
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground tabular-nums">
      {items.map((x, i) => (
        <span key={i} className="flex items-center gap-2">
          {i > 0 && <span className="opacity-40">·</span>}
          {x}
        </span>
      ))}
      <OnMac job={j} />
    </div>
  );
}

function FolderMenu({ job: j }: { job: Job }) {
  const { songs } = useSongs();
  const folders = useFolders(songs);
  const [newOpen, setNewOpen] = useState(false);
  const cur = folderOf(j);
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="sm" className="h-9 max-w-48" title="Move to a folder (its versions come along)">
            <Folder className={cn(cur && "text-primary")} />
            <span className="truncate">{cur ? leaf(cur) : "No folder"}</span>
            <ChevronDown className="opacity-60" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="max-h-80 w-56 overflow-y-auto">
          <FolderItems current={cur} folders={folders} onPick={(f) => moveSong(j.id, f)} onNew={() => setNewOpen(true)} />
        </DropdownMenuContent>
      </DropdownMenu>
      <NewFolderDialog open={newOpen} onOpenChange={setNewOpen} into="" then={(f) => moveSong(j.id, f)} />
    </>
  );
}

/** Download and Delete. */
function More({ job: j, file }: { job: Job; file?: string }) {
  const parent = j.request.parent;
  return (
    <DeleteJob
      job={j}
      after={() => navigate("music", parent && useApp.getState().jobs[parent] ? parent : "")}
      trigger={(open) => (
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" className="size-9" title="More">
              <MoreHorizontal />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            {file && (
              <DropdownMenuItem asChild>
                <a href={file} download={`${songTitle(j)}.${file.split(".").pop()}`}>
                  <Download /> Download
                </a>
              </DropdownMenuItem>
            )}
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive" disabled={isActive(j)} onSelect={open}>
              <Trash2 /> Delete
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      )}
    />
  );
}

/**
 * The lyrics as written — or, when the model got them translated to its own format (tags, directions moved out),
 * as sent to that model (stats.lyricsSent / styleSent).
 */
function Lyrics({ text, sent, styleSent, model }: { text: string; sent?: string; styleSent?: string; model: string }) {
  const [asSent, setAsSent] = useState(false);
  const differs = !!sent && sent.trim() !== text.trim();
  const shown = asSent && sent ? sent : text;
  return (
    <section className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <h3 className="text-xs font-semibold tracking-wide text-muted-foreground">Lyrics</h3>
        <div className="flex-1" />
        {differs && (
          <div className="flex rounded-md border p-0.5 text-[11px]">
            <button className={cn("rounded px-2 py-0.5", !asSent && "bg-accent font-medium")} onClick={() => setAsSent(false)}>
              As written
            </button>
            <button
              className={cn("rounded px-2 py-0.5", asSent && "bg-accent font-medium")}
              onClick={() => setAsSent(true)}
              title="What the model got: its own section tags, stage directions moved out of the sung text"
            >
              As sent to {model}
            </button>
          </div>
        )}
      </div>
      <div className="rounded-xl border bg-card/40 p-4 text-sm leading-relaxed">
        {shown.split("\n").map((l, i) =>
          /^\s*\[.*\]\s*$/.test(l) ? (
            <div key={i} className="mt-3 mb-0.5 text-[11px] font-semibold tracking-wider text-primary uppercase first:mt-0">
              {l.trim().slice(1, -1)}
            </div>
          ) : (
            <div key={i} className={cn(!l.trim() && "h-2")}>
              {l}
            </div>
          ),
        )}
      </div>
      {asSent && styleSent && (
        <p className="text-xs text-muted-foreground">
          <span className="font-medium">Style sent:</span> {styleSent}
        </p>
      )}
    </section>
  );
}
