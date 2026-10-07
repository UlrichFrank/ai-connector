package llm

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
)

func TestDefaultByMemory(t *testing.T) {
	for _, c := range []struct {
		mem  int64
		want string
	}{
		{0, "qwen3.5-4b"},
		{8 * gb, "qwen3.5-4b"},
		{16 * gb, "qwen3.5-9b"},
		{24 * gb, "qwen3.5-9b"},
		{32 * gb, "qwen3.5-9b"},
		{36 * gb, "qwen3.6-35b-a3b"},
		{64 * gb, "qwen3.6-35b-a3b"},
		{192 * gb, "qwen3.6-35b-a3b"}, // the dense 27B only on request
	} {
		if got := Default(c.mem).Short; got != c.want {
			t.Errorf("%d GB: %s, want %s", c.mem/gb, got, c.want)
		}
	}
	for _, name := range []string{"qwen3.6-35b-a3b", "mlx-community/Qwen3.6-35B-A3B-4bit", "Qwen3.6-35B-A3B-4bit", "QWEN3.6-35B-A3B"} {
		if c, ok := Find(name); !ok && name != "QWEN3.6-35B-A3B" || ok && c.Short != "qwen3.6-35b-a3b" {
			t.Errorf("Find(%q) = %v %v", name, c.Short, ok)
		}
	}
	if !Qwen36_35BA3B.Fits(64*gb) || Qwen36_35BA3B.Fits(24*gb) {
		t.Error("Fits")
	}
	// every catalogue model is pinned completely
	for _, c := range Catalogue {
		have := map[string]bool{}
		for _, f := range c.Files {
			if len(f.SHA256) != 64 || f.Size <= 0 {
				t.Errorf("%s: %s not pinned", c.Repo, f.Path)
			}
			have[f.Path] = true
		}
		if !have["config.json"] || !have["tokenizer.json"] || len(c.Rev) != 40 {
			t.Errorf("%s: incomplete", c.Repo)
		}
	}
}

// writeSafetensors writes a valid safetensors file with one tensor of n bytes.
func writeSafetensors(t *testing.T, path string, n int) []byte {
	t.Helper()
	hdr := []byte(`{"w":{"dtype":"U8","shape":[` + strconv.Itoa(n) + `],"data_offsets":[0,` + strconv.Itoa(n) + `]}}`)
	b := make([]byte, 8, 8+len(hdr)+n)
	binary.LittleEndian.PutUint64(b, uint64(len(hdr)))
	b = append(b, hdr...)
	b = append(b, make([]byte, n)...)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

const tinyConfig = `{"model_type":"qwen3_5","text_config":{"hidden_size":8,"num_hidden_layers":4,"num_attention_heads":2,
"num_key_value_heads":1,"head_dim":4,"intermediate_size":16,"vocab_size":32,"linear_num_key_heads":1,"linear_num_value_heads":2,
"linear_key_head_dim":4,"linear_value_head_dim":4,"linear_conv_kernel_dim":4,"full_attention_interval":4,"tie_word_embeddings":true,
"rope_parameters":{"rope_type":"default","rope_theta":10000000,"partial_rotary_factor":0.25},"eos_token_id":7},
"quantization":{"group_size":64,"bits":4,"mode":"affine","language_model.model.layers.0.mlp.gate":{"group_size":64,"bits":8}}}`

// a fake model folder and a catalogue entry pinned to it
func fakeModel(t *testing.T, dir string) Chat {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(tinyConfig), 0o644)
	os.WriteFile(filepath.Join(dir, "tokenizer.json"), []byte(`{}`), 0o644)
	writeSafetensors(t, filepath.Join(dir, "model.safetensors"), 100)
	var files []models.File
	for _, name := range []string{"config.json", "tokenizer.json", "model.safetensors"} {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		s := sha256.Sum256(b)
		files = append(files, models.File{Path: name, Size: int64(len(b)), SHA256: hex.EncodeToString(s[:])})
	}
	return Chat{Short: "tiny", Model: models.Model{Name: "Tiny", Repo: "mlx-community/Tiny-4bit", Rev: strings.Repeat("a", 40), Files: files}}
}

