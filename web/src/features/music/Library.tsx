import { ChevronRight, Folder, FolderInput, GitBranch, FolderPlus, Loader2, MoreHorizontal, Palette, Pause, Pencil, Play, Plus, Search, Star, Trash2, TriangleAlert, X } from "lucide-react";
import { memo, useMemo, useState, type DragEvent, type ReactNode } from "react";
import type { Job } from "@/api/types";
import { StarButton } from "@/components/kit/JobBits";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { progress } from "@/lib/format";
import { navigate } from "@/lib/route";
import { cn } from "@/lib/utils";
import { isActive } from "@/store/app";
import { usePlayer, usePlaying } from "@/store/player";
import { SongArt } from "./Art";
import { NewFolderDialog, RemoveFolderDialog, RenameFolderDialog } from "./Dialogs";
import { folderOf, inFolder, isUpload, leaf, mmss, songFile, songLen, songTitle, STARRED, versionLabel, type Family } from "./songs";
import { draggingFiles, importFiles, moveSong, useFolders, useSongs, useStudio } from "./state";
import { OnMac } from "@/components/kit/MacTag";

/** One row of the list: a song (the original of a family, or a version found by search or ★). */
interface Item {
  job: Job;
  versions: number;
  busy?: Job;
}

/** The library column: folders, ★, search, and the songs. */
export function Library({ open }: { open: string }) {
  const { songs, families } = useSongs();
  const folders = useFolders(songs);
  const folder = useStudio((s) => s.folder);
  const setFolder = useStudio((s) => s.setFolder);
  const search = useStudio((s) => s.search);
  const setSearch = useStudio((s) => s.setSearch);
  const [newOpen, setNewOpen] = useState(false);
  const q = search.trim().toLowerCase();
  const starred = folder === STARRED;

  // the open song's family is highlighted
  const openRoot = useMemo(() => families.find((f) => f.members.some((m) => m.id === open))?.root.id, [families, open]);

  const items: Item[] = useMemo(() => {
    const fam = new Map<string, Family>();
    for (const f of families) for (const m of f.members) fam.set(m.id, f);
    if (q || starred) {
      // single songs: every version that matches
      return songs
        .filter((j) => (starred ? j.starred : true))
        .filter((j) => !q || [songTitle(j), j.request.prompt, j.request.lyrics, folderOf(j), j.name].join(" ").toLowerCase().includes(q))
        .map((j) => ({ job: j, versions: 0, busy: isActive(j) ? j : undefined }));
    }
    return families.filter((f) => folderOf(f.root) === folder).map((f) => ({ job: f.root, versions: f.members.length - 1, busy: f.busy }));
  }, [songs, families, folder, q, starred]);

  const kids = useMemo(
    () => (q || starred ? [] : folders.filter((f) => (folder ? f.startsWith(folder + "/") && !f.slice(folder.length + 1).includes("/") : !f.includes("/")))),
    [folders, folder, q, starred],
  );
  const count = (f: string) => families.filter((x) => inFolder(x.root, f)).length;
  const nStarred = songs.filter((j) => j.starred).length;

  return (
    <aside className="flex w-80 shrink-0 flex-col border-r bg-card/30 max-lg:w-full max-lg:border-t max-lg:border-r-0 lg:min-h-0">
      <div className="flex flex-col gap-3 border-b p-3">
        <div className="flex items-center gap-1">
          <h2 className="pl-1 text-sm font-semibold">Library</h2>
          <span className="text-xs text-muted-foreground tabular-nums">{families.length}</span>
          <div className="flex-1" />
          <Button variant="ghost" size="icon-sm" title="New folder" onClick={() => setNewOpen(true)} disabled={starred}>
            <FolderPlus />
          </Button>
          <Button variant="ghost" size="icon-sm" title="Your styles: train a song model on your own tracks" onClick={() => navigate("music", "styles")}>
            <Palette className={cn(open.startsWith("styles") && "text-primary")} />
          </Button>
          <Button size="sm" variant={open ? "outline" : "secondary"} onClick={() => navigate("music")} title="Make a new song">
            <Plus /> New song
          </Button>
        </div>
        <div className="relative">
          <Search className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input value={search} onChange={(e) => setSearch(e.target.value)} placeholder="Search title, style, lyrics" className="h-8 pr-8 pl-8 text-sm" />
          {search && (
            <button className="absolute top-1/2 right-2 -translate-y-1/2 text-muted-foreground hover:text-foreground" onClick={() => setSearch("")} title="Clear">
              <X className="size-4" />
            </button>
          )}
        </div>
        {!q && <Crumbs folder={folder} />}
      </div>

      <div className="flex min-h-0 flex-1 flex-col gap-0.5 overflow-y-auto p-2 max-lg:overflow-visible">
        {!q && !folder && (
          <FolderRow icon={<Star className="size-4 fill-amber-400 text-amber-400" />} name="Starred" count={nStarred} onOpen={() => setFolder(STARRED)} />
        )}
        {kids.map((f) => (
          <FolderRow key={f} folder={f} icon={<Folder className="size-4 text-primary" />} name={leaf(f)} count={count(f)} onOpen={() => setFolder(f)} />
        ))}
        {(kids.length > 0 || (!q && !folder)) && items.length > 0 && <div className="my-1.5 border-t" />}
        {items.map((it) => (
          <SongRow key={it.job.id} job={it.job} versions={it.versions} busy={it.busy} selected={it.job.id === open || (!q && !starred && it.job.id === openRoot)} folders={folders} />
        ))}
        {!items.length && (
          <div className="px-3 py-8 text-center text-sm text-muted-foreground">
            {q
              ? "No song matches."
              : starred
                ? "Star a song to find it here."
                : folder
                  ? kids.length
                    ? ""
                    : "No songs in this folder yet. Drag one here, or make one while the folder is open."
                  : "Your songs appear here. Describe one on the right to make your first."}
          </div>
        )}
      </div>
      <NewFolderDialog open={newOpen} onOpenChange={setNewOpen} into={starred ? "" : folder} then={setFolder} />
    </aside>
  );
}

