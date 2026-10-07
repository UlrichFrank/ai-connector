import { Settings2, Sparkles, Undo2 } from "lucide-react";
import { useState, type ComponentProps } from "react";
import { toast } from "sonner";
import type { PromptUse } from "@/api/types";
import { errorText } from "@/api/client";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { helperModel, runChat, setHelperModel } from "@/lib/jobs";
import { local } from "@/lib/storage";
import { cn } from "@/lib/utils";
import { useApp } from "@/store/app";

/**
 * A labelled text area. With `enhance`, a ✨ in its header lets a chat model rewrite the text with one of the
 * system prompts of that use (Prompts page); ↺ puts the old text back.
 */
export function PromptBox({
  label,
  value,
  onChange,
  enhance,
  context,
  hint,
  actions,
  className,
  ...rest
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  enhance?: PromptUse;
  /** Extra text for the chat model (e.g. the lyrics when rewriting a style). */
  context?: string;
  hint?: string;
  actions?: React.ReactNode;
} & Omit<ComponentProps<typeof Textarea>, "value" | "onChange">) {
  const [busy, setBusy] = useState(false);
  const [before, setBefore] = useState<string | null>(null);
  const prompts = useApp((s) => s.prompts).filter((p) => p.use === enhance);
  const [promptId, setPromptId] = useState(() => local.get<string>(`enhance.${enhance}`, ""));
  const prompt = prompts.find((p) => p.id === promptId) ?? prompts[0];

  const run = async () => {
    const model = helperModel();
    if (!value.trim() || !prompt || !model) return;
    setBusy(true);
    try {
      const out = await runChat(model, prompt.text, context ? `${context}\n\n${value}` : value);
      if (out) {
        setBefore(value);
        onChange(out);
      }
    } catch (e) {
      toast.error("Enhance failed", { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className={cn("flex flex-col gap-1.5", className)}>
      <div className="flex min-h-6 items-center gap-1">
        <Label className="text-xs font-semibold tracking-wide text-muted-foreground">{label}</Label>
        <div className="flex-1" />
        {actions}
        {enhance && before != null && !busy && (
          <Button variant="ghost" size="xs" onClick={() => (onChange(before), setBefore(null))}>
            <Undo2 /> Undo
          </Button>
        )}
        {enhance && (
          <>
            <Button variant="ghost" size="xs" className="text-primary" disabled={busy || !value.trim() || !prompt} onClick={run}>
              <Sparkles className={cn(busy && "animate-pulse")} /> {busy ? "Enhancing …" : "Enhance"}
            </Button>
            <EnhanceSettings use={enhance} promptId={prompt?.id ?? ""} setPromptId={(id) => (setPromptId(id), local.set(`enhance.${enhance}`, id))} />
          </>
        )}
      </div>
      <Textarea value={value} onChange={(e) => onChange(e.target.value)} disabled={busy} {...rest} />
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
}

function EnhanceSettings({ use, promptId, setPromptId }: { use: PromptUse; promptId: string; setPromptId: (id: string) => void }) {
  const prompts = useApp((s) => s.prompts).filter((p) => p.use === use);
  const chats = useApp((s) => s.models).filter((m) => m.kind === "chat");
  const [model, setModel] = useState(helperModel() ?? "");
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button variant="ghost" size="icon-xs" title="Which model and prompt Enhance uses">
          <Settings2 />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="flex w-72 flex-col gap-3">
        <div className="flex flex-col gap-1.5">
          <Label className="text-xs">Model</Label>
          <Select value={model} onValueChange={(v) => (setModel(v), setHelperModel(v))}>
            <SelectTrigger className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {chats.map((m) => (
                <SelectItem key={m.id} value={m.id}>
                  {m.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="flex flex-col gap-1.5">
          <Label className="text-xs">Prompt</Label>
          <Select value={promptId} onValueChange={setPromptId}>
            <SelectTrigger className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {prompts.map((p) => (
                <SelectItem key={p.id} value={p.id}>
                  {p.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <a href="#/prompts" className="text-xs text-primary hover:underline">
            Edit prompts …
          </a>
        </div>
      </PopoverContent>
    </Popover>
  );
}
