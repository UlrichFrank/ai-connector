// "Copy chat": the worker writes the chat as text (GET /api/jobs/{id}/transcript, docs/chat-tools.md), so every
// client copies the same; this puts it on the clipboard.

import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import { bytes } from "@/lib/format";

/**
 * Copies the transcript of the branch `jobId` is on. The text is fetched inside the click: Safari only lets a page
 * write the clipboard during the gesture, so where it can, the fetch goes in as a ClipboardItem promise.
 */
export async function copyChat(jobId: string, opts: { thinking?: boolean } = {}) {
  const text = api.transcript(jobId, opts);
  const id = toast.loading(opts.thinking ? "Copying the chat with thinking …" : "Copying the chat …");
  try {
    await write(text);
    const t = await text;
    const turns = (t.match(/^Question:$/gm) ?? []).length;
    toast.success(opts.thinking ? "Chat copied with thinking" : "Chat copied", {
      id,
      description: `${turns === 1 ? "1 turn" : `${turns} turns`} · ${bytes(new Blob([t]).size)} — paste it anywhere to look at it.`,
    });
  } catch (e) {
    toast.error("Could not copy the chat", { id, description: errorText(e) });
  }
}

async function write(text: Promise<string>) {
  if (typeof ClipboardItem !== "undefined" && navigator.clipboard?.write) {
    try {
      await navigator.clipboard.write([new ClipboardItem({ "text/plain": text.then((t) => new Blob([t], { type: "text/plain" })) })]);
      return;
    } catch (e) {
      // the transcript itself failed: say so; else try the other ways
      await text;
      if (!(e instanceof DOMException)) throw e;
    }
  }
  const t = await text;
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(t);
      return;
    } catch {
      // not allowed here (no secure context, the gesture is over): the old way
    }
  }
  const area = document.createElement("textarea");
  area.value = t;
  area.setAttribute("readonly", "");
  area.style.position = "fixed";
  area.style.opacity = "0";
  document.body.appendChild(area);
  area.select();
  const ok = document.execCommand("copy");
  area.remove();
  if (!ok) throw new Error("The browser did not allow copying.");
}