/** Takes a dragged song: moves it (with its versions) into `folder`. */
function useDrop(folder: string | null) {
  const [over, setOver] = useState(false);
  if (folder == null || folder === STARRED) return { over: false, props: {} };
  return {
    over,
    props: {
      onDragOver: (e: DragEvent) => {
        const files = draggingFiles(e);
        if (!files && !e.dataTransfer.types.includes("text/song")) return;
        e.preventDefault();
        e.stopPropagation(); // this folder, not the page's drop zone
        e.dataTransfer.dropEffect = files ? "copy" : "move";
        setOver(true);
      },
      onDragLeave: () => setOver(false),
      onDrop: (e: DragEvent) => {
        e.preventDefault();
        e.stopPropagation();
        setOver(false);
        // audio files from the Finder are imported into this folder; one of our songs moves here
        if (draggingFiles(e)) return void importFiles([...e.dataTransfer.files], folder);
        const id = e.dataTransfer.getData("text/song");
        if (id) void moveSong(id, folder);
      },
    },
  };
}

/** Where we are: All songs › a › b (each one a drop target), with the folder's menu. */
function Crumbs({ folder }: { folder: string }) {
  const setFolder = useStudio((s) => s.setFolder);
  const [rename, setRename] = useState(false);
  const [remove, setRemove] = useState(false);
  const parts = folder && folder !== STARRED ? folder.split("/") : [];
  const path = (i: number) => parts.slice(0, i + 1).join("/");
  return (
    <div className="flex min-h-7 flex-wrap items-center gap-0.5 text-sm">
      <Crumb folder="" label="All songs" current={!folder} />
      {folder === STARRED && (
        <>
          <ChevronRight className="size-3.5 text-muted-foreground" />
          <Crumb folder={null} label="★ Starred" current />
        </>
      )}
      {parts.map((p, i) => (
        <span key={i} className="flex items-center gap-0.5">
          <ChevronRight className="size-3.5 text-muted-foreground" />
          <Crumb folder={path(i)} label={p} current={i === parts.length - 1} />
        </span>
      ))}
      {parts.length > 0 && (
        <>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon-xs" className="ml-0.5 text-muted-foreground" title="Folder">
                <MoreHorizontal />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start">
              <DropdownMenuItem onSelect={() => setRename(true)}>
                <Pencil /> Rename folder
              </DropdownMenuItem>
              <DropdownMenuItem onSelect={() => setRemove(true)}>
                <Trash2 /> Remove folder
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem onSelect={() => setFolder(parts.slice(0, -1).join("/"))}>Up one level</DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
          <RenameFolderDialog folder={folder} open={rename} onOpenChange={setRename} />
          <RemoveFolderDialog folder={folder} open={remove} onOpenChange={setRemove} />
        </>
      )}
    </div>
  );
}

