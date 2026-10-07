import { AlertTriangle, ArrowLeft, AudioLines, Cpu, FileAudio, Globe, Loader2, MoreHorizontal, Palette, Pause, Pencil, Play, Plus, ShieldCheck, Trash2, Upload } from "lucide-react";
import { useMemo, useRef, useState, type DragEvent } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { Style, StyleSettings } from "@/api/types";
import { Advanced, Empty, Field } from "@/components/kit/Layout";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Progress } from "@/components/ui/progress";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Slider } from "@/components/ui/slider";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { navigate } from "@/lib/route";
import { cn } from "@/lib/utils";
import { useManyMacs } from "@/store/app";
import { AddFromHubDialog, CommunityInfo, CommunityList } from "./CommunityStyles";
import { NameDialog } from "./Dialogs";
import { LossChart } from "./LossChart";
import { useStudio } from "./state";
import { about, clock, isBusy, STATUS_LABEL, useStyles, useStylesLive } from "./styles";

/**
 * Styles: train a song model on your own tracks (a LoRA) and use the result as a style when making songs.
 * `id` is the open style (#/music/styles/<id>).
 */
export function StylesPage({ id }: { id: string }) {
  useStylesLive();
  const { styles, loaded, error } = useStyles();
  const [creating, setCreating] = useState(false);
  const [fromHub, setFromHub] = useState(false);
  const open = styles.find((s) => s.id === id) ?? (id ? undefined : styles[0]);
  return (
    <div className="mx-auto flex w-full max-w-3xl flex-col gap-5 p-6 max-sm:p-4">
      <div className="flex flex-wrap items-center gap-3">
        <Button variant="ghost" size="icon-sm" onClick={() => navigate("music")} title="Back to the songs">
          <ArrowLeft />
        </Button>
        <h1 className="text-xl font-semibold tracking-tight">Your styles</h1>
        <div className="flex-1" />
        <Button size="sm" variant="outline" onClick={() => setFromHub(true)} title="A LoRA from Hugging Face (not trained here)">
          <Globe /> Add from Hugging Face
        </Button>
        <Button size="sm" onClick={() => setCreating(true)}>
          <Plus /> New style
        </Button>
      </div>
      <p className="-mt-2 text-sm text-muted-foreground">
        Teach a song model the sound of your own productions: upload a few tracks, train a style (a LoRA), then pick it when you make a song. Or add a community style: a LoRA someone shared on Hugging Face.
      </p>

      {!loaded ? (
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <Loader2 className="size-4 animate-spin" /> Loading styles …
        </div>
      ) : error && !styles.length ? (
        <Empty icon={<AlertTriangle />} title="Styles could not be loaded">
          {error}
        </Empty>
      ) : !styles.length ? (
        <Empty icon={<Palette />} title="No styles yet">
          Make one with “New style”, drop in three or more tracks you made yourself (or are licensed to use), and train it. It takes about half an hour on the Mac. Or add a community style below.
        </Empty>
      ) : (
        <>
          <div className="flex flex-wrap gap-2">
            {styles.map((s) => (
              <button
                key={s.id}
                onClick={() => navigate("music", "styles/" + s.id)}
                className={cn(
                  "flex min-w-0 max-w-64 flex-col items-start gap-0.5 rounded-lg border px-3 py-2 text-left transition-colors hover:border-primary/50",
                  open?.id === s.id && "border-primary bg-primary/5",
                )}
              >
                <span className="flex w-full items-center gap-1.5 text-sm font-medium">
                  <span className="truncate">{s.name}</span>
                  {isBusy(s) && <Loader2 className="size-3.5 shrink-0 animate-spin text-primary" />}
                </span>
                <span className="truncate text-xs text-muted-foreground">
                  {s.source ? "Community · Hugging Face" : STATUS_LABEL[s.status]}
                  {s.status === "training" && s.progress.total ? ` · ${Math.round((100 * s.progress.step) / s.progress.total)} %` : ""}
                </span>
              </button>
            ))}
          </div>
          {open ? <StyleDetail key={open.id} style={open} /> : <Empty title="This style is gone">It was deleted, or its Mac is offline.</Empty>}
        </>
      )}
      {loaded && <CommunityList />}
      <NewStyleDialog open={creating} onOpenChange={setCreating} />
      <AddFromHubDialog open={fromHub} onOpenChange={setFromHub} />
    </div>
  );
}

function NewStyleDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const models = useStyles((s) => s.models);
  const put = useStyles((s) => s.put);
  const [name, setName] = useState("");
  const [base, setBase] = useState("");
  const [busy, setBusy] = useState(false);
  // Stable Audio 3 Medium first (the best songs), then any model that is here
  const model = base || models.find((m) => m.here && m.id === "stable-audio-3-medium")?.id || models.find((m) => m.here)?.id || models[0]?.id || "";
  const picked = models.find((m) => m.id === model);
  const create = async () => {
    setBusy(true);
    try {
      const s = await api.createStyle(name.trim(), model);
      put(s);
      onOpenChange(false);
      setName("");
      navigate("music", "styles/" + s.id);
    } catch (e) {
      toast.error("Could not make the style", { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (name.trim() && model) void create();
          }}
        >
          <DialogHeader>
            <DialogTitle>New style</DialogTitle>
            <DialogDescription>A style is trained for one song model and works with that model only.</DialogDescription>
          </DialogHeader>
          <Field label="Name">
            <Input autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="My trance sound" />
          </Field>
          <Field
            label="Song model"
            hint={
              picked?.described
                ? "Stable Audio 3 makes instrumental music; it learns from your tracks and a description of their sound (about 1 s a step)."
                : "ACE-Step listens to each track and writes its captions itself; the 2B turbo takes about 2 s a step, the XL twice that."
            }
          >
            <Select value={model} onValueChange={setBase}>
              <SelectTrigger className="w-full">
                <SelectValue placeholder="No model can learn styles" />
              </SelectTrigger>
              <SelectContent>
                {models.map((m) => (
                  <SelectItem key={m.id} value={m.id}>
                    {m.name}
                    {!m.here && " (downloads first)"}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy || !name.trim() || !model}>
              {busy && <Loader2 className="animate-spin" />} Create
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** One style: its tracks, training (settings, progress, loss, samples) and how to use it. */
function StyleDetail({ style: s }: { style: Style }) {
  const put = useStyles((st) => st.put);
  const drop = useStyles((st) => st.drop);
  const models = useStyles((st) => st.models);
  const many = useManyMacs();
  const [rename, setRename] = useState(false);
  const [remove, setRemove] = useState(false);
  const busy = isBusy(s);
  const model = models.find((m) => m.id === s.baseModel)?.name ?? s.baseModel;
  const described = !!models.find((m) => m.id === s.baseModel)?.described;

  const act = async (what: string, f: () => Promise<unknown>) => {
    try {
      await f();
    } catch (e) {
      toast.error(what, { description: errorText(e) });
    }
  };

  return (
    <div className="flex flex-col gap-5 rounded-xl border bg-card/50 p-5 max-sm:p-4">
      <div className="flex flex-wrap items-start gap-3">
        <div className="min-w-0 flex-1">
          <h2 className="truncate text-lg font-semibold">{s.name}</h2>
          <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
            <span>{model}</span>
            <span>·</span>
            {s.source ? <span className="font-medium text-emerald-500">Community style</span> : <StatusBadge style={s} />}
            {many && s.worker && (
              <>
                <span>·</span>
                <span>on {s.worker.name}</span>
              </>
            )}
            {s.weights && (
              <>
                <span>·</span>
                <span title="Added to the style text of every song that uses this style">
                  trigger <code className="rounded bg-muted px-1">{s.trigger}</code>
                </span>
              </>
            )}
          </div>
        </div>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-sm" title="Style">
              <MoreHorizontal />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onSelect={() => setRename(true)}>
              <Pencil /> Rename
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem disabled={busy} onSelect={() => setRemove(true)} className="text-destructive">
              <Trash2 /> Delete style
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      {s.source ? (
        <CommunityInfo style={s} />
      ) : (
        <>
          {described && <Description style={s} />}
          <Tracks style={s} described={described} />
          <Training style={s} described={described} />
        </>
      )}
      {s.weights && <UseIt style={s} />}

      <NameDialog
        open={rename}
        onOpenChange={setRename}
        title="Rename style"
        initial={s.name}
        ok="Rename"
        onSubmit={(name) => act("Rename", async () => put(await api.updateStyle(s.id, { name })))}
      />
      <AlertDialog open={remove} onOpenChange={setRemove}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete “{s.name}”?</AlertDialogTitle>
            <AlertDialogDescription>
              {s.source
                ? "The style is removed from the Mac (the LoRA's downloaded weights stay in the model store). Songs made with it stay."
                : "Its tracks, samples and trained weights are removed from the Mac. Songs made with it stay."}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep</AlertDialogCancel>
            <AlertDialogAction
              onClick={() =>
                act("Delete", async () => {
                  await api.deleteStyle(s.id);
                  drop(s.id);
                  navigate("music", "styles");
                })
              }
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function StatusBadge({ style: s }: { style: Style }) {
  const tone = s.status === "trained" ? "text-emerald-500" : s.status === "failed" ? "text-destructive" : isBusy(s) ? "text-primary" : "";
  return <span className={cn("font-medium", tone)}>{STATUS_LABEL[s.status]}</span>;
}

/** What the tracks sound like, in tags (the caption of every track for Stable Audio 3, which has no captioner). */
function Description({ style: s }: { style: Style }) {
  const put = useStyles((st) => st.put);
  const [text, setText] = useState(s.description ?? "");
  const save = async () => {
    if (text.trim() === (s.description ?? "")) return;
    try {
      put(await api.updateStyle(s.id, { description: text }));
    } catch (e) {
      toast.error("Description", { description: errorText(e) });
    }
  };
  return (
    <Field
      label="What it sounds like"
      hint="Tags in the words you would use for a song: genre, instruments, mood, production. Every track is captioned with this (and its measured tempo) — so describe what all of them share."
    >
      <Textarea
        rows={2}
        value={text}
        disabled={isBusy(s)}
        onChange={(e) => setText(e.target.value)}
        onBlur={save}
        placeholder="uplifting trance, supersaw lead, rolling offbeat bass, big reverb, euphoric breakdown"
        className="resize-none"
      />
    </Field>
  );
}

// ---------- tracks ----------

function Tracks({ style: s, described }: { style: Style; described: boolean }) {
  const put = useStyles((st) => st.put);
  const input = useRef<HTMLInputElement>(null);
  const [over, setOver] = useState(false);
  const [uploading, setUploading] = useState(0);
  const [vocals, setVocals] = useState(false);
  const locked = isBusy(s);

  const add = async (files: File[]) => {
    files = files.filter((f) => f.type.startsWith("audio/") || /\.(wav|mp3|flac|m4a|aac|ogg|aiff?)$/i.test(f.name));
    if (!files.length) return toast("Drop audio files (wav, mp3, flac, m4a, aiff).");
    setUploading(files.length);
    try {
      // one at a time: a failure names its file, the others are kept
      for (const f of files) {
        try {
          put(await api.addStyleTracks(s.id, [f], vocals));
        } catch (e) {
          toast.error(`“${f.name}” was not added`, { description: errorText(e) });
        }
        setUploading((n) => n - 1);
      }
    } finally {
      setUploading(0);
    }
  };
  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    setOver(false);
    if (!locked) void add([...e.dataTransfer.files]);
  };

  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-baseline gap-2">
        <h3 className="text-sm font-semibold">Tracks</h3>
        {s.tracks.length > 0 && (
          <span className="text-xs text-muted-foreground">
            {s.tracks.length} · {clock(s.seconds)} · {described ? `${s.settings.segmentSeconds} s excerpts` : `${s.segments} segments of ${s.settings.segmentSeconds} s`}
          </span>
        )}
      </div>
      <div className="flex items-start gap-2 rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs">
        <ShieldCheck className="mt-0.5 size-3.5 shrink-0 text-amber-500" />
        <span>
          Only upload music you own or are licensed to use for training — your own productions, or tracks whose rights holder agreed. The tracks stay on this Mac.
        </span>
      </div>
      <div
        onDragOver={(e) => {
          if (locked || !e.dataTransfer.types.includes("Files")) return;
          e.preventDefault();
          setOver(true);
        }}
        onDragLeave={() => setOver(false)}
        onDrop={onDrop}
        className={cn(
          "flex flex-col items-center gap-2 rounded-xl border border-dashed px-4 py-6 text-center transition-colors",
          over && "border-primary bg-primary/5",
          locked && "opacity-60",
        )}
      >
        {uploading ? (
          <span className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" /> Converting {uploading} {uploading === 1 ? "track" : "tracks"} …
          </span>
        ) : (
          <>
            <Upload className="size-6 text-muted-foreground" />
            <div className="text-sm">
              {locked ? "Tracks can't change while the style trains." : "Drop tracks here, or"}{" "}
              {!locked && (
                <button className="text-primary hover:underline" onClick={() => input.current?.click()}>
                  choose files
                </button>
              )}
            </div>
            <div className="text-xs text-muted-foreground">
              3–20 tracks of one sound work best · up to 15 min each · {described ? `trained on random ${s.settings.segmentSeconds} s excerpts` : `cut into ${s.settings.segmentSeconds} s segments for training`}
            </div>
          </>
        )}
        {described ? (
          <div className="mt-1 text-xs text-muted-foreground">Stable Audio 3 learns instrumental music: vocals in a track are learned as sound, not words.</div>
        ) : (
          <label className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
            <Switch checked={vocals} onCheckedChange={setVocals} disabled={locked} /> These tracks have vocals (the model transcribes them; off: instrumental)
          </label>
        )}
        <input
          ref={input}
          type="file"
          accept="audio/*,.wav,.mp3,.flac,.m4a,.aif,.aiff"
          multiple
          hidden
          onChange={(e) => {
            const files = [...(e.target.files ?? [])];
            e.target.value = "";
            void add(files);
          }}
        />
      </div>
      {s.tracks.length > 0 && (
        <ul className="flex flex-col divide-y rounded-lg border">
          {s.tracks.map((t) => (
            <li key={t.id} className="flex items-center gap-3 px-3 py-2 text-sm">
              <FileAudio className="size-4 shrink-0 text-muted-foreground" />
              <span className="min-w-0 flex-1 truncate">{t.name}</span>
              {t.vocals && <span className="text-xs text-muted-foreground">vocals</span>}
              {t.bpm ? <span className="text-xs text-muted-foreground tabular-nums">{Math.round(t.bpm)} bpm</span> : null}
              <span className="text-xs text-muted-foreground tabular-nums">
                {clock(t.seconds)} · {t.segments} seg.
              </span>
              <MiniPlayer url={t.url} />
              <Button
                variant="ghost"
                size="icon-xs"
                title="Remove the track"
                disabled={locked}
                onClick={async () => {
                  try {
                    put(await api.deleteStyleTrack(s.id, t.id));
                  } catch (e) {
                    toast.error("Remove", { description: errorText(e) });
                  }
                }}
              >
                <Trash2 />
              </Button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/** Play / pause one file (a track, a sample) without taking over the app's player. */
function MiniPlayer({ url, label }: { url: string; label?: string }) {
  const ref = useRef<HTMLAudioElement>(null);
  const [playing, setPlaying] = useState(false);
  return (
    <>
      <Button
        variant={label ? "outline" : "ghost"}
        size={label ? "sm" : "icon-xs"}
        title={playing ? "Pause" : "Play"}
        onClick={() => {
          const a = ref.current;
          if (!a) return;
          if (a.paused) {
            document.querySelectorAll<HTMLAudioElement>("audio[data-style-player]").forEach((o) => o !== a && o.pause());
            void a.play();
          } else a.pause();
        }}
      >
        {playing ? <Pause /> : <Play />}
        {label}
      </Button>
      <audio ref={ref} data-style-player src={url} preload="none" onPlay={() => setPlaying(true)} onPause={() => setPlaying(false)} onEnded={() => setPlaying(false)} />
    </>
  );
}

// ---------- training ----------

const STEP_CHOICES = [300, 500, 1000, 1500, 2000, 3000];

function Training({ style: s, described }: { style: Style; described: boolean }) {
  const put = useStyles((st) => st.put);
  const models = useStyles((st) => st.models);
  const [busy, setBusy] = useState(false);
  const [fresh, setFresh] = useState(false);
  const set = s.settings;
  const running = isBusy(s);
  const p = s.progress;
  const canResume = s.status === "paused" || (s.status === "failed" && (p.checkpoint ?? 0) > 0);
  const job = s.running;
  const stepSeconds = p.secondsPerStep || models.find((m) => m.id === s.baseModel)?.stepSeconds || 2.5;
  const losses = s.losses;
  const defaults = models.find((m) => m.id === s.baseModel)?.defaults;
  const alphaPerRank = defaults ? defaults.alpha / defaults.rank : 2;
  const needsDescription = described && !s.description?.trim();

  const change = async (patch: Partial<StyleSettings>) => {
    try {
      put(await api.updateStyle(s.id, { settings: { ...set, ...patch } }));
    } catch (e) {
      toast.error("Settings", { description: errorText(e) });
    }
  };
  const train = async (opts: { fresh?: boolean } = {}) => {
    setBusy(true);
    try {
      const r = await api.trainStyle(s.id, opts);
      put(r.style);
      void useStyles.getState().refresh();
    } catch (e) {
      toast.error("Could not start the training", { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };
  const stop = async () => {
    if (!s.jobId) return;
    try {
      await api.cancel(s.jobId);
      toast("Stopping at the next step — the checkpoint is kept, Resume continues from it.");
      setTimeout(() => void useStyles.getState().refresh(), 1500);
    } catch (e) {
      toast.error("Stop", { description: errorText(e) });
    }
  };

  return (
    <section className="flex flex-col gap-3">
      <h3 className="text-sm font-semibold">Training</h3>

      {!running && (
        <>
          <Field
            label={
              <>
                Steps <span className="ml-1 font-normal text-foreground tabular-nums">{set.steps}</span>
              </>
            }
            hint={`More steps learn the sound more closely (and risk copying it). ${s.segments ? `About ${Math.round(set.steps / Math.max(1, s.segments))} passes over your ${s.segments} segments.` : ""}`}
          >
            <div className="flex flex-wrap gap-1.5">
              {STEP_CHOICES.map((n) => (
                <Button key={n} size="xs" variant={set.steps === n ? "secondary" : "outline"} onClick={() => change({ steps: n })}>
                  {n}
                </Button>
              ))}
            </div>
          </Field>
          <Advanced id="music.styles.train" summary={`rank ${set.rank} · lr ${set.lr} · ${set.segmentSeconds} s segments`}>
            <div className="grid grid-cols-2 gap-3 max-sm:grid-cols-1">
              <NumberField label="LoRA rank" value={set.rank} onCommit={(v) => change({ rank: v, alpha: alphaPerRank * v })} hint={alphaPerRank === 1 ? "Alpha is the rank." : "Alpha is twice the rank."} />
              <NumberField label="Learning rate" value={set.lr} step="0.00001" onCommit={(v) => change({ lr: v })} />
              <NumberField
                label={described ? "Excerpt length (s)" : "Segment length (s)"}
                value={set.segmentSeconds}
                onCommit={(v) => change({ segmentSeconds: v })}
                hint="10–120 s; longer ones make slower steps."
              />
              <NumberField label="Sample every (steps)" value={set.sampleEvery} onCommit={(v) => change({ sampleEvery: v })} hint="0: no samples." />
              <NumberField label="Checkpoint every (steps)" value={set.checkpointEvery} onCommit={(v) => change({ checkpointEvery: v })} />
              <NumberField label="Seed" value={set.seed} onCommit={(v) => change({ seed: v })} />
            </div>
            <Field label="Sample prompt" hint="What the samples ask for (empty: what the model heard in your first segment).">
              <Input defaultValue={set.samplePrompt ?? ""} placeholder="from the captions" onBlur={(e) => e.target.value !== (set.samplePrompt ?? "") && change({ samplePrompt: e.target.value })} />
            </Field>
          </Advanced>
          <div className="flex flex-wrap items-center gap-2">
            {canResume && !fresh ? (
              <Button disabled={busy} onClick={() => train()}>
                {busy ? <Loader2 className="animate-spin" /> : <Play />} Resume from step {p.checkpoint}
              </Button>
            ) : (
              <Button disabled={busy || !s.tracks.length || needsDescription} onClick={() => train({ fresh: s.status === "trained" || fresh })}>
                {busy ? <Loader2 className="animate-spin" /> : <Cpu />} {s.status === "trained" ? "Train again" : "Train style"}
              </Button>
            )}
            {canResume && (
              <label className="flex items-center gap-2 text-xs text-muted-foreground">
                <Switch checked={fresh} onCheckedChange={setFresh} /> Start over
              </label>
            )}
            <span className="text-xs text-muted-foreground">
              {!s.tracks.length ? "Add tracks first." : needsDescription ? "Describe the sound first." : `${about(s.estimate)} on the Mac`}
              {s.tracks.length > 0 && !needsDescription && ` (~${stepSeconds.toFixed(1)} s a step)`}
            </span>
          </div>
          <p className="text-xs text-muted-foreground">
            Training takes the GPU: other jobs on this Mac wait in the queue until it is done or you pause it. A pause keeps the last checkpoint.
          </p>
        </>
      )}

      {running && (
        <div className="flex flex-col gap-2 rounded-lg border border-primary/30 bg-primary/5 p-3">
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <Loader2 className="size-4 animate-spin text-primary" />
            <span className="font-medium">{s.status === "queued" ? "Waiting for the GPU (other jobs first)" : p.phase ? capitalize(p.phase) : "Training"}</span>
            <div className="flex-1" />
            <Button size="sm" variant="outline" onClick={stop}>
              <Pause /> {s.status === "queued" ? "Cancel" : "Pause"}
            </Button>
          </div>
          {s.status === "training" && p.total > 0 && (
            <>
              <Progress value={(100 * p.step) / p.total} className="h-1.5" />
              <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground tabular-nums">
                <span>
                  step {p.step} / {p.total}
                </span>
                {p.loss != null && p.step > 0 && <span>loss {p.loss.toFixed(3)}</span>}
                {p.secondsPerStep ? <span>{p.secondsPerStep.toFixed(1)} s a step</span> : null}
                {p.eta ? <span>{about(p.eta)} left</span> : null}
                {(p.checkpoint ?? 0) > 0 && <span>checkpoint at {p.checkpoint}</span>}
              </div>
            </>
          )}
          <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <AlertTriangle className="size-3.5 text-amber-500" /> The GPU is busy training: other jobs on {s.worker?.name ?? "this Mac"} wait until it finishes or is paused.
          </p>
          {job?.state === "running" && job.status && <p className="truncate text-xs text-muted-foreground">{job.status}</p>}
        </div>
      )}

      {s.status === "failed" && s.error && (
        <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-destructive" />
          <div className="min-w-0 break-words">{s.error}</div>
        </div>
      )}

      {losses.length > 1 && <LossChart points={losses} total={p.total || set.steps} />}

      {s.samples.length > 0 && (
        <div className="flex flex-col gap-2">
          <div className="flex items-baseline gap-2">
            <h4 className="text-xs font-semibold tracking-wide text-muted-foreground">SAMPLES</h4>
            <span className="text-xs text-muted-foreground">short songs made with the style so far</span>
          </div>
          <div className="flex flex-wrap gap-2">
            {s.samples.map((sm) => (
              <MiniPlayer key={sm.step} url={sm.url} label={sm.step === 0 ? "before" : sm.step >= (p.total || set.steps) ? `final (${sm.step})` : `step ${sm.step}`} />
            ))}
          </div>
        </div>
      )}

      {s.captions && Object.keys(s.captions).length > 0 && (
        <Advanced id="music.styles.captions" title="What the model heard" summary={`${Object.keys(s.captions).length} segments`}>
          <ul className="flex flex-col gap-2 text-xs">
            {Object.entries(s.captions).map(([seg, c]) => (
              <li key={seg}>
                <span className="font-mono text-muted-foreground">{seg.replace(/\.wav$/, "")}</span> — {c || "(no caption)"}
              </li>
            ))}
          </ul>
        </Advanced>
      )}
    </section>
  );
}

const capitalize = (t: string) => t.charAt(0).toUpperCase() + t.slice(1);

function NumberField({ label, value, onCommit, hint, step }: { label: string; value: number; onCommit: (v: number) => void; hint?: string; step?: string }) {
  return (
    <Field label={label} hint={hint}>
      <Input
        type="number"
        step={step}
        defaultValue={value}
        key={value}
        onBlur={(e) => {
          const v = Number(e.target.value);
          if (isFinite(v) && v !== value) onCommit(v);
        }}
      />
    </Field>
  );
}

// ---------- using it ----------

function UseIt({ style: s }: { style: Style }) {
  const put = useStyles((st) => st.put);
  const [strength, setStrength] = useState(s.strength);
  const models = useStyles((st) => st.models);
  const model = useMemo(() => models.find((m) => m.id === s.baseModel)?.name ?? s.baseModel, [models, s.baseModel]);
  return (
    <section className="flex flex-col gap-3 rounded-lg border p-3">
      <div className="flex items-center gap-2">
        <AudioLines className="size-4 text-primary" />
        <h3 className="text-sm font-semibold">Use it</h3>
      </div>
      <p className="text-xs text-muted-foreground">
        In “New song”, pick {model} and this style. Its trigger word <code className="rounded bg-muted px-1">{s.trigger}</code> is added to the style text for you.
      </p>
      <Field
        label={
          <>
            Default strength <span className="ml-1 font-normal text-foreground tabular-nums">{strength.toFixed(2)}</span>
          </>
        }
        hint="1 as trained; lower blends it with the model's own sound, above 1 exaggerates it."
      >
        <Slider
          min={0}
          max={2}
          step={0.05}
          value={[strength]}
          onValueChange={([v]) => setStrength(v)}
          onValueCommit={async ([v]) => {
            try {
              put(await api.updateStyle(s.id, { strength: v }));
            } catch (e) {
              toast.error("Strength", { description: errorText(e) });
            }
          }}
          className="py-2"
        />
      </Field>
      <div>
        <Button
          size="sm"
          variant="outline"
          onClick={() => {
            useStudio.getState().load({ trainedStyle: s.id, styleStrength: s.strength }, s.baseModel);
            navigate("music");
          }}
        >
          <Plus /> Make a song with it
        </Button>
      </div>
    </section>
  );
}
