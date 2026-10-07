import { Palette } from "lucide-react";
import { useEffect } from "react";
import type { Model } from "@/api/types";
import { Field } from "@/components/kit/Layout";
import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Slider } from "@/components/ui/slider";
import { navigate } from "@/lib/route";
import { licenceText, usableFor, useStyles } from "./styles";

const NONE = "\u0000none";

/**
 * A trained style for the song (StylesPage): shown for a model that takes styles; lists the styles trained for it,
 * with a strength slider. `value` "" is no style.
 */
export function StylePicker({ model, value, strength, onChange }: { model?: Model; value: string; strength: number; onChange: (style: string, strength: number) => void }) {
  const styles = useStyles((s) => s.styles);
  const loaded = useStyles((s) => s.loaded);
  const refresh = useStyles((s) => s.refresh);
  useEffect(() => {
    if (!loaded) void refresh();
  }, [loaded, refresh]);
  const usable = usableFor(styles, model?.id);
  const picked = usable.find((s) => s.id === value);
  // a style of another model (the model changed) is no choice
  useEffect(() => {
    if (value && loaded && model && !picked) onChange("", 1);
  }, [value, loaded, model, picked, onChange]);
  if (!model?.styles) return null;
  return (
    <Field
      label="Your style"
      actions={
        <Button variant="ghost" size="xs" onClick={() => navigate("music", "styles")}>
          <Palette /> {usable.length ? "Styles" : "Train or add one"}
        </Button>
      }
      hint={
        picked
          ? picked.source
            ? `Community LoRA from Hugging Face (${picked.source.repo}, ${licenceText(picked.source.licence)}); its trigger word “${picked.trigger}” is added to the style text${
                picked.recommended?.steps ? `, and its recommended settings (${picked.recommended.steps} steps${picked.recommended.shift ? `, shift ${picked.recommended.shift}` : ""}) are used` : ""
              }.`
            : `Trained on ${picked.tracks.length} of your tracks; its trigger word “${picked.trigger}” is added to the style text.`
          : usable.length
            ? undefined
            : `No style trained for ${model.name} yet.`
      }
    >
      {usable.length > 0 && (
        <Select value={picked ? picked.id : NONE} onValueChange={(v) => (v === NONE ? onChange("", 1) : onChange(v, usable.find((s) => s.id === v)?.strength ?? 1))}>
          <SelectTrigger className="w-full">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={NONE}>No style</SelectItem>
            {usable.map((s) => (
              <SelectItem key={s.id} value={s.id}>
                {s.name}
                {s.worker ? ` · on ${s.worker.name}` : ""}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
      {picked && (
        <div className="flex items-center gap-3 pt-1">
          <span className="w-16 shrink-0 text-xs text-muted-foreground">Strength</span>
          <Slider min={0.1} max={2} step={0.05} value={[strength]} onValueChange={([v]) => onChange(picked.id, v)} className="flex-1 py-2" />
          <span className="w-10 text-right text-xs tabular-nums">{strength.toFixed(2)}</span>
        </div>
      )}
    </Field>
  );
}
