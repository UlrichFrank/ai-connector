package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/philipparndt/ai-connector/internal/models"
)

func TestZImageCatalog(t *testing.T) {
	store := &models.Store{Dir: t.TempDir()}
	m := manifestModel(t, store, "", zImageID)
	if m.Kind != Image || m.Backend != "mflux" || m.mflux != "z-image-turbo" || m.Here || m.Licence != "Apache-2.0" || m.NonCommercial {
		t.Errorf("%+v", m)
	}
	if len(m.Tasks) != 1 || m.Tasks[0] != "generate" {
		t.Errorf("tasks %v, want [generate]", m.Tasks)
	}
	// pre-quantised: loaded as stored (-1 → no quantize in the worker), 9 steps
	if d := m.Defaults; d.Quantize != -1 || d.Steps != 9 || d.Width != 1024 || d.Height != 1024 {
		t.Errorf("defaults: %+v", d)
	}
	// the files mflux loads, not the example pictures of the pack
	keep := func(p string) bool { return models.Matches(p, m.files...) }
	for _, p := range []string{"transformer/0.safetensors", "transformer/model.safetensors.index.json", "text_encoder/1.safetensors",
		"vae/0.safetensors", "tokenizer/tokenizer.json"} {
		if !keep(p) {
			t.Errorf("does not fetch %s", p)
		}
	}
	for _, p := range []string{"README.md", ".DS_Store", "z_image_example.png", "z_image_turbo_example.jpg"} {
		if keep(p) {
			t.Errorf("fetches %s", p)
		}
	}

	// the one image generator that fits a 16 GB Mac; FLUX.2 [klein] 4B does not
	flux := &Model{ID: "flux2-klein-4b", Kind: Image, Size: 16 * gb}
	if !FitsMemory(m, 16*gb) || FitsMemory(flux, 16*gb) {
		t.Error("16 GB: Z-Image Turbo fits, FLUX.2 [klein] 4B does not")
	}
	for mem, want := range map[int64]string{0: "flux2-klein-4b", 8 * gb: "z-image-turbo", 16 * gb: "z-image-turbo", 24 * gb: "flux2-klein-4b", 64 * gb: "flux2-klein-4b"} {
		if got := RecommendedImage(mem); got != want {
			t.Errorf("RecommendedImage(%d GB) = %s, want %s", mem/gb, got, want)
		}
	}

	// here: the store has verified weights of the transformer, the VAE and the text encoder
	d := filepath.Join(store.Dir, filepath.FromSlash(m.repo), m.rev)
	for _, f := range []string{"transformer/0.safetensors.sha256", "vae/0.safetensors.sha256", "text_encoder/0.safetensors.sha256"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(d, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if m := manifestModel(t, store, "", zImageID); !m.Here || m.Where != d {
		t.Errorf("not here with every weight in the store: %+v", m)
	}
}

// Z-Image Turbo refuses reference images before anything is downloaded or loaded.
func TestZImageTextOnly(t *testing.T) {
	b := &FluxBackend{}
	m := manifestModel(t, &models.Store{Dir: t.TempDir()}, "", zImageID)
	err := b.Run(context.Background(), m, Request{Prompt: "a fox", Images: []string{"ref.png"}}, t.TempDir(), func(Event) {})
	if err == nil {
		t.Fatal("ran with a reference image")
	}
}
