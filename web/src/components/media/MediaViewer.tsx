import { Dialog as DialogPrimitive } from "radix-ui";
import { Check, ChevronLeft, ChevronRight, Copy, Download, Info, Library, RotateCcw, Trash2, Wand2, X } from "lucide-react";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import type { Job } from "@/api/types";
import { DeleteJob, StarButton } from "@/components/kit/JobBits";
import { Button } from "@/components/ui/button";
import { Dialog, DialogPortal } from "@/components/ui/dialog";
import { ago, duration, took } from "@/lib/format";
import { navigate } from "@/lib/route";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";
import { dims, download, mainFile, promptOf, secondsOf, seedOf, stepsOf, typeLabel, videoSeconds } from "./media";
import { ZoomImage } from "./ZoomImage";

/**
 * The full-screen view of images and videos: the picture (zoom, pan) or the video (with controls), ← → through
 * `items`, and the details at the side. `id` is the one shown (null: closed). The job is read live from the store,
 * so a star or a delete shows at once.
 */
export function MediaViewer({
  items,
  id,
  onNavigate,
  onClose,
  onUseAgain,
  canUseAgain,
  useAgainLabel = "Use again",
  inLibrary,
}: {
  items: Job[];
  id: string | null;
  onNavigate: (id: string) => void;
  onClose: () => void;
  /** Puts the settings back into a composer; without it there is no "Use again". */
  onUseAgain?: (j: Job) => void;
  /** Which jobs "Use again" is for (all, without it). */
  canUseAgain?: (j: Job) => boolean;
  useAgainLabel?: string;
  /** In the library already: no "Open in Library". */
  inLibrary?: boolean;
}) {
  const live = useApp((s) => (id ? s.jobs[id] : undefined));
  const [panel, setPanel] = useState(() => local.get("viewer.details", true));
  const [scale, setScale] = useState(1);
  const [resetKey, setResetKey] = useState(0);
  const content = useRef<HTMLDivElement>(null);

  // the list without what has no file yet, and with the shown job even when a filter hides it
  const list = useMemo(() => {
    const l = items.filter((j) => mainFile(j));
    return live && !l.some((j) => j.id === live.id) ? [live, ...l] : l;
  }, [items, live]);
  const at = live ? list.findIndex((j) => j.id === live.id) : -1;
  const prev = at > 0 ? list[at - 1] : undefined;
  const next = at >= 0 && at < list.length - 1 ? list[at + 1] : undefined;
  const go = (dir: -1 | 1) => {
    const j = dir < 0 ? prev : next;
    if (j) onNavigate(j.id);
  };

  // the neighbours' pictures load while this one is looked at
  useEffect(() => {
    for (const j of [prev, next]) if (j?.kind === "image" && mainFile(j)) new Image().src = mainFile(j)!;
  }, [prev, next]);

  const open = !!live && !!mainFile(live);
  const file = live ? mainFile(live) : undefined;

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogPortal>
        <DialogPrimitive.Overlay className="fixed inset-0 z-50 bg-black/90 backdrop-blur-sm data-open:animate-in data-open:fade-in-0 data-closed:animate-out data-closed:fade-out-0" />
        <DialogPrimitive.Content
          ref={content}
          aria-describedby={undefined}
          // focus the viewer itself (for ← →), not its first button
          onOpenAutoFocus={(e) => (e.preventDefault(), content.current?.focus())}
          className="fixed inset-0 z-50 flex outline-none max-md:flex-col data-open:animate-in data-open:fade-in-0 data-closed:animate-out data-closed:fade-out-0"
          onKeyDown={(e) => {
            // only keys meant for the viewer (not for a dialog on top of it, nor for a text field)
            if (!content.current?.contains(e.target as Node) || (e.target as HTMLElement).closest("input, textarea, video")) return;
            if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
            e.preventDefault();
            go(e.key === "ArrowLeft" ? -1 : 1);
          }}
        >
          {live && file && (
            <>
              <DialogPrimitive.Title className="sr-only">{typeLabel(live)}</DialogPrimitive.Title>
              <div className="relative min-h-0 min-w-0 flex-1 bg-black">
                {live.kind === "video" || live.kind === "avatar" ? (
                  <div className="flex size-full items-center justify-center p-4 pt-14 max-md:pt-14">
                    <video key={live.id} src={file} controls autoPlay playsInline loop className="max-h-full max-w-full rounded-md" />
                  </div>
                ) : (
                  <ZoomImage
                    src={file}
                    alt={promptOf(live)}
                    onSwipe={go}
                    onZoom={setScale}
                    resetKey={resetKey}
                    className="max-h-[calc(100%-7rem)] max-w-[calc(100%-2rem)] max-md:max-h-[calc(100%-6rem)] max-md:max-w-full"
                  />
                )}

                {/* top bar */}
                <div className="pointer-events-none absolute inset-x-0 top-0 flex items-center gap-2 bg-gradient-to-b from-black/60 to-transparent p-2.5 text-white">
                  <span className="px-2 text-sm text-white/70 tabular-nums">{list.length > 1 && at >= 0 ? `${at + 1} / ${list.length}` : ""}</span>
                  <div className="flex-1" />
                  <div className="pointer-events-auto flex items-center gap-1 [&_button]:text-white/85 [&_button:hover]:bg-white/15 [&_button:hover]:text-white">
                    {scale > 1.01 && (
                      <Button variant="ghost" size="sm" title="Fit to the screen" onClick={() => setResetKey((k) => k + 1)}>
                        <RotateCcw /> {Math.round(scale * 100)} %
                      </Button>
                    )}
                    <Button variant="ghost" size="icon" title="Download" onClick={() => download(file)}>
                      <Download />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      title={panel ? "Hide details" : "Show details"}
                      className={cn(panel && "bg-white/10")}
                      onClick={() => (setPanel(!panel), local.set("viewer.details", !panel))}
                    >
                      <Info />
                    </Button>
                    <DialogPrimitive.Close asChild>
                      <Button variant="ghost" size="icon" title="Close (Esc)">
                        <X />
                      </Button>
                    </DialogPrimitive.Close>
                  </div>
                </div>

                {prev && <NavButton side="left" onClick={() => go(-1)} />}
                {next && <NavButton side="right" onClick={() => go(1)} />}
              </div>

              {panel && (
                <Details
                  job={live}
                  onUseAgain={onUseAgain && (!canUseAgain || canUseAgain(live)) ? () => (onClose(), onUseAgain(live)) : undefined}
                  useAgainLabel={useAgainLabel}
                  inLibrary={inLibrary}
                  afterDelete={() => (next ? onNavigate(next.id) : prev ? onNavigate(prev.id) : onClose())}
                />
              )}
            </>
          )}
        </DialogPrimitive.Content>
      </DialogPortal>
    </Dialog>
  );
}