func TestResolveLocalCopies(t *testing.T) {
	root := t.TempDir()
	lms := filepath.Join(root, "lmstudio")
	tiny := fakeModel(t, filepath.Join(lms, "lmstudio-community", "Tiny-MLX-4bit")) // another publisher, another name
	old := Catalogue
	Catalogue = []Chat{tiny}
	defer func() { Catalogue = old }()

	store := &models.Store{Dir: filepath.Join(root, "models")}
	var progress []string
	v := &Verifier{Path: filepath.Join(root, "models", "verified.json"), Progress: func(f string, a ...any) { progress = append(progress, f) }}
	f := &Finder{Store: store, Verifier: v, LMStudio: []string{lms}, HFCache: filepath.Join(root, "hf"), Mem: 64 * gb}

	// auto: the catalogue model is found in LM Studio's folder by its files (sizes, then checksums), used in place
	c, err := f.Resolve("", true)
	if err != nil || c.Chat == nil || c.Where != "LM Studio" || !strings.HasSuffix(c.Dir, "Tiny-MLX-4bit") {
		t.Fatalf("auto: %+v %v", c, err)
	}
	if _, err := os.Stat(v.Path); err != nil {
		t.Fatal("the checksums were not remembered")
	}
	if _, err := f.Ensure(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	// by name, too
	if c, err := f.Resolve("tiny", true); err != nil || c.Dir == "" {
		t.Fatalf("by name: %+v %v", c, err)
	}
	// a changed file no longer matches (size and modification time checked; the checksum again)
	p := filepath.Join(c.Dir, "config.json")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "10000000", "10000001", 1)), 0o644)
	os.Chtimes(p, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	f2 := &Finder{Store: store, Verifier: v, LMStudio: []string{lms}, Mem: 64 * gb}
	if c, err := f2.Resolve("", true); err != nil || c.Dir != "" {
		t.Fatalf("a modified copy was taken: %+v %v", c, err)
	}
	// … but it is still a model of its own, by its folder or its LM Studio name (not pinned, checked for consistency)
	if c, err := f2.Resolve("lmstudio-community/Tiny-MLX-4bit", true); err != nil || c.Chat != nil || c.Where != "LM Studio" {
		t.Fatalf("own model: %+v %v", c, err)
	}
	if c, err := f2.Resolve(filepath.Join(lms, "lmstudio-community", "Tiny-MLX-4bit"), true); err != nil || c.Where != "your folder" {
		t.Fatalf("own folder: %+v %v", c, err)
	}
	// unknown names list the catalogue and the local models
	if _, err := f2.Resolve("nope", true); err == nil || !strings.Contains(err.Error(), "lmstudio-community/Tiny-MLX-4bit") {
		t.Fatalf("unknown: %v", err)
	}
	// nothing local: the default is fetched on first use
	f3 := &Finder{Store: store, Verifier: v, Mem: 64 * gb}
	if c, err := f3.Resolve("", false); err != nil || c.Dir != "" || c.Missing(store) == 0 {
		t.Fatalf("download: %+v %v", c, err)
	}
}

func TestCheckDir(t *testing.T) {
	dir := t.TempDir()
	fakeModel(t, dir)
	cfg, err := CheckDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Hybrid() || !cfg.IsLinear(0) || cfg.IsLinear(3) || cfg.PartialRotary != 0.25 || cfg.ToolFormat() != "xml" {
		t.Fatalf("config %+v", cfg)
	}
	if gs, bits := cfg.Quant("language_model.model.layers.0.mlp.gate"); gs != 64 || bits != 8 {
		t.Fatalf("override %d %d", gs, bits)
	}
	// a cut-off download
	p := filepath.Join(dir, "model.safetensors")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, b[:len(b)-10], 0o644)
	if _, err := CheckDir(dir); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("cut off: %v", err)
	}
	// an index that names a missing file
	writeSafetensors(t, p, 100)
	os.WriteFile(filepath.Join(dir, "model.safetensors.index.json"), []byte(`{"weight_map":{"a":"model-00001-of-00002.safetensors"}}`), 0o644)
	if _, err := CheckDir(dir); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("index: %v", err)
	}
	// a model type the runner does not know
	os.Remove(filepath.Join(dir, "model.safetensors.index.json"))
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"model_type":"llama","quantization":{"group_size":64,"bits":4}}`), 0o644)
	if _, err := CheckDir(dir); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("llama: %v", err)
	}
}

func TestParseConfigMoE(t *testing.T) {
	raw := `{"model_type":"qwen3_5_moe","quantization":{"group_size":64,"bits":4},"text_config":{"hidden_size":2048,"num_hidden_layers":40,
	"num_attention_heads":16,"num_key_value_heads":2,"head_dim":256,"vocab_size":248320,"linear_num_key_heads":16,"linear_num_value_heads":32,
	"linear_key_head_dim":128,"linear_value_head_dim":128,"linear_conv_kernel_dim":4,"num_experts":256,"num_experts_per_tok":8,
	"moe_intermediate_size":512,"shared_expert_intermediate_size":512,"eos_token_id":[248046,248044]}}`
	c, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsMoE(0) || !c.NormTopK || c.FullAttnInterval != 4 || len(c.EOS) != 2 || !c.AttnGate || c.PartialRotary != 0.25 {
		t.Fatalf("%+v", c)
	}
	// plain Qwen3: attention only, JSON tool calls
	c, err = ParseConfig([]byte(`{"model_type":"qwen3","hidden_size":2560,"num_hidden_layers":36,"num_attention_heads":32,"num_key_value_heads":8,
	"head_dim":128,"vocab_size":151936,"rope_theta":5000000,"quantization":{"group_size":64,"bits":4}}`))
	if err != nil || c.Hybrid() || c.AttnGate || c.PartialRotary != 1 || c.ToolFormat() != "json" || c.RopeTheta != 5e6 {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := ParseConfig([]byte(`{"model_type":"qwen3","hidden_size":8,"num_hidden_layers":1,"num_attention_heads":1,"vocab_size":8}`)); err == nil {
		t.Fatal("an unquantised checkpoint was accepted")
	}
}
