package qwen3

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestParity compares with transformers on the real model — when it is there: MLXLAB_QWEN=<model dir> and
// MLXLAB_REF=<models.json with the vectors of tools/embed-server's Embedder on the CPU>.
func TestParity(t *testing.T) {
	dir, ref := os.Getenv("MLXLAB_QWEN"), os.Getenv("MLXLAB_REF")
	if dir == "" || ref == "" {
		t.Skip("MLXLAB_QWEN / MLXLAB_REF not set")
	}
	raw, err := os.ReadFile(ref)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Embed struct {
			Texts   []string    `json:"texts"`
			Vectors [][]float64 `json:"vectors"`
		} `json:"embed"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("loaded in %v", time.Since(start))
	start = time.Now()
	got := m.Embed(r.Embed.Texts, 32)
	t.Logf("%d texts in %v", len(got), time.Since(start))
	for i, want := range r.Embed.Vectors {
		if len(got[i]) != len(want) {
			t.Fatalf("dimension %d, want %d", len(got[i]), len(want))
		}
		var cos float64
		for j := range want {
			cos += float64(got[i][j]) * want[j]
		}
		t.Logf("%q: cosine %.6f", r.Embed.Texts[i][:min(30, len(r.Embed.Texts[i]))], cos)
		if cos < 0.9999 {
			t.Errorf("%q: cosine to the reference %.6f", r.Embed.Texts[i], cos)
		}
	}
}