function Crumb({ folder, label, current }: { folder: string | null; label: string; current?: boolean }) {
  const setFolder = useStudio((s) => s.setFolder);
  const drop = useDrop(current ? null : folder);
  return (
    <button
      {...drop.props}
      disabled={current || folder == null}
      onClick={() => folder != null && setFolder(folder)}
      className={cn(
        "max-w-40 truncate rounded-md px-1.5 py-0.5 transition-colors",
        current ? "font-medium text-foreground" : "text-muted-foreground hover:bg-accent hover:text-foreground",
        drop.over && "bg-primary/20 text-foreground ring-1 ring-primary",
      )}
    >
      {label}
    </button>
  );
}

function FolderRow({ folder, icon, name, count, onOpen }: { folder?: string; icon: ReactNode; name: string; count: number; onOpen: () => void }) {
  const drop = useDrop(folder ?? null);
  const [rename, setRename] = useState(false);
  const [remove, setRemove] = useState(false);
  return (
    <div
      {...drop.props}
      role="button"
      tabIndex={0}
      onClick={onOpen}
      onKeyDown={(e) => e.key === "Enter" && onOpen()}
      className={cn(
        "group flex h-10 items-center gap-2.5 rounded-lg px-2.5 text-sm transition-colors hover:bg-accent/60",
        drop.over && "bg-primary/15 ring-1 ring-primary",
      )}
    >
      {icon}
      <span className="min-w-0 flex-1 truncate">{name}</span>
      <span className="text-xs text-muted-foreground tabular-nums">{count}</span>
      {folder && (
        <>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button
                variant="ghost"
                size="icon-xs"
                className="text-muted-foreground opacity-0 group-hover:opacity-100 focus-visible:opacity-100 data-[state=open]:opacity-100 max-lg:opacity-100"
                onClick={(e) => e.stopPropagation()}
                title="Folder"
              >
                <MoreHorizontal />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" onClick={(e) => e.stopPropagation()}>
              <DropdownMenuItem onSelect={() => setRename(true)}>
                <Pencil /> Rename
              </DropdownMenuItem>
              <DropdownMenuItem onSelect={() => setRemove(true)}>
                <Trash2 /> Remove
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
          <div onClick={(e) => e.stopPropagation()} className="contents">
            <RenameFolderDialog folder={folder} open={rename} onOpenChange={setRename} />
            <RemoveFolderDialog folder={folder} open={remove} onOpenChange={setRemove} />
          </div>
        </>
      )}
    </div>
  );
}

