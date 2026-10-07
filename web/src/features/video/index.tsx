import { Clapperboard, Dices, Download, Film, Lock, Star, Video, WifiOff } from "lucide-react";
import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import type { Example, Job } from "@/api/types";
import { ExamplesMenu } from "@/components/kit/Examples";
import { CreateButton } from "@/components/kit/JobBits";
import { Advanced, CreateLayout, Empty, Field, SectionHeader } from "@/components/kit/Layout";
import { ModelPicker, useModelChoice } from "@/components/kit/ModelPicker";
import { PromptBox } from "@/components/kit/PromptBox";
import { RunOn, runOnWorker } from "@/components/kit/RunOn";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { bytes, isMovie } from "@/lib/format";
import { submitEach } from "@/lib/jobs";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";
import { jobsOf, useApp } from "@/store/app";
import { ClipLength, LengthControl, SizePicker } from "./Controls";
import { jobModel, request, useDraft, type Mode } from "./draft";
import { FirstFrame } from "./FirstFrame";
import { MovieEditor } from "./MovieEditor";
import { movieFrames, secondsOf, secs } from "./units";
import { VideoCard } from "./VideoCard";

/**
 * Video: one clip from a prompt (and maybe a first frame), or a movie — a story and a script of clips rendered one
 * after another, each a new scene or continuing from the last frame of the clip before.
 *
 * `arg`: a video's job id (shows it), or a /files/ image url (starts a clip from it, e.g. "Animate" on an image).
 */
