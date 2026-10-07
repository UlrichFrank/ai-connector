package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/pyenv"
)

// MfluxEnv is the pinned mflux runtime.
var MfluxEnv = pyenv.Env{Name: "mflux", Python: "3.12", Packages: []string{"mflux==0.20.0"}}

// FluxBackend runs FLUX.2 klein and Qwen-Image through mflux in a Python worker that stays alive between jobs (the model stays
// loaded: the next image starts at once).
type FluxBackend struct {
	Root  string // ~/.config/mlxlab
	Store *models.Store

	mu    sync.Mutex
	w     *pyWorker
	state status
}

func (b *FluxBackend) Name() string { return "mflux" }
func (b *FluxBackend) Heavy() bool  { return true }

func (b *FluxBackend) Loaded() string { return b.state.get() }

func (b *FluxBackend) Unload() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.w != nil {
		b.w.stop()
	}
	b.state.set("")
}

func (b *FluxBackend) Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Prompt == "" {
		return errors.New("no prompt")
	}
	// a model with tasks does only those (Qwen-Image: text to image, or editing)
	if len(m.Tasks) > 0 {
		if len(r.Images) > 0 && !slices.Contains(m.Tasks, "edit") {
			return fmt.Errorf("%s makes pictures from text only: remove the reference images, or pick an editing model (Qwen-Image-Edit, FLUX.2 klein)", m.Name)
		}
		if len(r.Images) == 0 && !slices.Contains(m.Tasks, "generate") {
			return fmt.Errorf("%s edits pictures: add at least one reference image, or pick a text to image model", m.Name)
		}
	}
	d := m.Defaults
	r.Width, r.Height = orInt(r.Width, d.Width), orInt(r.Height, d.Height)
	r.Width, r.Height = r.Width/16*16, r.Height/16*16
	r.Steps = orInt(r.Steps, d.Steps)
	if r.Guidance == 0 {
		r.Guidance = d.Guidance
	}
	q := orInt(r.Quantize, d.Quantize)
	if q < 0 {
		q = 0
	}
	if r.Seed == 0 {
		r.Seed = rand.Int64N(1_000_000_000)
	}
	// the diffusers layout mflux loads (not the single-file copy of the transformer next to it); a model that names
	// its files (Qwen-Image) only those
	files := m.files
	if len(files) == 0 {
		files = []string{"text_encoder/", "tokenizer/", "transformer/", "vae/", "scheduler/", "model_index.json"}
	}
	hm, err := hubModel(ctx, b.Store, m, func(p string) bool { return models.Matches(p, files...) })
	if err != nil {
		return err
	}
	m.Size = hm.Size()
	dir, err := fetch(ctx, b.Store, hm, emit)
	if err != nil {
		return err
	}
	m.Here, m.Where = true, dir
	if b.w == nil {
		b.w = &pyWorker{env: MfluxEnv, script: "flux_worker.py"}
	}
	if err := b.w.start(ctx, b.Root, emit); err != nil {
		return err
	}
	out := filepath.Join(outDir, fmt.Sprintf("%s-%d.png", time.Now().Format("20060102-150405"), r.Seed))
	emit(Event{Type: "stats", Data: map[string]any{"seed": r.Seed, "width": r.Width, "height": r.Height, "steps": r.Steps, "quantize": q}})
	b.state.set(m.ID) // loading counts as loaded: it takes the memory
	ev, err := b.w.call(ctx, map[string]any{"model": m.mflux, "path": dir, "quantize": q, "prompt": r.Prompt, "width": r.Width,
		"height": r.Height, "steps": r.Steps, "seed": r.Seed, "guidance": r.Guidance, "images": r.Images, "output": out}, emit)
	if err != nil {
		if !b.w.running() {
			b.state.set("") // canceled or crashed: the worker and its model are gone
		}
		if isGated(err.Error()) {
			return GatedError(m.repo)
		}
		return err
	}
	b.state.set(m.ID)
	emit(Event{Type: "file", File: ev.File})
	emit(Event{Type: "stats", Data: map[string]any{"seconds": ev.Seconds, "peakGB": ev.PeakGB}})
	emit.Log(fmt.Sprintf("%dx%d, %d steps in %.1f s, peak %.1f GB", r.Width, r.Height, r.Steps, ev.Seconds, ev.PeakGB))
	return nil
}

func orInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
