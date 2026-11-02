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

// CutoutEnv() is the cutout worker's runtime: PyTorch and what BiRefNet's own code (birefnet.py, loaded with
// trust_remote_code) imports — timm, kornia, einops, torchvision.
func CutoutEnv() pyenv.Env { return runtimeEnv("cutout") } // runtimes/cutout.json

// cutoutCatalog is the catalogue's BiRefNet models (catalog/birefnet*.json): the files the worker loads (its code, its
// config and the weights), side: what the model sees.
func cutoutCatalog(store *models.Store, hub string) []*Model {
	return builtinModels("cutout", store, hub)
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
	hm, err := hubModel(ctx, b.Store, m, func(p string) bool { return models.Matches(p, m.files...) })
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
		b.w = &pyWorker{env: CutoutEnv(), script: "birefnet_worker.py"}
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
