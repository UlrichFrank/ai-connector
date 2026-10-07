package engine

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/models"
)

// The LTX-2.5 models: the distilled ones fetch neither the dev transformer nor the distilled LoRA, the HQ ones share
// their folder and fetch those on top; each is here only with the weights its pipeline loads.
func TestLTXCatalog(t *testing.T) {
	store := &models.Store{Dir: t.TempDir()}
	byID := map[string]*Model{}
	for _, m := range ltxModels(store) {
		byID[m.ID] = m
		if m.Kind != Video || m.Backend != "ltx" || !m.Gated || m.Here || m.Size == 0 {
			t.Errorf("%s: %+v", m.ID, m)
		}
	}
	for _, id := range []string{"ltx-2.5-q4", "ltx-2.5-q8", "ltx-2.5-hq-q4", "ltx-2.5-hq-q8"} {
		if byID[id] == nil {
			t.Fatalf("no %s", id)
		}
	}
	keep := func(m *Model, p string) bool { return !models.Matches(p, m.ignore...) }
	for _, q := range []string{"q4", "q8"} {
		dist, hq := byID["ltx-2.5-"+q], byID["ltx-2.5-hq-"+q]
		if dist.hq || !hq.hq || dist.repo != hq.repo || dist.rev != hq.rev {
			t.Errorf("%s: the HQ model must share the distilled one's pack: %+v / %+v", q, dist, hq)
		}
		if !strings.Contains(hq.Name, "HQ") || strings.Contains(hq.Name, "Pro") || !strings.Contains(hq.About, "Pro") {
			t.Errorf("%s: name %q, about %q", hq.ID, hq.Name, hq.About)
		}
		for _, p := range []string{"transformer-dev.safetensors", "ltx-2.5-22b-distilled-lora-450-bf16.safetensors"} {
			if keep(dist, p) {
				t.Errorf("%s fetches %s", dist.ID, p)
			}
			if !keep(hq, p) {
				t.Errorf("%s does not fetch %s", hq.ID, p)
			}
		}
		for _, p := range []string{"transformer-distilled.safetensors", "text_encoder.safetensors", "spatial_upscaler_x2_v1_0.safetensors", "vae_decoder_av.safetensors"} {
			if !keep(dist, p) || !keep(hq, p) {
				t.Errorf("%s: both fetch %s", q, p)
			}
		}
		if d := dist.Defaults; d.Steps != 0 || d.Guidance != 0 {
			t.Errorf("%s: the distilled pipeline has no steps or CFG to set: %+v", dist.ID, d)
		}
		if d := hq.Defaults; d.Steps != 15 || d.Guidance != 3 || d.Width%64 != 0 || d.Height%64 != 0 || d.FPS != 24 {
			t.Errorf("%s defaults: %+v", hq.ID, d)
		}
	}

	// the distilled q8 here: the HQ q8 is not until the dev transformer and the LoRA are too
	dir := filepath.Join(store.Dir, filepath.FromSlash(ltxQ8.repo), ltxQ8.rev)
	touch := func(names ...string) {
		for _, n := range names {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	here := func() map[string]bool {
		h := map[string]bool{}
		for _, m := range ltxModels(store) {
			h[m.ID] = m.Here
		}
		return h
	}
	touch("transformer-distilled.safetensors.sha256", "text_encoder.safetensors.sha256")
	if h := here(); !h["ltx-2.5-q8"] || h["ltx-2.5-hq-q8"] || h["ltx-2.5-q4"] {
		t.Errorf("distilled q8 only: %v", h)
	}
	touch("transformer-dev.safetensors.sha256")
	if h := here(); h["ltx-2.5-hq-q8"] {
		t.Errorf("HQ q8 here without the distilled LoRA: %v", h)
	}
	touch("ltx-2.5-22b-distilled-lora-450-bf16.safetensors.sha256")
	if h := here(); !h["ltx-2.5-hq-q8"] || !h["ltx-2.5-q8"] {
		t.Errorf("all of q8: %v", h)
	}
}

func TestLTXArgs(t *testing.T) {
	r := Request{Width: 512, Height: 320, FPS: 24}
	dist := ltxArgs(ltxRun{dir: "/p"}, r, "a fox", 49, []string{"/f.png"}, 7, "/o.mp4")
	if !slices.Contains(dist, "--distilled") || slices.Contains(dist, "--cfg-scale") || slices.Contains(dist, "--stage1-steps") {
		t.Errorf("distilled: %v", dist)
	}
	r.Steps, r.Guidance = 15, 3.5
	hq := strings.Join(ltxArgs(ltxRun{dir: "/p", hq: true}, r, "a fox", 49, []string{"/f.png"}, 7, "/o.mp4"), " ")
	for _, want := range []string{"generate --two-stages-hq --model /p", "--stage1-steps 15", "--cfg-scale 3.5", "--frames 49", "--image /f.png", "--seed 7"} {
		if !strings.Contains(hq, want) {
			t.Errorf("HQ args %q lack %q", hq, want)
		}
	}
	if strings.Contains(hq, "--distilled ") {
		t.Errorf("HQ args: %q", hq)
	}
	for desc, want := range map[string]string{"Denoising (res2s guided)": "stage 1/2", "Denoising": "stage 2/2", "Loading": ""} {
		if got := hqStage(desc); !strings.HasPrefix(got, want) || (want == "" && got != "") {
			t.Errorf("hqStage(%q) = %q", desc, got)
		}
	}
	if sm := tqdmDesc.FindStringSubmatch("Denoising (res2s guided):  20%|██        | 3/15 [00:10<00:40,  3.1s/it]"); sm == nil || sm[1] != "Denoising (res2s guided)" {
		t.Errorf("tqdm description: %v", sm)
	}
	// the distilled models' movie folders stay as they were
	if movieKey("ltx-2.5-q8", Request{Width: 1}) != fingerprint("ltx-2.5-q8", 1, 0, 0, int64(0), false)[:16] {
		t.Error("movieKey changed for the distilled models")
	}
	if movieKey("m", Request{Steps: 15}) == movieKey("m", Request{Steps: 20}) {
		t.Error("movieKey ignores the steps")
	}
}
