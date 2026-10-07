import { ArrowDownUp, AudioLines, Clapperboard, ImageIcon, Laptop, Library, Music, Search, Star, X } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { OfflineError } from "@/api/client";
import type { Job } from "@/api/types";
import { ErrorNotice } from "@/components/connection/ErrorNotice";
import { Empty } from "@/components/kit/Layout";
import { FilterChip } from "@/components/media/FilterChip";
import { MediaCard } from "@/components/media/MediaCard";
import { MediaViewer } from "@/components/media/MediaViewer";
import { isAudio, isVisual, promptOf } from "@/components/media/media";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { isTranscription, title } from "@/lib/format";
import { macsOf } from "@/lib/macs";
import { navigate } from "@/lib/route";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";

type Type = "all" | "image" | "video" | "music" | "audio";
type Sort = "newest" | "oldest";

/**
 * What the library holds: pictures (not songs' covers), videos, movies and avatars, songs (not their versions),
 * speech (not transcriptions) and sound effects; nothing failed.
 */
function inLibrary(j: Job): boolean {
  if (j.state === "failed" || j.state === "canceled") return false;
  if (j.kind === "image") return !j.request.coverFor;
  if (j.kind === "video") return true;
  if (j.kind === "music") return !j.request.parent && j.request.task !== "train-style"; // a style's training is no song
  if (j.kind === "avatar" || j.kind === "sound") return true;
  if (j.kind === "speech") return !isTranscription(j);
  return false;
}

/** The filter a job falls under: avatars are videos, speech and sound effects are audio. */
function typeOf(j: Job): Exclude<Type, "all"> {
  if (j.kind === "avatar") return "video";
  if (isAudio(j)) return "audio";
  return j.kind as "image" | "video" | "music";
}

const page = 60;