export function VideoPage({ arg }: { arg: string }) {
  const choice = useModelChoice("video");
  const d = useDraft();
  const [busy, setBusy] = useState(false);
  const composer = useRef<HTMLDivElement>(null);
  const connection = useApp((s) => s.connection);
  const macAway = connection === "waiting" || connection === "offline";

  // an image handed over from elsewhere becomes the first frame
  useEffect(() => {
    if (arg.startsWith("/files/")) d.set({ mode: "clip", image: arg });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [arg]);

  const base = { width: choice.primary?.defaults.width || 704, height: choice.primary?.defaults.height || 448 };
  const width = d.width || base.width;
  const height = d.height || base.height;
  const movie = d.mode === "movie";

  const empty = movie ? d.clips.findIndex((c) => !c.prompt.trim()) : -1;
  const problem = macAway
    ? "The Mac is offline: you can keep writing; Create works once it is back."
    : movie
      ? empty >= 0
        ? `Describe clip ${empty + 1} (or delete it).`
        : ""
      : !d.prompt.trim()
        ? "Describe the video to make."
        : "";

  const create = async () => {
    if (problem) return;
    setBusy(true);
    try {
      const jobs = await submitEach(choice.ids, { ...request(d, width, height), worker: runOnWorker("video") });
      if (jobs.length) document.getElementById("video-results")?.scrollIntoView({ behavior: "smooth", block: "start" });
    } finally {
      setBusy(false);
    }
  };

  const useAgainRef = useRef<(j: Job) => void>(() => {});
  useAgainRef.current = (j: Job) => {
    d.load(j.request);
    if (choice.models.some((m) => m.id === jobModel(j))) choice.set([jobModel(j)]);
    composer.current?.scrollIntoView({ behavior: "smooth", block: "start" });
    composer.current?.closest(".overflow-y-auto")?.scrollTo({ top: 0, behavior: "smooth" });
    toast.success(isMovie(j) ? "The movie's script is in the composer" : "Loaded into the composer", { description: "Change what you like and press Create." });
  };

  const onUse = useCallback((j: Job) => useAgainRef.current(j), []);

  const total = movie ? movieFrames(d.clips, d.clipFrames) : d.frames;

  const summary = [
    `${width}×${height}`,
    total ? secs(secondsOf(total, d.fps)) : "auto length",
    movie ? `${d.clips.length} ${d.clips.length === 1 ? "clip" : "clips"}` : "",
    d.sound ? "" : "no sound",
  ].filter(Boolean);

  return (
    <CreateLayout
      composer={
        <div ref={composer} className="flex flex-col gap-5">
          <div className="flex items-center gap-2">
            <ModeSwitch mode={d.mode} onChange={(mode) => d.set({ mode })} />
            <div className="flex-1" />
            <ExamplesMenu kind="video" onPick={(e: Example) => d.example(e)} />
          </div>

          <Field label="Model">
            <ModelPicker choice={choice} />
            <ModelNotice choice={choice} />
          </Field>

          {movie ? (
            <>
              <PromptBox
                label="Story"
                value={d.story}
                onChange={(story) => d.set({ story })}
                enhance="enhance-video"
                rows={3}
                placeholder="What every clip shares: the characters, the place, the look and the sound. “Funny 2D cartoon, bold outlines. A lanky orange fox with a green scarf and a tiny grey squirrel …”"
                hint="Goes in front of every clip's description, so the characters and the look stay the same."
              />
              <div className="flex flex-col gap-1.5">
                <div className="flex min-h-6 items-center gap-2">
                  <span className="text-xs font-semibold tracking-wide text-muted-foreground">Clips</span>
                  <span className="text-xs text-muted-foreground tabular-nums">
                    {d.clips.length} · {total ? `${secs(secondsOf(total, d.fps))} in all` : "length up to LTX"}
                  </span>
                  <div className="flex-1" />
                  <ClearMovie />
                </div>
                <MovieEditor />
              </div>
              <Field label="Clip length" hint="Each clip's length unless it has its own. Longer clips take longer and need more memory.">
                <ClipLength frames={d.clipFrames} fps={d.fps} onChange={(clipFrames) => d.set({ clipFrames })} className="h-9 w-full text-sm" />
              </Field>
            </>
          ) : (
            <>
              <PromptBox
                label="Prompt"
                value={d.prompt}
                onChange={(prompt) => d.set({ prompt })}
                enhance="enhance-video"
                rows={5}
                placeholder="What happens, how the camera moves, and what it sounds like: “Waves crash against black rocks at sunset, slow-motion spray, the roar of the surf.”"
                onKeyDown={(e) => {
                  if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) create();
                }}
              />
              <Field label="First frame">
                <FirstFrame value={d.image} onChange={(image) => d.set({ image })} />
              </Field>
              <Field label="Length">
                <LengthControl frames={d.frames} fps={d.fps} onChange={(frames) => d.set({ frames })} />
              </Field>
            </>
          )}

          <Field label="Size">
            <SizePicker width={width} height={height} base={base} onChange={(s) => d.set(s)} />
          </Field>

          <Advanced id="video" summary={`${d.fps} fps · ${d.seed ? `seed ${d.seed}` : "random seed"}${d.lowRam ? " · low RAM" : ""}`}>
            <div className="grid grid-cols-2 gap-3">
              <Field label="Frame rate">
                <Select value={String(d.fps)} onValueChange={(v) => d.set({ fps: +v })}>
                  <SelectTrigger className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {[24, 25, 30].map((f) => (
                      <SelectItem key={f} value={String(f)}>
                        {f} fps
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </Field>
              <Field label="Seed">
                <div className="flex gap-1">
                  <Input
                    inputMode="numeric"
                    placeholder="random"
                    value={d.seed || ""}
                    onChange={(e) => d.set({ seed: Math.max(0, parseInt(e.target.value.replace(/\D/g, "")) || 0) })}
                  />
                  <Button variant="outline" size="icon" className="size-8 shrink-0" title="A random seed (to reuse it)" onClick={() => d.set({ seed: Math.floor(Math.random() * 1e9) })}>
                    <Dices />
                  </Button>
                </div>
              </Field>
            </div>
            {movie && d.seed > 0 && <p className="-mt-2 text-xs text-muted-foreground">Clip n uses seed + n − 1.</p>}
            <Toggle
              label="Sound"
              hint="LTX-2.5 makes the sound with the picture: voices, ambience, music."
              checked={d.sound}
              onChange={(sound) => d.set({ sound })}
            />
            <Toggle
              label="Low-RAM mode"
              hint="Renders block by block: slower, but gentler on memory and on macOS's GPU watchdog (it is used once by itself if the watchdog stops a render)."
              checked={d.lowRam}
              onChange={(lowRam) => d.set({ lowRam })}
            />
            <RunOn page="video" model={choice.primary} />
          </Advanced>
        </div>
      }
      footer={
        <div className="flex flex-col gap-2">
          <div className={cn("flex items-center gap-1.5 text-xs", problem && macAway ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground")}>
            {macAway && <WifiOff className="size-3.5 shrink-0" />}
            <span className="truncate">{problem || summary.join(" · ")}</span>
          </div>
          <CreateButton count={choice.ids.length} busy={busy} disabled={!!problem} onClick={create} label={movie ? "Create movie" : "Create"} />
        </div>
      }
    >
      <Results arg={arg} onUse={onUse} />
    </CreateLayout>
  );
}

function ModeSwitch({ mode, onChange }: { mode: Mode; onChange: (m: Mode) => void }) {
  return (
    <div className="flex rounded-lg bg-muted p-0.5" role="tablist">
      {(
        [
          ["clip", Video, "Clip"],
          ["movie", Clapperboard, "Movie"],
        ] as const
      ).map(([m, Icon, label]) => (
        <button
          key={m}
          role="tab"
          aria-selected={mode === m}
          onClick={() => onChange(m)}
          className={cn(
            "flex items-center gap-1.5 rounded-md px-3 py-1.5 text-sm font-medium transition-colors",
            mode === m ? "bg-background text-foreground shadow-sm" : "text-muted-foreground hover:text-foreground",
          )}
        >
          <Icon className="size-4" /> {label}
        </button>
      ))}
    </div>
  );
}

function Toggle({ label, hint, checked, onChange }: { label: string; hint: string; checked: boolean; onChange: (v: boolean) => void }) {
  return (
    <label className="flex cursor-pointer items-start gap-3">
      <div className="min-w-0 flex-1">
        <div className="text-sm font-medium">{label}</div>
        <div className="text-xs text-muted-foreground">{hint}</div>
      </div>
      <Switch className="mt-0.5" checked={checked} onCheckedChange={onChange} />
    </label>
  );
}

/** LTX packs are big and gated: say so before Create starts a 30 GB download. */
function ModelNotice({ choice }: { choice: ReturnType<typeof useModelChoice> }) {
  const m = choice.primary;
  if (!choice.models.length) return <p className="text-xs text-muted-foreground">No video models on this Mac.</p>;
  if (!m || m.here) return null;
  return (
    <div className="flex gap-2 rounded-lg border border-amber-500/30 bg-amber-500/10 p-2.5 text-xs">
      {m.gated ? <Lock className="mt-0.5 size-3.5 shrink-0 text-amber-500" /> : <Download className="mt-0.5 size-3.5 shrink-0 text-amber-500" />}
      <div className="text-muted-foreground">
        <span className="font-medium text-foreground">Not downloaded yet ({bytes(m.size)}).</span> The first video downloads it, which takes a while.
        {m.gated && " It is gated: accept the LTX-2 Community License on Hugging Face and add your token first."}{" "}
        <a href="#/models" className="text-primary hover:underline">
          Models →
        </a>
      </div>
    </div>
  );
}

function ClearMovie() {
  const clear = useDraft((s) => s.clearMovie);
  const [sure, setSure] = useState(false);
  useEffect(() => {
    if (!sure) return;
    const t = setTimeout(() => setSure(false), 3000);
    return () => clearTimeout(t);
  }, [sure]);
  return (
    <Button variant="ghost" size="xs" className={cn("text-muted-foreground", sure && "text-destructive")} onClick={() => (sure ? (clear(), setSure(false)) : setSure(true))}>
      {sure ? "Clear the script?" : "New movie"}
    </Button>
  );
}

type Filter = "all" | "starred" | "movies";

const Results = memo(function Results({ arg, onUse }: { arg: string; onUse: (j: Job) => void }) {
  const jobs = useApp((s) => s.jobs);
  const connection = useApp((s) => s.connection);
  // newest first (by time: the server's and a fresh job's timestamps may be written differently)
  const all = useMemo(() => jobsOf(jobs, "video").sort((a, b) => Date.parse(b.created) - Date.parse(a.created)), [jobs]);
  const [filter, setFilter] = useState<Filter>(() => local.get<Filter>("video.filter", "all"));
  const shown = useMemo(() => all.filter((j) => (filter === "starred" ? j.starred : filter === "movies" ? isMovie(j) : true)), [all, filter]);
  // a video asked for by the URL: scroll to it and mark it for a moment
  const [faded, setFaded] = useState("");
  const ready = !!jobs[arg];
  const highlight = ready && faded !== arg ? arg : "";
  useEffect(() => {
    if (!ready) return;
    requestAnimationFrame(() => document.getElementById(`video-${arg}`)?.scrollIntoView({ behavior: "smooth", block: "center" }));
    const t = setTimeout(() => setFaded(arg), 2500);
    return () => clearTimeout(t);
  }, [arg, ready]);

  const counts = { all: all.length, starred: all.filter((j) => j.starred).length, movies: all.filter(isMovie).length };

  return (
    <div id="video-results" className="mx-auto max-w-[1600px] scroll-mt-4">
      <SectionHeader title="Videos">
        <ToggleGroup
          type="single"
          size="sm"
          variant="outline"
          spacing={0}
          value={filter}
          onValueChange={(v) => v && (setFilter(v as Filter), local.set("video.filter", v))}
        >
          <ToggleGroupItem value="all" className="data-[state=on]:text-foreground">
            All <Count n={counts.all} />
          </ToggleGroupItem>
          <ToggleGroupItem value="starred" className="data-[state=on]:text-foreground">
            <Star className="size-3.5" /> Starred <Count n={counts.starred} />
          </ToggleGroupItem>
          <ToggleGroupItem value="movies" className="data-[state=on]:text-foreground">
            <Clapperboard className="size-3.5" /> Movies <Count n={counts.movies} />
          </ToggleGroupItem>
        </ToggleGroup>
      </SectionHeader>

      {shown.length ? (
        <div className="grid grid-cols-[repeat(auto-fill,minmax(min(100%,360px),1fr))] items-start gap-5">
          {shown.map((j) => (
            <VideoCard key={j.id} job={j} onUse={onUse} highlight={highlight === j.id} />
          ))}
        </div>
      ) : connection === "connecting" ? null : filter === "starred" ? (
        <Empty icon={<Star />} title="Nothing starred yet">
          Press ☆ on a video you want to keep handy.
        </Empty>
      ) : filter === "movies" ? (
        <Empty icon={<Clapperboard />} title="No movies yet">
          Switch the composer to <b>Movie</b>, write a story and a few clips, and press Create movie. Each clip continues from the last frame of the one before, or
          starts a new scene.
        </Empty>
      ) : (
        <Empty icon={<Film />} title="No videos yet">
          Describe a shot on the left and press Create — or pick one of the Examples. Clips take a few minutes; they appear here as they render.
        </Empty>
      )}
    </div>
  );
});

const Count = ({ n }: { n: number }) => (n ? <span className="text-muted-foreground tabular-nums">{n}</span> : null);
