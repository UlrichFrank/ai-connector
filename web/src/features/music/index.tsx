import { Upload } from "lucide-react";
import { useState } from "react";
import { CreatePanel } from "./CreatePanel";
import { Library } from "./Library";
import { SongPage } from "./SongPage";
import { StylesPage } from "./StylesPage";
import { STARRED } from "./songs";
import { draggingFiles, importFiles, useStudio } from "./state";

/**
 * Music: the library of songs (folders, ★, search) beside either the form for a new song or one song's page with its
 * waveform, studio tools and versions, or the trained styles. `arg` is the song to open (#/music/<id>), or
 * "styles[/<style id>]". On a phone they stack.
 */
export function MusicPage({ arg }: { arg: string }) {
  // #/music/styles[/<id>]: the trained styles
  const styles = arg === "styles" ? "" : arg.startsWith("styles/") ? arg.slice(7) : null;
  const folder = useStudio((s) => s.folder);
  // audio files dragged from the Finder anywhere onto the page: imported into the open folder
  const [over, setOver] = useState(0);
  const target = folder && folder !== STARRED ? folder.split("/").at(-1) : "All songs";
  return (
    <div
      className="relative flex h-full min-h-0 max-lg:h-auto max-lg:flex-col"
      onDragEnter={(e) => draggingFiles(e) && styles == null && setOver((n) => n + 1)}
      onDragLeave={(e) => draggingFiles(e) && setOver((n) => Math.max(0, n - 1))}
      onDragOver={(e) => {
        if (!draggingFiles(e) || styles != null) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = "copy";
      }}
      onDrop={(e) => {
        if (!draggingFiles(e) || styles != null) return;
        e.preventDefault();
        setOver(0);
        void importFiles([...e.dataTransfer.files], folder);
      }}
    >
      <Library open={arg} />
      <div className="min-h-0 min-w-0 flex-1 overflow-y-auto max-lg:order-first max-lg:overflow-visible">
        {styles != null ? <StylesPage id={styles} /> : arg ? <SongPage key={arg} id={arg} /> : <CreatePanel />}
      </div>
      {over > 0 && (
        <div className="pointer-events-none absolute inset-3 z-30 grid place-items-center rounded-2xl border-2 border-dashed border-primary bg-background/80 backdrop-blur-sm">
          <div className="text-center">
            <Upload className="mx-auto mb-2 size-8 text-primary" />
            <div className="text-base font-semibold">Drop to import into {target}</div>
            <div className="mt-1 text-sm text-muted-foreground">MP3, WAV, M4A, FLAC … — several at once is fine. Or drop onto a folder on the left.</div>
          </div>
        </div>
      )}
    </div>
  );
}