/** Play / pause from the global player; acts on press, so a row that redraws under the pointer cannot swallow it. */
export function PlayButton({ job: j, className, size = "icon-sm", variant = "ghost" }: { job: Job; className?: string; size?: "icon-sm" | "icon" | "icon-lg"; variant?: "ghost" | "default" | "secondary" }) {
  const file = songFile(j);
  const playing = usePlaying(file);
  if (!file) return null;
  return (
    <Button
      variant={variant}
      size={size}
      title={playing ? "Pause" : "Play"}
      className={cn("rounded-full", className)}
      onPointerDown={(e) => {
        if (e.button !== 0) return;
        e.preventDefault();
        e.stopPropagation();
        usePlayer.getState().toggle(j, file);
      }}
      onClick={(e) => e.stopPropagation()}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          usePlayer.getState().toggle(j, file);
        }
      }}
    >
      {playing ? <Pause className="fill-current" /> : <Play className="fill-current" />}
    </Button>
  );
}

const SongRow = memo(function SongRow({ job: j, versions, busy, selected, folders }: Item & { selected: boolean; folders: string[] }) {
  const file = songFile(j);
  const playing = usePlaying(file);
  const loaded = usePlayer((s) => !!file && s.file === file);
  const p = busy ? progress(busy) : undefined;
  const failed = j.state === "failed" || j.state === "canceled";
  const len = songLen(j);
  return (
    <div
      role="button"
      tabIndex={0}
      draggable
      onDragStart={(e) => {
        e.dataTransfer.setData("text/song", j.id);
        e.dataTransfer.effectAllowed = "move";
      }}
      onClick={() => navigate("music", j.id)}
      onKeyDown={(e) => e.key === "Enter" && e.target === e.currentTarget && navigate("music", j.id)}
      className={cn(
        "group relative flex items-center gap-3 rounded-lg p-1.5 pr-1 transition-colors outline-none hover:bg-accent/60 focus-visible:ring-2 focus-visible:ring-ring",
        selected && "bg-accent hover:bg-accent",
      )}
    >
      <SongArt job={j} className="size-11" icon={!file || !(playing || loaded)}>
        {file && (
          <button
            title={playing ? "Pause" : "Play"}
            className={cn(
              "absolute inset-0 grid place-items-center bg-black/45 text-white opacity-0 transition-opacity group-hover:opacity-100 focus-visible:opacity-100",
              (playing || loaded) && "opacity-100",
            )}
            onPointerDown={(e) => {
              if (e.button !== 0) return;
              e.preventDefault();
              e.stopPropagation();
              usePlayer.getState().toggle(j, file);
            }}
            onClick={(e) => e.stopPropagation()}
          >
            {playing ? <Bars /> : <Play className="size-4 fill-current" />}
          </button>
        )}
        {busy && !file && (
          <div className="absolute inset-0 grid place-items-center bg-black/40 text-[11px] font-medium text-white tabular-nums">
            {p != null ? `${Math.round(p * 100)}%` : <Loader2 className="size-4 animate-spin" />}
          </div>
        )}
        {failed && !file && (
          <div className="absolute inset-0 grid place-items-center bg-black/50 text-white">
            <TriangleAlert className="size-4" />
          </div>
        )}
      </SongArt>
      <div className="min-w-0 flex-1">
        <div className={cn("truncate text-sm font-medium", playing && "text-primary")}>{songTitle(j)}</div>
        <div className="flex items-center gap-1.5 truncate text-xs text-muted-foreground">
          {busy ? (
            <span className="flex items-center gap-1 truncate text-primary">
              <Loader2 className="size-3 shrink-0 animate-spin" />
              {busy.id !== j.id && <span>{versionLabel(busy)} ·</span>}
              {busy.state === "queued" ? "waiting" : p != null ? `${Math.round(p * 100)}%` : (busy.status ?? "starting")}
            </span>
          ) : failed ? (
            <span className="truncate text-destructive">{j.problem?.title ?? "Failed"}</span>
          ) : (
            <>
              {j.request.parent && <span className="shrink-0 rounded bg-muted px-1 py-px text-[10px] font-medium text-foreground/80">{versionLabel(j)}</span>}
              <span className="min-w-0 truncate">{isUpload(j) ? "Imported" : j.name}</span>
              {len > 0 && <span className="shrink-0 tabular-nums">· {mmss(len)}</span>}
              <OnMac job={j} plain prefix="· " className="shrink-0" />
            </>
          )}
          {versions > 0 && (
            <span className="flex shrink-0 items-center gap-0.5 tabular-nums" title={`${versions + 1} versions`}>
              · <GitBranch className="size-3" />
              {versions + 1}
            </span>
          )}
        </div>
        {busy && <div className="absolute right-2 bottom-0 left-2 h-0.5 overflow-hidden rounded-full bg-primary/15">{p != null && <div className="h-full bg-primary transition-[width]" style={{ width: `${p * 100}%` }} />}</div>}
      </div>
      <div className="flex shrink-0 items-center">
        {file && <StarButton job={j} className={cn(!j.starred && "opacity-0 group-hover:opacity-100 max-lg:opacity-100")} />}
        <MoveMenu job={j} folders={folders} />
      </div>
    </div>
  );
});

