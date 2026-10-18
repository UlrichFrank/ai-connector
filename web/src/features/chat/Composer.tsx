import { t } from "@/lib/i18n";
import { ArrowUp, Brain, ListPlus, ScrollText, SlidersHorizontal, Square } from "lucide-react";
import { forwardRef, useImperativeHandle, useLayoutEffect, useRef, type ReactNode } from "react";
import type { Model, Prompt } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectSeparator, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Slider } from "@/components/ui/slider";
import { Toggle } from "@/components/ui/toggle";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";

const thinkHint =
  "the model reasons before it answers and before each tool call (you can read its steps). Slower, often better for hard questions and tools. Remembered per kind of chat.";

export interface Settings {
  /** A prompt id of the library, "none", or "keep" (the open conversation's own system prompt). */
  system: string;
  think: boolean;
  /** null: the model's default for thinking on or off (Auto); 0: greedy. */
  temperature: number | null;
  /** null: Auto (32768 tokens with thinking, 4096 without). */
  maxTokens: number | null;
}

export interface ComposerHandle {
  focus: () => void;
}

export const Composer = forwardRef<
  ComposerHandle,
  {
    value: string;
    onChange: (v: string) => void;
    onSend: () => void;
    onStop: () => void;
    streaming: boolean;
    canSend: boolean;
    models: ReactNode;
    /** The chat's tools (zone): a choice for a new chat, fixed in a conversation. */
    tools?: ReactNode;
    settings: Settings;
    setSettings: (s: Partial<Settings>) => void;
    /** The chosen model's defaults (shown for the settings left on Auto). */
    sampling?: Model["sampling"];
    prompts: Prompt[];
    /** The open conversation's system prompt when it is not one of the library's. */
    keepText?: string;
    placeholder?: string;
    /** While an answer runs, Enter queues the message (onSend) instead of being blocked. */
    queueing?: boolean;
    /** Messages queued in this conversation (the badge on Queue). */
    queued?: number;
    /** ↑ in an empty composer: edit the last queued message. */
    onEditLast?: () => void;
  }
