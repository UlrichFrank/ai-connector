// Community styles (docs/music.md): LoRAs from Hugging Face used as styles — not trained here, pinned to a commit.
// The curated list offers "Add"; "Add from Hugging Face" takes any repository id and reads what it says about itself.
import { Check, ExternalLink, Globe, Loader2, Play, Plus, Search } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { api, errorText } from "@/api/client";
import type { CommunityLookup, Style } from "@/api/types";
import { Field } from "@/components/kit/Layout";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { navigate } from "@/lib/route";
import { useStudio } from "./state";
import { licenceText, megabytes, useStyles } from "./styles";

/** The style of a repository, if added (any revision). */
const addedStyle = (styles: Style[], repo: string) => styles.find((s) => s.source?.repo.toLowerCase() === repo.toLowerCase());

/** The curated community LoRAs: each with "Add" (or a link to it once added). */
export function CommunityList() {
  const community = useStyles((s) => s.community);
  const styles = useStyles((s) => s.styles);
  const models = useStyles((s) => s.models);
  const put = useStyles((s) => s.put);
  const [busy, setBusy] = useState("");
  if (!community.length) return null;
  const add = async (repo: string, rev: string) => {
    setBusy(repo);
    try {
      const s = await api.addCommunityStyle({ repo, rev });
      put(s);
      navigate("music", "styles/" + s.id);
    } catch (e) {
      toast.error("Could not add the style", { description: errorText(e) });
    } finally {
      setBusy("");
    }
  };
  return (
    <section className="flex flex-col gap-2">
      <h2 className="flex items-center gap-1.5 text-sm font-semibold">
        <Globe className="size-4 text-primary" /> Community styles
      </h2>
      {community.map((c) => {
        const have = addedStyle(styles, c.repo);
        return (
          <div key={c.repo} className="flex flex-wrap items-start gap-3 rounded-lg border p-3">
            <div className="min-w-0 flex-1">
              <div className="text-sm font-medium">{c.name}</div>
              <p className="text-xs text-muted-foreground">{c.about}</p>
              <p className="mt-1 text-xs text-muted-foreground">
                <a className="underline-offset-2 hover:underline" href={`https://huggingface.co/${c.repo}`} target="_blank" rel="noreferrer">
                  {c.repo}
                </a>{" "}
                · {models.find((m) => m.id === c.baseModel)?.name ?? c.baseModel} · {licenceText(c.licence)}
                {c.size ? ` · ${megabytes(c.size)}` : ""}
              </p>
            </div>
            {have ? (
              <Button size="sm" variant="ghost" onClick={() => navigate("music", "styles/" + have.id)}>
                <Check /> Added
              </Button>
            ) : (
              <Button size="sm" variant="outline" disabled={!!busy} onClick={() => void add(c.repo, c.rev)}>
                {busy === c.repo ? <Loader2 className="animate-spin" /> : <Plus />} Add
              </Button>
            )}
          </div>
        );
      })}
    </section>
  );
}

