import { memo, useDeferredValue, useMemo, type MouseEvent } from "react";
import { cn } from "@/lib/utils";
import { renderMarkdown } from "./render";
import "./chat.css";

/** An answer as markdown. While it streams, the text is rendered a little behind (deferred) to stay smooth. */
export const Markdown = memo(function Markdown({
  text,
  streaming,
  thought,
  className,
}: {
  text: string;
  streaming?: boolean;
  /** The model's thinking: quiet and smaller, any HTML in it shown as text. */
  thought?: boolean;
  className?: string;
}) {
  const deferred = useDeferredValue(text);
  const html = useMemo(() => renderMarkdown(deferred, streaming, thought), [deferred, streaming, thought]);
  return <div className={cn("md", thought && "md-thought", streaming && "md-streaming", className)} onClick={copyCode} dangerouslySetInnerHTML={{ __html: html }} />;
});

function copyCode(e: MouseEvent<HTMLDivElement>) {
  const button = (e.target as HTMLElement).closest<HTMLButtonElement>("button[data-copy]");
  const code = button?.closest(".md-code")?.querySelector("code");
  if (!button || !code) return;
  navigator.clipboard.writeText(code.textContent ?? "");
  const label = button.querySelector("span");
  if (label) {
    label.textContent = "Copied";
    setTimeout(() => (label.textContent = "Copy"), 1500);
  }
}