>(function Composer(
  { value, onChange, onSend, onStop, streaming, canSend, models, tools, settings, setSettings, sampling, prompts, keepText, placeholder, queueing, queued = 0, onEditLast },
  ref,
) {
  const area = useRef<HTMLTextAreaElement>(null);
  useImperativeHandle(ref, () => ({ focus: () => area.current?.focus() }), []);

  // grows with the text, up to a third of the screen
  useLayoutEffect(() => {
    const t = area.current;
    if (!t) return;
    t.style.height = "auto";
    t.style.height = Math.min(t.scrollHeight, Math.max(160, window.innerHeight / 3)) + "px";
  }, [value]);

  const send = () => {
    if (canSend && value.trim() && (!streaming || queueing)) onSend();
  };

  return (
    <div className="rounded-2xl border bg-card shadow-sm transition-colors focus-within:border-ring/60 dark:bg-card/70">
      <textarea spellCheck={false} autoCorrect="off" autoCapitalize="off"
        ref={area}
        rows={1}
        value={value}
        placeholder={placeholder ?? t("Ask anything")}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
            e.preventDefault();
            send();
          } else if (e.key === "Escape" && value) {
            e.preventDefault();
            onChange("");
          } else if (e.key === "ArrowUp" && !value && queued > 0 && onEditLast) {
            e.preventDefault();
            onEditLast();
          }
        }}
        className="block max-h-[50vh] w-full resize-none bg-transparent px-4 pt-3.5 pb-2 text-[0.9375rem] leading-relaxed outline-none placeholder:text-muted-foreground"
      />
      <div className="flex items-center gap-1 px-2 pb-2">
        <div className="flex min-w-0 flex-1 flex-wrap items-center gap-0.5 sm:gap-1">
          {models}
          {tools}
          <SystemPicker settings={settings} setSettings={setSettings} prompts={prompts} keepText={keepText} />
          <Tooltip>
            <TooltipTrigger asChild>
              <Toggle
                size="sm"
                pressed={settings.think}
                onPressedChange={(think) => setSettings({ think })}
                aria-label={t("Think")}
                className="h-7 gap-1 rounded-full border border-transparent px-2.5 text-[0.8rem] text-muted-foreground hover:text-foreground aria-pressed:border-primary/35 aria-pressed:bg-primary/12 aria-pressed:text-foreground [&_svg]:size-3.5 aria-pressed:[&_svg]:text-primary"
              >
                <Brain /> Think
              </Toggle>
            </TooltipTrigger>
            <TooltipContent className="max-w-64">
              {settings.think ? t("Think is on: ") : t("Think is off: ")}
              {thinkHint}
            </TooltipContent>
          </Tooltip>
          <Tuning settings={settings} setSettings={setSettings} sampling={sampling} />
        </div>
        {streaming && queueing && (
          <Button
            className="relative h-9 shrink-0 gap-1.5 rounded-full px-3.5 max-sm:w-9 max-sm:px-0"
            title={t("Queue: sent when the answer is done (Enter)")}
            disabled={!canSend || !value.trim()}
            onClick={send}
          >
            <ListPlus className="size-4" /> <span className="max-sm:hidden">{t("Queue")}</span>
            {queued > 0 && (
              <span className="absolute -top-1.5 -right-1.5 grid h-4.5 min-w-4.5 place-items-center rounded-full bg-foreground px-1 text-[10px] leading-none font-semibold text-background tabular-nums">
                {queued}
              </span>
            )}
          </Button>
        )}
        {streaming ? (
          <Button size="icon" variant="secondary" className="size-9 shrink-0 rounded-full" title={t("Stop")} onClick={onStop}>
            <Square className="size-3.5 fill-current" />
          </Button>
        ) : (
          <Button size="icon" className="size-9 shrink-0 rounded-full" title={t("Send (Enter)")} disabled={!canSend || !value.trim()} onClick={send}>
            <ArrowUp className="size-4.5" />
          </Button>
        )}
      </div>
    </div>
  );
});

function SystemPicker({
  settings,
  setSettings,
  prompts,
  keepText,
}: {
  settings: Settings;
  setSettings: (s: Partial<Settings>) => void;
  prompts: Prompt[];
  keepText?: string;
}) {
  const value = settings.system === "keep" && keepText == null ? "none" : settings.system;
  const name = value === "none" ? t("No system prompt") : value === "keep" ? t("This chat's prompt") : (prompts.find((p) => p.id === value)?.name ?? t("No system prompt"));
  return (
    <Select value={value} onValueChange={(system) => setSettings({ system })}>
      <SelectTrigger
        size="sm"
        title={t("System prompt (edit them on the Prompts page)")}
        className="h-7 max-w-44 border-0 bg-transparent text-[0.8rem] max-sm:px-1.5 max-sm:[&>svg:last-child]:hidden text-muted-foreground shadow-none hover:bg-muted hover:text-foreground dark:bg-transparent dark:hover:bg-muted [&_svg]:size-3.5"
      >
        <ScrollText />
        <SelectValue>
          <span className="max-sm:hidden">{name}</span>
        </SelectValue>
      </SelectTrigger>
      <SelectContent align="start" position="popper" className="max-w-80">
        {keepText != null && (
          <SelectItem value="keep">
            <span className="flex flex-col">
              <span>{t("This chat's prompt")}</span>
              <span className="line-clamp-1 text-xs text-muted-foreground">{keepText}</span>
            </span>
          </SelectItem>
        )}
        <SelectItem value="none">{t("No system prompt")}</SelectItem>
        <SelectSeparator />
        {prompts.map((p) => (
          <SelectItem key={p.id} value={p.id}>
            <span className="flex flex-col">
              <span>{p.name}</span>
              <span className="line-clamp-1 text-xs text-muted-foreground">{p.text}</span>
            </span>
          </SelectItem>
        ))}
        <SelectSeparator />
        <a href="#/prompts" className="block px-2 py-1.5 text-xs text-muted-foreground hover:text-foreground">
          {t("Edit prompts …")}
        </a>
      </SelectContent>
    </Select>
  );
}

