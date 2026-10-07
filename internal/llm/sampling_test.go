package llm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModelDefaults(t *testing.T) {
	dir := t.TempDir()
	// Qwen3.6-35B-A3B's own file
	gen := `{"bos_token_id": 248044, "do_sample": true, "eos_token_id": [248046, 248044], "pad_token_id": 248044,
		"temperature": 1.0, "top_k": 20, "top_p": 0.95}`
	if err := os.WriteFile(filepath.Join(dir, "generation_config.json"), []byte(gen), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := ReadGenerationConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g.Temperature == nil || *g.Temperature != 1 || g.TopK == nil || *g.TopK != 20 || g.PresencePenalty != nil {
		t.Fatalf("generation config %+v", g)
	}
	moe := &Config{ModelType: "qwen3_5_moe", Layers: 40, FullAttnInterval: 4, LinearVHeads: 32, KVHeads: 2, HeadDim: 256}
	d := ModelDefaults(moe, g)
	want := Options{MaxTokens: 32768, Temperature: 1, TopK: 20, TopP: 0.95, PresencePenalty: 1.5}
	if d.For(true) != want {
		t.Errorf("thinking %+v, want %+v", d.Think, want)
	}
	if w := (Options{MaxTokens: 4096, Temperature: 0.7, TopK: 20, TopP: 0.8, PresencePenalty: 1.5}); d.For(false) != w {
		t.Errorf("no thinking %+v, want %+v", d.NoThink, w)
	}
	// the file's values replace the card's thinking settings
	pp, temp := float32(0.5), float32(0.8)
	d = ModelDefaults(moe, GenerationConfig{Temperature: &temp, PresencePenalty: &pp})
	if d.Think.Temperature != 0.8 || d.Think.PresencePenalty != 0.5 || d.NoThink.Temperature != 0.7 {
		t.Errorf("override %+v", d)
	}
	// Qwen3: the card's 0.6 for thinking, no penalty
	d = ModelDefaults(&Config{ModelType: "qwen3", Layers: 36, KVHeads: 8, HeadDim: 128}, GenerationConfig{})
	if d.Think.Temperature != 0.6 || d.Think.PresencePenalty != 0 || d.NoThink.TopP != 0.8 {
		t.Errorf("qwen3 %+v", d)
	}
	// 36 attention layers × 8 heads × 128 × 2 × 2 bytes = 147 KB a token: 32768 tokens would be 4.5 GB, cut below 4 GB
	if n := d.Think.MaxTokens; n >= 32768 || n < 8192 || n*147456 > 4<<30 {
		t.Errorf("qwen3 thinking budget %d", n)
	}
	// a catalogue model not downloaded: Qwen3.5's
	if d := DefaultsFor(""); d.Think.PresencePenalty != 1.5 || d.Think.MaxTokens != 32768 {
		t.Errorf("no folder %+v", d)
	}
}
