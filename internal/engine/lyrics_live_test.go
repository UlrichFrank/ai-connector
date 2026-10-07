package engine

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/models"
)

// TestLyricsLive sings the user's song with raw and translated lyrics and transcribes both (MLXLAB_LYRICS_LIVE=<model
// id>, e.g. heartmula-3b; MLXLAB_HOME with the models and envs). The direction words must be sung only in the raw one.
func TestLyricsLive(t *testing.T) {
	id := os.Getenv("MLXLAB_LYRICS_LIVE")
	if id == "" {
		t.Skip("MLXLAB_LYRICS_LIVE=<model id> sings a song twice")
	}
	root := ConfigDir()
	store := &models.Store{Dir: filepath.Join(root, "models")}
	var m *Model
	for _, x := range musicModels() {
		if x.ID == id {
			m = x
		}
	}
	rts := MusicRuntimes()
	rts["raw"] = rts[m.runtime] // same worker, no translation
	raw := *m
	raw.runtime = "raw"
	b := &MusicBackend{Root: root, Store: store, Runtimes: rts}
	defer b.Unload()
	sb := &SpeechBackend{Root: root, Store: store, Voices: fakeVoices{}}
	defer sb.Unload()
	ctx := context.Background()
	out := os.Getenv("MLXLAB_LYRICS_OUT")
	if out == "" {
		out = t.TempDir()
	}
	lyrics := "[intro]\n(Heavy bass drop, mechanical breathing)\nYeah.\nBlack. Pink.\nWatch your step.\n\n" +
		"[verse]\nSilk on the skin, but the heart is cold steel\nYou wanted the fairy tale, that is not how we deal"
	if f := os.Getenv("MLXLAB_LYRICS_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lyrics = string(b)
	}
	style := "K-Pop, Dance, Dark Pop, 125 bpm, Heavy 808 Bass, Aggressive Female Vocals"
	if s := os.Getenv("MLXLAB_LYRICS_STYLE"); s != "" {
		style = s
	}
	seeds := []int64{4242}
	if v := os.Getenv("MLXLAB_LYRICS_SEEDS"); v != "" {
		seeds = nil
		for _, f := range strings.Split(v, ",") {
			n, _ := strconv.ParseInt(f, 10, 64)
			seeds = append(seeds, n)
		}
	}
	only := os.Getenv("MLXLAB_LYRICS_ONLY")
	for _, seed := range seeds {
		for _, c := range []struct {
			name string
			m    *Model
		}{{"before", &raw}, {"after", m}} {
			if only != "" && only != c.name {
				continue
			}
			var file string
			stats := map[string]any{}
			err := b.Run(ctx, c.m, Request{Prompt: style, Lyrics: lyrics, Duration: 30, Seed: seed}, out, func(ev Event) {
				switch ev.Type {
				case "file":
					file = ev.File
				case "stats":
					for k, v := range ev.Data {
						stats[k] = v
					}
				case "status":
					t.Log(ev.Text)
				}
			})
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			b.Unload()
			named := filepath.Join(out, id+"-"+c.name+"-"+strconv.FormatInt(seed, 10)+".wav")
			_ = os.Rename(file, named)
			heard, err := sb.transcribe(ctx, named, "en", func(Event) {})
			sb.Unload()
			if err != nil {
				t.Fatalf("%s: transcribe: %v", c.name, err)
			}
			t.Logf("RESULT %s %s seed %d\nstyle sent: %v\nlyrics sent:\n%v\nheard: %q", id, c.name, seed, stats["styleSent"], stats["lyricsSent"], heard)
			l := strings.ToLower(heard)
			t.Logf("RESULT %s %s: 'bass drop' sung: %v, 'breathing' sung: %v", id, c.name,
				strings.Contains(l, "bass drop"), strings.Contains(l, "breath"))
		}
	}
}