const fmt = (n: number) => String(Math.round(n * 100) / 100);

function Tuning({ settings, setSettings, sampling }: { settings: Settings; setSettings: (s: Partial<Settings>) => void; sampling?: Model["sampling"] }) {
  // what Auto means for this model with thinking on or off (an older server sends no defaults)
  const auto = sampling?.[settings.think ? "think" : "noThink"];
  const autoTokens = auto?.maxTokens ?? (settings.think ? 32768 : 4096);
  const temp = settings.temperature;
  const changed = temp != null || settings.maxTokens != null;
  const link = "text-xs text-muted-foreground underline-offset-2 hover:text-foreground hover:underline";
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button variant="ghost" size="icon-sm" className="relative text-muted-foreground" title={t("Temperature and length")}>
          <SlidersHorizontal className="size-3.5" />
          {changed && <span className="absolute top-1 right-1 size-1.5 rounded-full bg-primary" aria-label="changed" />}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="flex w-72 flex-col gap-4">
        <div className="flex flex-col gap-2.5">
          <div className="flex items-center justify-between gap-2">
            <Label className="text-xs">{t("Temperature")}</Label>
            {temp == null ? (
              <span className="text-xs text-muted-foreground tabular-nums">Auto{auto ? ` · ${fmt(auto.temperature)}` : ""}</span>
            ) : (
              <span className="flex items-center gap-2 text-xs tabular-nums">
                <span>{temp === 0 ? "0 (greedy)" : temp.toFixed(2)}</span>
                <button type="button" className={link} onClick={() => setSettings({ temperature: null })}>
                  Auto
                </button>
              </span>
            )}
          </div>
          <Slider
            value={[temp ?? auto?.temperature ?? 0.7]}
            min={0}
            max={1.5}
            step={0.05}
            className={temp == null ? "opacity-60" : undefined}
            onValueChange={([temperature]) => setSettings({ temperature })}
          />
          <p className="text-xs text-muted-foreground">
            {settings.think ? t("Auto is the model's recommendation for thinking. A low temperature makes a thinking model go in circles.") : t("Auto is the model's recommendation without thinking. A low temperature makes a thinking model go in circles.")}
          </p>
        </div>
        <div className="flex flex-col gap-1.5">
          <div className="flex items-center justify-between gap-2">
            <Label className="text-xs" htmlFor="chat-max-tokens">
              {t("Max tokens")}
            </Label>
            {settings.maxTokens != null && (
              <button type="button" className={link} onClick={() => setSettings({ maxTokens: null })}>
                Auto
              </button>
            )}
          </div>
          <Input
            id="chat-max-tokens"
            type="number"
            min={16}
            max={81920}
            step={64}
            placeholder={`Auto (${autoTokens})`}
            value={settings.maxTokens ?? ""}
            onChange={(e) => {
              const n = Number(e.target.value);
              setSettings({ maxTokens: n > 0 ? Math.min(81920, n) : null });
            }}
          />
          <p className="text-xs text-muted-foreground">{t("The longest answer, thinking included.")}</p>
        </div>
        {auto && (
          <p className="text-xs text-muted-foreground tabular-nums">
            {settings.think ? "Thinking" : "Without thinking"}, this model samples at temperature {fmt(auto.temperature)}, top-p {fmt(auto.topP)}, top-k {auto.topK}
            {auto.presencePenalty ? `, presence penalty ${fmt(auto.presencePenalty)}` : ""}, up to {auto.maxTokens} tokens.
          </p>
        )}
        <Button variant="ghost" size="sm" className="self-start text-muted-foreground" disabled={!changed} onClick={() => setSettings({ temperature: null, maxTokens: null })}>
          {t("Reset to Auto")}
        </Button>
      </PopoverContent>
    </Popover>
  );
}
