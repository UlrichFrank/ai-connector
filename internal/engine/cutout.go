package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/pyenv"
)

// Cutouts: one image in, the same image with its background transparent out (an RGBA PNG; vboard's "Remove
// background"). BiRefNet (ZhengPeng7, MIT) predicts a soft foreground mask, which becomes the alpha channel as it is
// — never thresholded; the worker estimates the edge colours anew and, on a plain background, keeps glows and soft
// shadows half transparent (workers/birefnet_worker.py). No prompt.
//
// It runs on PyTorch: CUDA where there is one, Apple's MPS, else the CPU — the one Python backend that is not tied to
// Apple silicon. Measured on a Mac (MPS): BiRefNet loads in 1–4 s, a 1024² picture takes about 1 s (3 s the first
// after loading), a 2048² one about 1 s more; the worker's start (importing PyTorch) 5–14 s.

// TaskCutout: an image model that only cuts the background away (BiRefNet).
const TaskCutout = "cutout"

// CutoutMaxPNG: a cutout PNG larger than this is written as a WebP with alpha instead (vboard keeps image assets of at
// most 15 MB; a JPEG would lose the transparency).
const CutoutMaxPNG = 15 << 20

// CutoutEnv is the cutout worker's runtime: PyTorch and what BiRefNet's own code (birefnet.py, loaded with
// trust_remote_code) imports — timm, kornia, einops, torchvision.
var CutoutEnv = pyenv.Env{Name: "cutout", Python: "3.12", Packages: []string{
	"torch==2.14.1", "torchvision==0.29.1", "transformers==5.19.0", "timm==1.0.30", "kornia==0.8.3", "einops==0.8.2",
	"safetensors==0.8.0", "numpy==2.5.4", "pillow==12.3.0",
}}

// cutoutFiles are the files of a BiRefNet repository the worker loads: its code, its config and the weights.
var cutoutFiles = []string{"birefnet.py", "BiRefNet_config.py", "config.json", "model.safetensors"}

// cutoutModels are the BiRefNet checkpoints (pinned; sizes: the files above; side: what the model sees).
var cutoutModels = []struct {
	id, name, repo, rev, about string
	side                       int
	size                       int64
}{
	{"birefnet", "BiRefNet", "ZhengPeng7/BiRefNet", "e2bf8e4460fc8fa32bba5ea4d94b3233d367b0e4",
		"BiRefNet (ZhengPeng7), MIT: removes the background of an image — a soft alpha matte, edges cleaned for a " +
			"dark backdrop, glows and shadows kept on a plain background; 1024² inside, 1–3 s an image on a Mac, 450 MB",
		1024, 444473596 + 91896 + 298 + 405},
	{"birefnet-hr-matting", "BiRefNet HR matting", "ZhengPeng7/BiRefNet_HR-matting", "5d6b6f8adcb5b417c871b1d84ceaae9871355b7f",
		"BiRefNet HR matting (ZhengPeng7), MIT: trained for soft alpha (hair, fur, smoke) at 2048² inside; finer " +
			"edges for large pictures; slower (about 4–5 s on a Mac) and far more GPU memory (MPS took up to 26 GB)",
		2048, 444473596 + 92134 + 298 + 416},
	{"birefnet-lite", "BiRefNet lite", "ZhengPeng7/BiRefNet_lite", "aa62cd87eafb9cc43056d08ef3615a14628b831d",
		"BiRefNet lite (ZhengPeng7), MIT: the small backbone (Swin-T), 180 MB — quicker, for the CPU or a small " +
			"GPU; coarser edges than BiRefNet",
		1024, 177634392 + 92134 + 298 + 410},
}

// cutoutCatalog adds the cutout models: here when the store has their weights verified, or the Hugging Face cache has them.
func cutoutCatalog(store *models.Store, hub string) []*Model {
	var out []*Model
	for _, c := range cutoutModels {
		m := &Model{ID: c.id, Name: c.name, Kind: Image, Backend: "cutout", About: c.about, Size: c.size, repo: c.repo,
			rev: c.rev, files: cutoutFiles, Tasks: []string{TaskCutout}, Licence: "MIT", side: c.side}
		if d := filepath.Join(store.Dir, filepath.FromSlash(c.repo), c.rev); hasAll(d, "model.safetensors.sha256", "birefnet.py.sha256") {
			m.Here, m.Where = true, d
		} else if d := hfSnapshot(hub, c.repo, c.rev); d != "" && hasAll(d, cutoutFiles...) {
			m.Here, m.Where = true, "the Hugging Face cache (linked on first use)"
		}
		out = append(out, m)
	}
	return out
}

// CutoutBackend runs the cutout models: one worker, which keeps its model loaded between images (under 1 GB of
// weights; light enough to stay beside a chat model).
type CutoutBackend struct {
	Root  string
	Store *models.Store

	mu    sync.Mutex
	w     *pyWorker
	state status
}

func (b *CutoutBackend) Name() string   { return "cutout" }
func (b *CutoutBackend) Heavy() bool    { return false }
func (b *CutoutBackend) Loaded() string { return b.state.get() }

func (b *CutoutBackend) Unload() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.w != nil {
		b.w.stop()
	}
	b.state.set("")
}

// Run cuts the background of the request's one image away; the file is a PNG (a WebP when the PNG would pass
// CutoutMaxPNG), the stats say its size.
func (b *CutoutBackend) Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(r.Images) != 1 || r.Images[0] == "" {
		return fmt.Errorf("%s cuts out one image: give exactly one", m.Name)
	}
	hm, err := hubModel(ctx, b.Store, m, func(p string) bool { return models.Matches(p, cutoutFiles...) })
	if errors.Is(err, models.ErrGated) {
		return GatedError(m.repo)
	}
	if err != nil {
		return err
	}
	dir, err := fetch(ctx, b.Store, hm, emit)
	if err != nil {
		return err
	}
	m.Here, m.Where = true, dir
	if b.w == nil {
		b.w = &pyWorker{env: CutoutEnv, script: "birefnet_worker.py"}
	}
	if err := b.w.start(ctx, b.Root, emit); err != nil {
		return err
	}
	out := filepath.Join(outDir, fmt.Sprintf("%s-cutout.png", time.Now().Format("20060102-150405")))
	b.state.set(m.ID) // loading counts as loaded: it takes the memory
	ev, err := b.w.call(ctx, map[string]any{"model": m.ID, "path": dir, "size": cmpSide(m.side), "image": r.Images[0],
		"output": out, "maxBytes": CutoutMaxPNG}, emit)
	if err != nil {
		if !b.w.running() {
			b.state.set("") // canceled or crashed: the worker and its model are gone
		}
		return err
	}
	st := map[string]any{"seconds": ev.Seconds, "peakGB": ev.PeakGB}
	if ev.Width > 0 && ev.Height > 0 {
		st["width"], st["height"] = ev.Width, ev.Height
	}
	emit(Event{Type: "file", File: ev.File})
	emit(Event{Type: "stats", Data: st})
	emit.Log(fmt.Sprintf("cut out %dx%d in %.1f s, GPU %.1f GB", ev.Width, ev.Height, ev.Seconds, ev.PeakGB))
	return nil
}

// cmpSide is the side a model sees (1024 when the catalogue does not say).
func cmpSide(side int) int {
	if side > 0 {
		return side
	}
	return 1024
}
