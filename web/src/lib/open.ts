import type { Job } from "@/api/types";
import { navigate } from "./route";

/** Shows a job where it lives: a song in the studio, everything else in the library. */
export function openJob(j: Job) {
  if (j.kind === "music") navigate("music", j.request.coverFor ?? j.id);
  else if (j.kind === "chat") navigate("chat", j.id);
  else if (j.kind === "embed" || j.kind === "classify") navigate(j.kind, j.id);
  else if (j.kind === "speech" || j.kind === "sound") navigate("voice", j.id);
  else if (j.request.coverFor) navigate("music", j.request.coverFor);
  else navigate("library", j.id);
}
