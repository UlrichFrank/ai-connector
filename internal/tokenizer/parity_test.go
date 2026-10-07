package tokenizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestParity compares with the Rust tokenizers library on the real tokenizer files — when they are there:
// MLXLAB_TOKENIZERS=<dir> with laya.json, qwen.json (the two tokenizer.json files) and tokens.json, the reference
// written by Python (texts → ids with and without special tokens).
func TestParity(t *testing.T) {
	dir := os.Getenv("MLXLAB_TOKENIZERS")
	if dir == "" {
		t.Skip("MLXLAB_TOKENIZERS not set")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ref map[string][]struct {
		Text    string `json:"text"`
		IDs     []int  `json:"ids"`
		Special []int  `json:"special"`
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	for name, cases := range ref {
		tk, err := Load(filepath.Join(dir, name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			if got := tk.Encode(c.Text); !slices.Equal(got, c.IDs) && !(len(got) == 0 && len(c.IDs) == 0) {
				t.Errorf("%s %q:\n got %v\nwant %v", name, c.Text, got, c.IDs)
			}
			if got := tk.EncodeSpecial(c.Text, 0); !slices.Equal(got, c.Special) {
				t.Errorf("%s %q with special tokens:\n got %v\nwant %v", name, c.Text, got, c.Special)
			}
		}
	}
}