/** "Add from Hugging Face": a repository id, looked up (commit, PEFT config, metadata), then added. */
export function AddFromHubDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const models = useStyles((s) => s.models);
  const put = useStyles((s) => s.put);
  const [repo, setRepo] = useState("");
  const [found, setFound] = useState<CommunityLookup | null>(null);
  const [trigger, setTrigger] = useState("");
  const [base, setBase] = useState("");
  const [busy, setBusy] = useState(false);
  // community LoRAs are PEFT LoRAs of ACE-Step's DiT (not Stable Audio 3)
  const aceModels = models.filter((m) => !m.described);
  const lookup = async () => {
    setBusy(true);
    setFound(null);
    try {
      const l = await api.lookupCommunityStyle(repo.trim());
      setFound(l);
      setTrigger(l.trigger);
      setBase(l.baseModel);
    } catch (e) {
      toast.error("Not a LoRA mlxlab can use", { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };
  const add = async () => {
    if (!found) return;
    setBusy(true);
    try {
      const s = await api.addCommunityStyle({ repo: found.repo, rev: found.rev, trigger: trigger.trim() || undefined, baseModel: base || undefined });
      put(s);
      onOpenChange(false);
      setRepo("");
      setFound(null);
      navigate("music", "styles/" + s.id);
    } catch (e) {
      toast.error("Could not add the style", { description: errorText(e) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (found && repo.trim() === found.repo) void add();
            else if (repo.trim()) void lookup();
          }}
        >
          <DialogHeader>
            <DialogTitle>Add from Hugging Face</DialogTitle>
            <DialogDescription>
              A PEFT LoRA of ACE-Step 1.5, pinned to its current commit. Its weights download (checked) with the first song that uses it. Use only LoRAs whose terms
              allow your use.
            </DialogDescription>
          </DialogHeader>
          <Field label="Repository">
            <div className="flex gap-2">
              <Input
                autoFocus
                value={repo}
                onChange={(e) => {
                  setRepo(e.target.value);
                  setFound(null);
                }}
                placeholder="owner/name or a huggingface.co link"
              />
              <Button type="button" variant="outline" disabled={busy || !repo.trim()} onClick={() => void lookup()}>
                {busy && !found ? <Loader2 className="animate-spin" /> : <Search />} Look up
              </Button>
            </div>
          </Field>
          {found && (
            <div className="flex flex-col gap-3 rounded-lg border p-3 text-sm">
              <div>
                <div className="font-medium">{found.name}</div>
                {found.description && <p className="text-xs text-muted-foreground">{found.description}</p>}
                <p className="mt-1 text-xs text-muted-foreground">
                  {found.repo}@{found.rev.slice(0, 7)}
                  {found.author ? ` · by ${found.author}` : ""} · {licenceText(found.licence)} · rank {found.rank} · {megabytes(found.size)}
                  {found.metadata ? "" : " · no metadata file: name the trigger word and model"}
                </p>
              </div>
              <Field label="Trigger word" hint="Put in front of the style text of every song with it (the model card names it).">
                <Input value={trigger} onChange={(e) => setTrigger(e.target.value)} placeholder="from the model card" />
              </Field>
              <Field label="Song model" hint={found.baseText ? `The repository says: ${found.baseText}` : undefined}>
                <Select value={base} onValueChange={setBase} disabled={!!found.baseModel}>
                  <SelectTrigger className="w-full">
                    <SelectValue placeholder="Pick the model it was trained on" />
                  </SelectTrigger>
                  <SelectContent>
                    {aceModels.map((m) => (
                      <SelectItem key={m.id} value={m.id}>
                        {m.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </Field>
              {(found.recommended.steps || found.recommended.shift) && (
                <p className="text-xs text-muted-foreground">
                  Recommended: {[found.recommended.steps && `${found.recommended.steps} steps`, found.recommended.guidance && `guidance ${found.recommended.guidance}`, found.recommended.shift && `shift ${found.recommended.shift}`]
                    .filter(Boolean)
                    .join(", ")}{" "}
                  (used when a song leaves them on Auto)
                </p>
              )}
            </div>
          )}
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy || !found || !trigger.trim() || !base}>
              {busy && found && <Loader2 className="animate-spin" />} Add
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** A community style's details: where it comes from, its licence, recommended settings and example prompts. */
export function CommunityInfo({ style: s }: { style: Style }) {
  const src = s.source!;
  const rec = s.recommended;
  const use = (prompt: string, bpm?: number) => {
    useStudio.getState().load({ trainedStyle: s.id, styleStrength: s.strength, style: prompt, bpm: bpm ? String(Math.round(bpm)) : "" }, s.baseModel);
    navigate("music");
  };
  return (
    <section className="flex flex-col gap-3 rounded-lg border p-3">
      <div className="flex items-center gap-2">
        <Globe className="size-4 text-primary" />
        <h3 className="text-sm font-semibold">From Hugging Face</h3>
        <div className="flex-1" />
        <Button asChild size="xs" variant="ghost">
          <a href={src.url || `https://huggingface.co/${src.repo}`} target="_blank" rel="noreferrer">
            <ExternalLink /> Model card
          </a>
        </Button>
      </div>
      {s.description && <p className="text-xs text-muted-foreground">{s.description}</p>}
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs">
        <dt className="text-muted-foreground">Source</dt>
        <dd className="min-w-0 break-all font-mono">
          {src.repo}@{src.rev.slice(0, 12)}
        </dd>
        {src.author && (
          <>
            <dt className="text-muted-foreground">Author</dt>
            <dd>{src.author}</dd>
          </>
        )}
        <dt className="text-muted-foreground">Licence</dt>
        <dd className={src.licence ? "" : "text-amber-600 dark:text-amber-400"}>{licenceText(src.licence)}</dd>
        {src.baseText && (
          <>
            <dt className="text-muted-foreground">Made for</dt>
            <dd>{src.baseText}</dd>
          </>
        )}
        <dt className="text-muted-foreground">LoRA</dt>
        <dd>
          {[src.rank && `rank ${src.rank}`, src.alpha && `alpha ${src.alpha}`, megabytes(src.size), s.weightsFile].filter(Boolean).join(" · ")}
        </dd>
        {rec && (rec.steps || rec.guidance || rec.shift) ? (
          <>
            <dt className="text-muted-foreground">Recommended</dt>
            <dd>
              {[rec.steps && `${rec.steps} steps`, rec.guidance && `guidance ${rec.guidance}`, rec.shift && `shift ${rec.shift}`].filter(Boolean).join(", ")} — used when the
              song leaves them on Auto
            </dd>
          </>
        ) : null}
      </dl>
      <p className="text-xs text-muted-foreground">
        Not trained here, so it cannot be trained further; its weights download (pinned, checked) with the first song that uses it.
        {!src.licence && " The repository states no licence: check its model card before you publish songs made with it."}
      </p>
      {!!s.examples?.length && (
        <div className="flex flex-col gap-1.5">
          <div className="text-xs font-semibold tracking-wide text-muted-foreground">Example prompts</div>
          {s.examples.map((e, i) => (
            <div key={i} className="flex items-start gap-2 rounded-md bg-muted/50 px-2 py-1.5 text-xs">
              <div className="min-w-0 flex-1">
                <div>{e.prompt}</div>
                {(e.bpm || e.key) && <div className="text-muted-foreground">{[e.bpm && `${Math.round(e.bpm)} BPM`, e.key].filter(Boolean).join(" · ")}</div>}
              </div>
              <Button size="xs" variant="ghost" onClick={() => use(e.prompt, e.bpm)} title="Make a song with this prompt and the style">
                <Play /> Use
              </Button>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}
