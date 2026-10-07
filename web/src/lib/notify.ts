import { useEffect, useRef } from "react";
import { toast } from "sonner";
import type { Job } from "@/api/types";
import { useApp } from "@/store/app";
import { isTranscription, noun, title } from "./format";
import { openJob } from "./open";

/** A toast when a job ends (done or failed), with a way to open it. Jobs that ended before the page loaded stay quiet. */
export function useDoneToasts() {
  const seen = useRef<Map<string, Job["state"]> | null>(null);
  useEffect(
    () =>
      useApp.subscribe((s) => {
        const jobs = Object.values(s.jobs);
        if (!seen.current) {
          if (s.connection !== "live") return;
          seen.current = new Map(jobs.map((j) => [j.id, j.state]));
          return;
        }
        for (const j of jobs) {
          const before = seen.current.get(j.id);
          seen.current.set(j.id, j.state);
          if (before === j.state || (before !== "running" && before !== "queued")) continue;
          if (j.kind === "chat" || j.kind === "embed" || j.kind === "classify") continue; // their pages show them as they come
          if (isTranscription(j)) continue; // a voice sample written down: the voice form shows it
          if (j.state === "done") toast.success(`The ${noun(j)} is ready`, { description: title(j), action: { label: "Open", onClick: () => openJob(j) } });
          else if (j.state === "failed")
            toast.error(j.problem?.title ?? `The ${noun(j)} failed`, { description: title(j), action: { label: "Open", onClick: () => openJob(j) } });
        }
      }),
    [],
  );
}