/** Move a song (and its versions) into a folder: the way on a phone, where there is no dragging. */
function MoveMenu({ job: j, folders }: { job: Job; folders: string[] }) {
  const [newOpen, setNewOpen] = useState(false);
  const cur = folderOf(j);
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="ghost"
            size="icon-sm"
            className="text-muted-foreground opacity-0 group-hover:opacity-100 focus-visible:opacity-100 data-[state=open]:opacity-100 max-lg:opacity-100"
            onClick={(e) => e.stopPropagation()}
            title="More"
          >
            <MoreHorizontal />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" onClick={(e) => e.stopPropagation()} className="w-52">
          <DropdownMenuItem onSelect={() => navigate("music", j.id)}>Open</DropdownMenuItem>
          <DropdownMenuSub>
            <DropdownMenuSubTrigger>
              <FolderInput /> Move to
            </DropdownMenuSubTrigger>
            <DropdownMenuSubContent className="max-h-80 w-52 overflow-y-auto">
              <FolderItems current={cur} folders={folders} onPick={(f) => moveSong(j.id, f)} onNew={() => setNewOpen(true)} />
            </DropdownMenuSubContent>
          </DropdownMenuSub>
        </DropdownMenuContent>
      </DropdownMenu>
      <div onClick={(e) => e.stopPropagation()} className="contents">
        <NewFolderDialog open={newOpen} onOpenChange={setNewOpen} into="" then={(f) => moveSong(j.id, f)} />
      </div>
    </>
  );
}

/** The folders as menu items (No folder, each folder indented by depth, New folder …). */
export function FolderItems({ current, folders, onPick, onNew }: { current: string; folders: string[]; onPick: (f: string) => void; onNew: () => void }) {
  return (
    <>
      <DropdownMenuLabel className="text-xs text-muted-foreground">Versions come along</DropdownMenuLabel>
      <DropdownMenuItem disabled={!current} onSelect={() => onPick("")}>
        <Folder className="opacity-40" /> No folder
      </DropdownMenuItem>
      {folders.map((f) => (
        <DropdownMenuItem key={f} disabled={f === current} onSelect={() => onPick(f)} style={{ paddingLeft: 8 + (f.split("/").length - 1) * 14 }}>
          <Folder className="text-primary" /> <span className="truncate">{leaf(f)}</span>
        </DropdownMenuItem>
      ))}
      <DropdownMenuSeparator />
      <DropdownMenuItem onSelect={onNew}>
        <FolderPlus /> New folder …
      </DropdownMenuItem>
    </>
  );
}

/** Three bouncing bars: this one is playing. */
function Bars() {
  return (
    <span className="flex h-4 items-end gap-[3px]">
      {[0, 1, 2].map((i) => (
        <span key={i} className="w-[3px] animate-pulse rounded-full bg-white" style={{ height: `${[60, 100, 75][i]}%`, animationDelay: `${i * 0.18}s`, animationDuration: "0.9s" }} />
      ))}
    </span>
  );
}