function NavButton({ side, onClick }: { side: "left" | "right"; onClick: () => void }) {
  const Icon = side === "left" ? ChevronLeft : ChevronRight;
  return (
    <button
      onClick={onClick}
      title={side === "left" ? "Previous (←)" : "Next (→)"}
      className={cn(
        "absolute top-1/2 grid size-11 -translate-y-1/2 place-items-center rounded-full bg-black/40 text-white/80 backdrop-blur-sm transition hover:bg-black/70 hover:text-white max-md:size-9",
        side === "left" ? "left-3" : "right-3",
      )}
    >
      <Icon className="size-6" />
    </button>
  );
}

function Details({ job: j, onUseAgain, useAgainLabel, inLibrary, afterDelete }: { job: Job; onUseAgain?: () => void; useAgainLabel: string; inLibrary?: boolean; afterDelete: () => void }) {
  const prompt = promptOf(j);
  const { w, h } = dims(j);
  const seed = seedOf(j);
  const steps = stepsOf(j);
  const t = took(j);
  const work = secondsOf(j);
  const file = mainFile(j)!;
  const created = new Date(j.created);
  const refs = j.request.images ?? [];

  return (
    <aside className="flex w-[360px] shrink-0 flex-col overflow-y-auto border-l bg-background text-foreground max-md:max-h-[42vh] max-md:w-full max-md:border-t max-md:border-l-0">
      <div className="flex items-start gap-2 p-5 pb-3">
        <div className="min-w-0 flex-1">
          <div className="text-xs font-medium tracking-wide text-muted-foreground uppercase">{typeLabel(j)}</div>
          <div className="mt-0.5 text-sm text-muted-foreground" title={created.toLocaleString()}>
            {ago(j.created)}
          </div>
        </div>
        <StarButton job={j} className="size-8" />
      </div>

      {j.kind === "avatar" && j.request.text && j.request.prompt && (
        <Section label="Says" action={<CopyButton text={j.request.text} />}>
          <p className="max-h-60 overflow-y-auto text-sm leading-relaxed whitespace-pre-wrap">{j.request.text}</p>
        </Section>
      )}

      {prompt && (
        <Section label={j.kind === "avatar" ? (j.request.prompt ? "Scene" : "Says") : "Prompt"} action={<CopyButton text={prompt} />}>
          <p className="max-h-60 overflow-y-auto text-sm leading-relaxed whitespace-pre-wrap">{prompt}</p>
        </Section>
      )}

      {j.request.script && j.request.script.length > 1 && (
        <Section label={`${j.request.script.length} scenes`}>
          <ol className="flex list-decimal flex-col gap-1 pl-4 text-sm text-muted-foreground">
            {j.request.script.map((c, i) => (
              <li key={i} className="line-clamp-2">
                {c.prompt}
              </li>
            ))}
          </ol>
        </Section>
      )}

      {refs.length > 0 && (
        <Section label={j.kind === "avatar" ? "Portrait" : refs.length > 1 ? "Reference images" : "Reference image"}>
          <div className="flex flex-wrap gap-2">
            {refs.map((r) => (
              <img key={r} src={r} alt="" className="size-16 rounded-md object-cover ring-1 ring-foreground/10" />
            ))}
          </div>
        </Section>
      )}

      <Section label="Details">
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-sm">
          <Fact k="Model">{j.name || j.model}</Fact>
          <Fact k="Size">
            {w} × {h}
          </Fact>
          {(j.kind === "video" || j.kind === "avatar") && videoSeconds(j) != null &&<Fact k="Length">{duration(videoSeconds(j))}</Fact>}
          {seed != null && (
            <Fact k="Seed">
              <span className="inline-flex items-center gap-1">
                <span className="tabular-nums">{seed}</span>
                <CopyButton text={String(seed)} />
              </span>
            </Fact>
          )}
          {steps != null && <Fact k="Steps">{steps}</Fact>}
          {j.request.guidance != null && <Fact k="Guidance">{j.request.guidance}</Fact>}
          {t != null && (
            <Fact k="Took">
              {duration(t)}
              {work != null && Math.abs(work - t) > 1 && <span className="text-muted-foreground"> ({duration(work)} drawing)</span>}
            </Fact>
          )}
          <Fact k="Created">{created.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" })}</Fact>
        </dl>
      </Section>

      <div className="mt-auto grid grid-cols-2 gap-2 border-t p-5">
        {onUseAgain && (
          <Button variant="outline" onClick={onUseAgain}>
            <Wand2 /> {useAgainLabel}
          </Button>
        )}
        <Button variant="outline" onClick={() => download(file)}>
          <Download /> Download
        </Button>
        {!inLibrary && (
          <Button variant="outline" onClick={() => navigate("library", j.id)}>
            <Library /> In Library
          </Button>
        )}
        <DeleteJob
          job={j}
          after={afterDelete}
          trigger={(open) => (
            <Button variant="outline" className="text-muted-foreground hover:text-destructive" onClick={open}>
              <Trash2 /> Delete
            </Button>
          )}
        />
      </div>
    </aside>
  );
}

function Section({ label, action, children }: { label: string; action?: ReactNode; children: ReactNode }) {
  return (
    <div className="flex flex-col gap-1.5 px-5 py-3">
      <div className="flex min-h-6 items-center">
        <span className="text-xs font-semibold tracking-wide text-muted-foreground">{label}</span>
        <div className="flex-1" />
        {action}
      </div>
      {children}
    </div>
  );
}

function Fact({ k, children }: { k: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{k}</dt>
      <dd className="min-w-0 truncate">{children}</dd>
    </>
  );
}

function CopyButton({ text }: { text: string }) {
  const [done, setDone] = useState(false);
  return (
    <Button
      variant="ghost"
      size="icon-xs"
      title="Copy"
      className="text-muted-foreground"
      onClick={() => {
        void navigator.clipboard?.writeText(text);
        setDone(true);
        setTimeout(() => setDone(false), 1200);
      }}
    >
      {done ? <Check /> : <Copy />}
    </Button>
  );
}
