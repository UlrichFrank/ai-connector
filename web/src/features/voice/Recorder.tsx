import { Mic, Square, Upload } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

/** The type the browser records in (Chrome, Firefox: webm/opus; Safari: mp4). */
function recordType(): string {
  if (typeof MediaRecorder === "undefined") return "";
  return ["audio/webm;codecs=opus", "audio/webm", "audio/mp4", "audio/ogg"].find((t) => MediaRecorder.isTypeSupported(t)) ?? "";
}

const extOf = (type: string) => (type.includes("mp4") ? "m4a" : type.includes("ogg") ? "ogg" : "webm");

/**
 * Record in the browser (MediaRecorder) or pick / drop an audio file; hands the result to `onFile`. While recording
 * a bar shows the time against the good length (`min`–`ideal` seconds); it stops by itself at `max`.
 */
export function Recorder({
  onFile,
  min = 5,
  ideal = 15,
  max = 20,
  className,
}: {
  onFile: (f: File) => void;
  min?: number;
  ideal?: number;
  max?: number;
  className?: string;
}) {
  const [rec, setRec] = useState<MediaRecorder | null>(null);
  const [secs, setSecs] = useState(0);
  const [level, setLevel] = useState(0);
  const [over, setOver] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const stopRef = useRef<() => void>(() => {});
  const canRecord = typeof MediaRecorder !== "undefined" && !!navigator.mediaDevices?.getUserMedia;

  // stop the microphone when leaving
  useEffect(() => () => stopRef.current(), []);

  const start = async () => {
    let stream: MediaStream;
    try {
      stream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true } });
    } catch (e) {
      toast.error("No microphone", { description: e instanceof DOMException && e.name === "NotAllowedError" ? "The browser was not allowed to use the microphone." : String(e) });
      return;
    }
    const type = recordType();
    const r = new MediaRecorder(stream, type ? { mimeType: type } : undefined);
    const chunks: Blob[] = [];
    const t0 = performance.now();
    // a level meter, so it is clear the microphone hears something
    const ctx = new AudioContext();
    const an = ctx.createAnalyser();
    an.fftSize = 512;
    ctx.createMediaStreamSource(stream).connect(an);
    const buf = new Uint8Array(an.fftSize);
    let raf = 0;
    const tick = () => {
      an.getByteTimeDomainData(buf);
      let peak = 0;
      for (const v of buf) peak = Math.max(peak, Math.abs(v - 128));
      setLevel(peak / 128);
      const s = (performance.now() - t0) / 1000;
      setSecs(s);
      if (s < max) raf = requestAnimationFrame(tick);
      else if (r.state === "recording") r.stop();
    };
    raf = requestAnimationFrame(tick);
    r.ondataavailable = (e) => e.data.size && chunks.push(e.data);
    r.onstop = () => {
      cancelAnimationFrame(raf);
      stream.getTracks().forEach((t) => t.stop());
      void ctx.close();
      setRec(null);
      setLevel(0);
      const mime = r.mimeType || type || "audio/webm";
      const blob = new Blob(chunks, { type: mime });
      if (blob.size) onFile(new File([blob], `recording.${extOf(mime)}`, { type: mime }));
    };
    stopRef.current = () => r.state === "recording" && r.stop();
    r.start(250);
    setSecs(0);
    setRec(r);
  };

  const pick = (files: FileList | null | undefined) => {
    const f = [...(files ?? [])].find((x) => x.type.startsWith("audio/") || /\.(wav|mp3|m4a|flac|ogg|webm|aac)$/i.test(x.name));
    if (f) onFile(f);
    else if (files?.length) toast.info("That is not an audio file");
  };

  if (rec) {
    const short = secs < min;
    return (
      <div className={cn("flex flex-col gap-2 rounded-lg border border-red-500/40 bg-red-500/5 p-3", className)}>
        <div className="flex items-center gap-2 text-sm">
          <span className="size-2.5 animate-pulse rounded-full bg-red-500" />
          <span className="font-medium">Recording</span>
          <span className="text-muted-foreground tabular-nums">{secs.toFixed(1)} s</span>
          <div className="flex-1" />
          <Button size="sm" variant={short ? "outline" : "default"} onClick={() => rec.stop()}>
            <Square className="fill-current" /> Stop
          </Button>
        </div>
        {/* time against the good range */}
        <div className="relative h-2 overflow-hidden rounded-full bg-muted">
          <div className="absolute inset-y-0 bg-emerald-500/25" style={{ left: `${(min / max) * 100}%`, width: `${((ideal - min) / max) * 100}%` }} />
          <div className={cn("absolute inset-y-0 left-0 rounded-full", short ? "bg-amber-500" : secs <= ideal ? "bg-emerald-500" : "bg-amber-500")} style={{ width: `${Math.min(100, (secs / max) * 100)}%` }} />
        </div>
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <span className="flex h-3 items-end gap-px" aria-hidden>
            {[0.15, 0.3, 0.45, 0.6, 0.75].map((t) => (
              <span key={t} className={cn("w-1 rounded-sm bg-muted-foreground/30", level > t && "bg-emerald-500")} style={{ height: `${30 + t * 90}%` }} />
            ))}
          </span>
          {short ? `Keep going — at least ${min} s` : secs <= ideal ? "Good length. Stop when you have finished a sentence." : `Stops by itself at ${max} s`}
        </div>
      </div>
    );
  }

  return (
    <div
      className={cn("grid grid-cols-2 gap-2 rounded-lg p-0.5", over && "bg-primary/10 ring-2 ring-primary", className)}
      onDragOver={(e) => (e.preventDefault(), setOver(true))}
      onDragLeave={() => setOver(false)}
      onDrop={(e) => (e.preventDefault(), setOver(false), pick(e.dataTransfer.files))}
    >
      <input ref={input} type="file" accept="audio/*" hidden onChange={(e) => (pick(e.target.files), (e.target.value = ""))} />
      <Button variant="outline" className="h-auto flex-col gap-1 py-3" disabled={!canRecord} onClick={start} title={canRecord ? undefined : "This browser cannot record"}>
        <Mic className="size-5" />
        <span className="text-sm">Record</span>
      </Button>
      <Button variant="outline" className="h-auto flex-col gap-1 py-3" onClick={() => input.current?.click()}>
        <Upload className="size-5" />
        <span className="text-sm">Upload a file</span>
      </Button>
    </div>
  );
}
