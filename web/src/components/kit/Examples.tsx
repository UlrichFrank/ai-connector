import { Lightbulb } from "lucide-react";
import type { Example, Kind } from "@/api/types";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { useApp } from "@/store/app";

/** The server's example prompts of a kind, as a small menu; picking one fills the form. */
export function ExamplesMenu({ kind, onPick, filter }: { kind: Kind; onPick: (e: Example) => void; filter?: (e: Example) => boolean }) {
  const examples = (useApp((s) => s.examples)[kind] ?? []).filter((e) => !filter || filter(e));
  if (!examples.length) return null;
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="sm" className="text-muted-foreground">
          <Lightbulb /> Examples
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="max-h-96 w-72 overflow-y-auto">
        {examples.map((e) => (
          <DropdownMenuItem key={e.title} onSelect={() => onPick(e)} className="flex flex-col items-start gap-0.5">
            <span className="font-medium">{e.title}</span>
            {(e.prompt || e.question) && <span className="line-clamp-1 text-xs text-muted-foreground">{e.prompt || e.question}</span>}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
