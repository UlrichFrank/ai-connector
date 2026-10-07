package engine

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/models"
)

func TestQwenImageCatalog(t *testing.T) {
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	store := &models.Store{Dir: t.TempDir()}
	byID := map[string]*Model{}
	for _, q := range qwenImages {
		m := q.model(store, "")
		byID[m.ID] = m
		if !sha.MatchString(m.rev) || m.Kind != Image || m.Backend != "mflux" || m.mflux == "" || m.Here || m.Licence == "" {
			t.Errorf("%s: %+v", m.ID, m)
		}
		if d := m.Defaults; d.Steps == 0 || d.Guidance == 0 || d.Quantize != 8 || d.Width != 1024 || d.Height != 1024 {
			t.Errorf("%s defaults: %+v", m.ID, d)
		}
		// the files mflux loads, not the rest of the repository
		keep := func(p string) bool { return models.Matches(p, m.files...) }
		for _, p := range []string{"transformer/diffusion_pytorch_model-00001-of-00009.safetensors", "transformer/config.json",
			"vae/diffusion_pytorch_model.safetensors", "text_encoder/model-00001-of-00004.safetensors", "text_encoder/config.json"} {
			if !keep(p) {
				t.Errorf("%s does not fetch %s", m.ID, p)
			}
		}
		for _, p := range []string{"README.md", ".gitattributes", "assets/show.jpg", "scheduler/scheduler_config.json"} {
			if keep(p) {
				t.Errorf("%s fetches %s", m.ID, p)
			}
		}
	}
	for id, want := range map[string]string{"qwen-image-2512": "generate", "qwen-image-edit-2511": "edit", "qwen-image-2.1": "generate"} {
		if m := byID[id]; m == nil || len(m.Tasks) != 1 || m.Tasks[0] != want {
			t.Errorf("%s: tasks %v, want [%s]", id, m.Tasks, want)
		}
	}
	if byID["qwen-image-2512"].NonCommercial || byID["qwen-image-edit-2511"].NonCommercial || !byID["qwen-image-2.1"].NonCommercial {
		t.Error("licences: 2512 and Edit 2511 are Apache 2.0, 2.1 the Qwen Research License (non-commercial)")
	}
	if m := byID["qwen-image-2.1"]; !models.Matches("processor/tokenizer.json", m.files...) {
		t.Error("2.1 needs its tokenizer from processor/")
	}
	if m := byID["qwen-image-2512"]; !models.Matches("tokenizer/vocab.json", m.files...) {
		t.Error("2512 needs tokenizer/")
	}

	// here: the store has verified weights of the transformer, the VAE and the text encoder
	q := qwenImages[0]
	d := filepath.Join(store.Dir, filepath.FromSlash(q.repo), q.rev)
	for _, f := range []string{"transformer/a.safetensors.sha256", "vae/b.safetensors.sha256", "text_encoder/c.safetensors.sha256"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(d, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if m := q.model(store, ""); !m.Here || m.Where != d {
		t.Errorf("not here with every weight in the store: %+v", m)
	}
}

// A Qwen-Image model refuses what it cannot do before anything is downloaded or loaded.
func TestQwenImageTasks(t *testing.T) {
	b := &FluxBackend{}
	byID := map[string]*Model{}
	for _, q := range qwenImages {
		byID[q.id] = q.model(&models.Store{Dir: t.TempDir()}, "")
	}
	for _, c := range []struct {
		id     string
		images []string
		want   string
	}{
		{"qwen-image-2512", []string{"ref.png"}, "text only"},
		{"qwen-image-2.1", []string{"ref.png"}, "text only"},
		{"qwen-image-edit-2511", nil, "add at least one reference image"},
	} {
		err := b.Run(context.Background(), byID[c.id], Request{Prompt: "a cat", Images: c.images}, t.TempDir(), func(Event) {})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s with %d images: %v, want %q", c.id, len(c.images), err, c.want)
		}
	}
}

// TestQwenImageLive makes one picture for real (the download, the mflux runtime, the GPU): MLXLAB_QWEN_LIVE=1, with
// MLXLAB_QWEN_MODEL (default qwen-image-2512), MLXLAB_QWEN_IMAGES (comma-separated references, for the edit model),
// MLXLAB_MODELS_DIR (the model store; default ConfigDir()/models) and MLXLAB_QWEN_OUT (default a temp dir).
func TestQwenImageLive(t *testing.T) {
	if os.Getenv("MLXLAB_QWEN_LIVE") == "" {
		t.Skip("MLXLAB_QWEN_LIVE=1 runs Qwen-Image")
	}
	root := ConfigDir()
	dir := os.Getenv("MLXLAB_MODELS_DIR")
	if dir == "" {
		dir = filepath.Join(root, "models")
	}
	srcs, _ := models.Sources(os.Getenv("MLXLAB_MODELS_FROM"), nil)
	store := &models.Store{Dir: dir, Sources: srcs}
	id := os.Getenv("MLXLAB_QWEN_MODEL")
	if id == "" {
		id = "qwen-image-2512"
	}
	var m *Model
	for _, q := range qwenImages {
		if q.id == id {
			m = q.model(store, "")
		}
	}
	if m == nil {
		t.Fatalf("no Qwen-Image model %q", id)
	}
	out := os.Getenv("MLXLAB_QWEN_OUT")
	if out == "" {
		out = t.TempDir()
	}
	var images []string
	if s := os.Getenv("MLXLAB_QWEN_IMAGES"); s != "" {
		images = strings.Split(s, ",")
	}
	b := &FluxBackend{Root: root, Store: store}
	defer b.Unload()
	stats := map[string]any{}
	err := b.Run(context.Background(), m, Request{Prompt: "A red fox in fresh snow at golden hour, a wooden sign that says \"MLXLAB\"",
		Width: 512, Height: 512, Steps: 8, Seed: 7, Images: images}, out, func(ev Event) {
		switch ev.Type {
		case "status", "log":
			t.Log(ev.Text)
		case "file":
			t.Log("file:", ev.File)
		case "stats":
			for k, v := range ev.Data {
				stats[k] = v
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("RESULT %s: %v", id, stats)
}
