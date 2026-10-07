import { Check, Copy, Eye, EyeOff } from "lucide-react";
import { useEffect, useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { copyText } from "@/lib/clipboard";
import { maskToken } from "@/lib/connect";

/** A command to paste in a terminal: monospace, one Copy button. A secret in it is masked on screen until shown. */
export function CommandBlock({ text, secret }: { text: string; secret?: string }) {
  const [copied, setCopied] = useState(false);
  const [shown, setShown] = useState(false);
  const masked = !!secret && !shown;
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(t);
  }, [copied]);

  const copy = async () => {
    try {
      await copyText(text);
      setCopied(true);
    } catch (e) {
      toast.error("Could not copy", { description: e instanceof Error ? e.message : String(e) });
    }
  };

  return (
    <div className="overflow-hidden rounded-lg border bg-muted/50">
      <div className="flex items-center gap-1 border-b px-2 py-1">
        <span className="flex-1 pl-1 text-xs text-muted-foreground">Terminal on the Mac</span>
        {secret && (
          <Button variant="ghost" size="xs" onClick={() => setShown((v) => !v)} aria-pressed={shown}>
            {shown ? <EyeOff /> : <Eye />} {shown ? "Hide token" : "Show token"}
          </Button>
        )}
        <Button variant={copied ? "secondary" : "default"} size="xs" onClick={copy} data-copy-command="">
          {copied ? <Check /> : <Copy />} {copied ? "Copied" : "Copy"}
        </Button>
      </div>
      <pre
        className="overflow-x-auto px-3 py-2.5 font-mono text-xs leading-relaxed whitespace-pre select-all sm:whitespace-pre-wrap sm:[overflow-wrap:anywhere]"
        // copied by hand while masked: the real command, not the dots
        onCopy={(e) => {
          if (!masked) return;
          e.preventDefault();
          e.clipboardData.setData("text/plain", text);
          setCopied(true);
        }}
      >
        <code>{masked ? maskToken(text, secret ?? "") : text}</code>
      </pre>
    </div>
  );
}
