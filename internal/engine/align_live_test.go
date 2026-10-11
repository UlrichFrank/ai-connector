package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/models"
)

// TestAlignLive times the words of real clips with Qwen3-ForcedAligner (MLXLAB_ALIGN_LIVE=1; fetches the aligner,
// 1.2 GB, into the model store first): Kokoro in English, Qwen3-TTS 1.7B in German (MLXLAB_ALIGN_MODELS=kokoro-82m
// for one of them). The wavs stay in MLXLAB_ALIGN_OUT, their words in <wav>.words.json (to compare with ffmpeg's
// silencedetect).
func TestAlignLive(t *testing.T) {
	if os.Getenv("MLXLAB_ALIGN_LIVE") == "" {
		t.Skip("MLXLAB_ALIGN_LIVE=1 runs the aligner")
	}
	root := ConfigDir()
	srcs, _ := models.Sources(os.Getenv("MLXLAB_MODELS_FROM"), nil)
	b := &SpeechBackend{Root: root, Store: &models.Store{Dir: filepath.Join(root, "models"), Sources: srcs}}
	defer b.Unload()
	ctx := context.Background()
	emit := func(ev Event) {
		if ev.Type == "status" || ev.Type == "log" {
			t.Log(ev.Text)
		}
	}
	if _, err := b.fetchRepo(ctx, "Qwen3-ForcedAligner", alignRepo, emit); err != nil {
		t.Fatal(err)
	}
	if !b.alignerHere() {
		t.Fatal("the aligner is not here after its download")
	}
	out := os.Getenv("MLXLAB_ALIGN_OUT")
	if out == "" {
		out = t.TempDir()
	}
	only := os.Getenv("MLXLAB_ALIGN_MODELS")
	for _, x := range []struct{ model, text, lang string }{
		{"kokoro-82m", "Hello! This is Kokoro, reading a short note on the board. Penguins live in Antarctica, far from here.", "en"},
		{"qwen3-tts-1.7b", "Guten Abend! Das ist ein Test der lokalen Sprachausgabe. Pinguine leben in der Antarktis, weit weg von hier.", "de"},
	} {
		if only != "" && !strings.Contains(only, x.model) {
			continue
		}
		m := byID(speechModels())[x.model]
		var res map[string]any
		var file string
		err := b.Run(ctx, m, Request{Text: x.text, Language: x.lang, Speed: 1, Seed: 7}, out, func(ev Event) {
			switch ev.Type {
			case "result":
				res = ev.Data
			case "file":
				file = ev.File
			}
			emit(ev)
		})
		if err != nil {
			t.Fatalf("%s: %v", x.model, err)
		}
		words, _ := res["words"].([]Word)
		js, _ := json.Marshal(words)
		must(t, os.WriteFile(file+".words.json", js, 0o644))
		_, dur := wavInfo(file)
		t.Logf("RESULT %s %s (%.2f s): %d words", x.model, file, dur, len(words))
		for _, w := range words {
			t.Logf("WORD %-14s %6.3f %6.3f", w.Text, w.Start, w.End)
		}
		if len(words) != len(strings.Fields(x.text)) {
			t.Errorf("%s: %d words for %d", x.model, len(words), len(strings.Fields(x.text)))
		}
		for i, w := range words {
			if w.End < w.Start || i > 0 && w.Start < words[i-1].Start || w.End > dur+0.01 {
				t.Errorf("%s: word %d %+v out of order (%.2f s)", x.model, i, w, dur)
			}
		}
	}
}
