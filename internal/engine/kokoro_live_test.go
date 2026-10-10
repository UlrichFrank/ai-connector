package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/philipparndt/ai-connector/internal/audio"
	"github.com/philipparndt/ai-connector/internal/models"
)

// TestKokoroLive runs Kokoro 82M for real (MLXLAB_KOKORO_LIVE=1; ≈ 0.35 GB of weights and the speech runtime under
// $MLXLAB_HOME — keep that path short: espeak-ng cuts its data path at 160 characters): American and British
// English with word timings, French and Chinese without.
func TestKokoroLive(t *testing.T) {
	if os.Getenv("MLXLAB_KOKORO_LIVE") == "" {
		t.Skip("MLXLAB_KOKORO_LIVE=1 runs Kokoro")
	}
	root := ConfigDir()
	srcs, _ := models.Sources(os.Getenv("MLXLAB_MODELS_FROM"), nil)
	b := &SpeechBackend{Root: root, Store: &models.Store{Dir: filepath.Join(root, "models"), Sources: srcs}}
	defer b.Unload()
	var m *Model
	for _, x := range speechModels() {
		if x.ID == "kokoro-82m" {
			m = x
		}
	}
	out := os.Getenv("MLXLAB_KOKORO_OUT")
	if out == "" {
		out = t.TempDir()
	}
	for _, x := range []struct{ text, voice, lang string }{
		{"Hello! This is Kokoro, reading a short note on the board. It should sound natural.", "", "en"},
		{"Good evening. The British voice reads this one, with its own timing.", "bm_george", ""},
		{"Bonjour ! Ceci est un petit test de la synthèse vocale en français.", "", "fr"},
		{"你好，这是一个简短的中文语音测试。", "zf_xiaobei", ""},
	} {
		var file string
		var res, stats map[string]any
		stats = map[string]any{}
		err := b.Run(context.Background(), m, Request{Text: x.text, Voice: x.voice, Language: x.lang, Speed: 1}, out, func(ev Event) {
			switch ev.Type {
			case "file":
				file = ev.File
			case "result":
				res = ev.Data
			case "stats":
				for k, v := range ev.Data {
					stats[k] = v
				}
			case "status", "log":
				t.Log(ev.Text)
			}
		})
		if err != nil {
			t.Fatalf("%q: %v", x.text, err)
		}
		peaks, info, err := audio.PeaksFile(file, 256)
		if err != nil || info.Seconds < 1 || info.Rate != 24000 {
			t.Fatalf("%q: %+v %v", x.text, info, err)
		}
		loud := 0.0
		for _, p := range peaks {
			loud = max(loud, p)
		}
		words, _ := res["words"].([]Word)
		t.Logf("RESULT %s %v: %.2f s audio in %.2f s, peak %.1f GB, loudest %.2f, %d words: %+v", res["language"], res["voice"],
			info.Seconds, stats["seconds"], stats["peakGB"], loud, len(words), words)
		if loud < 0.05 {
			t.Errorf("%q: silent", x.text)
		}
		if res["language"] == "en" {
			if len(words) < 8 {
				t.Errorf("%q: words %v", x.text, words)
			}
			for i, w := range words {
				if w.End < w.Start || i > 0 && w.Start < words[i-1].Start || w.End > info.Seconds+0.1 {
					t.Errorf("word %d %+v out of order (%.2f s)", i, w, info.Seconds)
				}
			}
		}
	}
}