/** Everything made, in one place. `arg`: a job to open at once (links from toasts and Activity). */
export function LibraryPage({ arg }: { arg: string }) {
  const jobs = useApp((s) => s.jobs);
  const connection = useApp((s) => s.connection);
  const [type, setType] = useState<Type>(() => local.get<Type>("library.type", "all"));
  const [starred, setStarred] = useState(() => local.get("library.starred", false));
  const [sort, setSort] = useState<Sort>(() => local.get<Sort>("library.sort", "newest"));
  const [query, setQuery] = useState("");
  // several Macs: only what one of them holds ("" = every Mac)
  const [mac, setMac] = useState(() => local.get("library.mac", ""));
  // how many are drawn; back to one page when the filters change
  const sig = `${type}|${starred}|${sort}|${query}|${mac}`;
  const [shownCount, setShownCount] = useState({ sig, n: page });
  const limit = shownCount.sig === sig ? shownCount.n : page;

  useEffect(() => local.set("library.type", type), [type]);
  useEffect(() => local.set("library.starred", starred), [starred]);
  useEffect(() => local.set("library.sort", sort), [sort]);
  useEffect(() => local.set("library.mac", mac), [mac]);

  const all = useMemo(() => Object.values(jobs).filter(inLibrary), [jobs]);
  const macs = useMemo(() => macsOf(all), [all]);
  // a remembered Mac that holds nothing here (any more) filters nothing
  const onMac = macs.length > 1 && macs.some((m) => m.id === mac) ? mac : "";
  const counts = useMemo(() => {
    const c = { image: 0, video: 0, music: 0, audio: 0 };
    for (const j of all) c[typeOf(j)]++;
    return c;
  }, [all]);

  const shown = useMemo(() => {
    const q = query.trim().toLowerCase();
    const l = all.filter(
      (j) => (type === "all" || typeOf(j) === type) && (!starred || j.starred) && (!onMac || j.worker?.id === onMac) && (!q || `${title(j)}\n${promptOf(j)}`.toLowerCase().includes(q)),
    );
    l.sort((a, b) => (sort === "newest" ? b.created.localeCompare(a.created) : a.created.localeCompare(b.created)));
    return l;
  }, [all, type, starred, sort, query, onMac]);

  const visual = useMemo(() => shown.filter(isVisual), [shown]);
  const groups = useMemo(() => groupByDay(shown.slice(0, limit)), [shown, limit]);

  // #/library/<id>: a song opens in the studio, a song's cover with its song; pictures and videos here
  const target = arg ? jobs[arg] : undefined;
  useEffect(() => {
    if (!target) return;
    if (isAudio(target)) location.replace(`#/voice/${encodeURIComponent(target.id)}`);
    else if (target.kind === "music") location.replace(`#/music/${encodeURIComponent(target.request.coverFor ?? target.id)}`);
    else if (target.request.coverFor) location.replace(`#/music/${encodeURIComponent(target.request.coverFor)}`);
  }, [target]);

  // more as the end comes into view
  const more = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = more.current;
    if (!el) return;
    const io = new IntersectionObserver((e) => e[0]?.isIntersecting && setShownCount({ sig, n: limit + page }), { rootMargin: "800px" });
    io.observe(el);
    return () => io.disconnect();
  }, [shown.length, limit, sig]);

  const open = (j: Job) => (j.kind === "music" ? navigate("music", j.id) : isAudio(j) ? navigate("voice", j.id) : navigate("library", j.id));
  const filtered = type !== "all" || starred || !!query.trim() || !!onMac;

  return (
    <div className="mx-auto flex max-w-[1600px] flex-col gap-5 p-5 max-sm:p-4">
      {connection === "waiting" && <ErrorNotice error={new OfflineError()} />}

      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <div className="flex items-center gap-1 overflow-x-auto max-sm:-mx-4 max-sm:w-[calc(100%+2rem)] max-sm:px-4 [&::-webkit-scrollbar]:hidden">
          <FilterChip on={type === "all"} onClick={() => setType("all")}>
            All
          </FilterChip>
          <FilterChip on={type === "image"} onClick={() => setType("image")} count={counts.image}>
            <ImageIcon /> Images
          </FilterChip>
          <FilterChip on={type === "video"} onClick={() => setType("video")} count={counts.video}>
            <Clapperboard /> Videos
          </FilterChip>
          <FilterChip on={type === "music"} onClick={() => setType("music")} count={counts.music}>
            <Music /> Songs
          </FilterChip>
          <FilterChip on={type === "audio"} onClick={() => setType("audio")} count={counts.audio}>
            <AudioLines /> Audio
          </FilterChip>
          <span className="mx-1 h-5 w-px shrink-0 bg-border" />
          <FilterChip on={starred} onClick={() => setStarred(!starred)}>
            <Star className={cn(starred && "fill-current text-amber-400")} /> Starred
          </FilterChip>
          {macs.length > 1 && (
            <>
              <span className="mx-1 h-5 w-px shrink-0 bg-border" />
              {macs.map((m) => (
                <FilterChip key={m.id} on={onMac === m.id} onClick={() => setMac(onMac === m.id ? "" : m.id)} count={m.count}>
                  <Laptop /> {m.name}
                </FilterChip>
              ))}
            </>
          )}
        </div>
        <div className="flex-1" />
        <div className="flex items-center gap-1.5 max-sm:w-full">
          <div className="relative max-sm:flex-1">
            <Search className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => e.key === "Escape" && setQuery("")}
              placeholder="Search prompts and titles"
              aria-label="Search"
              className="h-8 w-64 pr-8 pl-8 max-sm:w-full"
            />
            {query && (
              <Button variant="ghost" size="icon-xs" title="Clear" className="absolute top-1/2 right-1.5 -translate-y-1/2" onClick={() => setQuery("")}>
                <X />
              </Button>
            )}
          </div>
          <Button variant="ghost" size="sm" className="text-muted-foreground" title="Change the order" onClick={() => setSort(sort === "newest" ? "oldest" : "newest")}>
            <ArrowDownUp /> {sort === "newest" ? "Newest" : "Oldest"}
          </Button>
        </div>
      </div>

      {shown.length ? (
        <div className="flex flex-col gap-8">
          {groups.map((g) => (
            <section key={g.label} className="flex flex-col gap-3">
              <h2 className="text-sm font-semibold text-muted-foreground">
                {g.label} <span className="font-normal">· {g.items.length}</span>
              </h2>
              <div className="grid grid-cols-[repeat(auto-fill,minmax(210px,1fr))] gap-3 max-sm:grid-cols-2">
                {g.items.map((j) => (
                  <div key={j.id} className="aspect-square">
                    <MediaCard job={j} badge onOpen={() => open(j)} />
                  </div>
                ))}
              </div>
            </section>
          ))}
          {limit < shown.length && <div ref={more} className="h-10" />}
        </div>
      ) : filtered && all.length ? (
        <Empty icon={<Search />} title="Nothing matches">
          {query.trim() ? <>No {type === "all" ? "" : label(type) + " "}matches “{query.trim()}”. </> : null}
          <button className="text-primary hover:underline" onClick={() => (setType("all"), setStarred(false), setQuery(""), setMac(""))}>
            Show everything
          </button>
        </Empty>
      ) : (
        <Empty icon={<Library />} title="Nothing made yet">
          Images, videos, songs, speech and avatars you make show up here. Start on the{" "}
          <a href="#/image" className="text-primary hover:underline">
            Image
          </a>
          ,{" "}
          <a href="#/video" className="text-primary hover:underline">
            Video
          </a>{" "}
          or{" "}
          <a href="#/music" className="text-primary hover:underline">
            Music
          </a>{" "}
          page.
        </Empty>
      )}

      <MediaViewer
        items={visual}
        id={target && isVisual(target) && !target.request.coverFor ? target.id : null}
        onNavigate={(id) => location.replace(`#/library/${encodeURIComponent(id)}`)}
        onClose={() => location.replace("#/library")}
        onUseAgain={(j) => navigate(j.kind === "avatar" ? "avatar" : "image", j.id)}
        canUseAgain={(j) => j.kind === "image" || j.kind === "avatar"}
        inLibrary
      />
    </div>
  );
}

function label(t: Type): string {
  return { all: "", image: "image", video: "video", music: "song", audio: "audio" }[t];
}

/** "Today", "Yesterday", "Monday" (this week), "12 September", "12 September 2025". */
function dayLabel(iso: string, now = new Date()): string {
  const d = new Date(iso);
  const start = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const days = Math.round((start(now) - start(d)) / 86400000);
  if (days <= 0) return "Today";
  if (days === 1) return "Yesterday";
  if (days < 7) return d.toLocaleDateString(undefined, { weekday: "long" });
  return d.toLocaleDateString(undefined, { day: "numeric", month: "long", ...(d.getFullYear() !== now.getFullYear() ? { year: "numeric" } : {}) });
}

function groupByDay(jobs: Job[]): { label: string; items: Job[] }[] {
  const out: { label: string; items: Job[] }[] = [];
  for (const j of jobs) {
    const l = dayLabel(j.created);
    const last = out[out.length - 1];
    if (last?.label === l) last.items.push(j);
    else out.push({ label: l, items: [j] });
  }
  return out;
}
