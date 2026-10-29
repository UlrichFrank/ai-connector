package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/pyenv"
)

// Sound effects: a description in, a short WAV out (docs/voice-avatar.md, kind sound).
//
// MOSS-SoundEffect (OpenMOSS, Apache-2.0) runs through mlx-speech (github.com/appautomaton/mlx-speech, MIT), pure
// MLX: a 4-bit Qwen3-8B MossTTSDelay model writes RVQ audio tokens at 12.5 frames per second, the MOSS audio
// tokenizer (int8) decodes them to 24 kHz mono (not 48 kHz: the codec's rate). Measured on a 64 GB Mac (GPU shared with other
// jobs): 8 s of sound in 8–22 s, 2 s in 5 s; MLX peak about 6 GB, the process up to 11 GB.

// Sound durations: the model takes the length as a hint (expected tokens) in its prompt. Upstream gives no limit;
// its examples are 5–15 s (a sound effect, not a soundtrack) and the time grows linearly (about 1–2.5 s of compute
// per second of sound), so mlxlab keeps a sound between 1 and 30 s. The worker gives the model twice the expected
// tokens to finish, so a sound comes out about as long as asked (measured: 2 s → 2.2 s, 8 s → 8.2 s).
const (
	soundMinSeconds     = 1.0
	soundMaxSeconds     = 30.0
	soundDefaultSeconds = 8.0
)

// clampSoundDuration is the length a sound request gets: the default when unset, else within what the model does well.
func clampSoundDuration(d float64) float64 {
	switch {
	case d <= 0:
		return soundDefaultSeconds
	case d < soundMinSeconds:
		return soundMinSeconds
	case d > soundMaxSeconds:
		return soundMaxSeconds
	}
	return d
}

// soundEnv is the sound worker's runtime: mlx-speech at a fixed commit and its dependencies as in its uv.lock.
var soundEnv = pyenv.Env{Name: "sound", Python: "3.13", Packages: []string{
	"mlx-speech @ git+https://github.com/appautomaton/mlx-speech@4577ad28e6bef2ae2a11b8fa76a6cdb23bb2a9cc",
	"mlx==0.32.2", "numpy==2.4.4", "safetensors==0.7.0", "soundfile==0.13.1", "tokenizers==0.22.2", "huggingface-hub==1.8.0",
}}

// soundModels are the catalogue's sound effect models.
func soundModels() []*Model { return builtinModels("sound", nil, "") }

// soundReposHere: the store has every file of every repository (from the cached file lists, no network).
func soundReposHere(store *models.Store, repos []hubRepo) bool { return reposHere(store, repos) }

// soundKeep is the file filter of a repository (nil: every file).
func soundKeep(hr hubRepo) func(string) bool {
	if len(hr.include) == 0 {
		return nil
	}
	inc := hr.include
	return func(p string) bool { return models.Matches(p, inc...) }
}

// SoundBackend runs the sound effect models: one worker, which keeps its model loaded between sounds (about 6 GB at its peak).
type SoundBackend struct {
	Root  string
	Store *models.Store

	// fetch makes the model's files available and returns their directories by key (tests replace it)
	fetch func(ctx context.Context, m *Model, emit Emit) (map[string]string, error)
	env   *pyenv.Env // tests: a fake runtime

	mu     sync.Mutex
	worker *pyWorker
	state  status
}

func (b *SoundBackend) Name() string   { return "sound" }
func (b *SoundBackend) Heavy() bool    { return false } // about 6 GB (MLX peak), the process up to 11 GB
func (b *SoundBackend) Loaded() string { return b.state.get() }

func (b *SoundBackend) Unload() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.worker != nil {
		b.worker.stop()
	}
	b.state.set("")
}

// files downloads (or finds) every repository the model needs, pinned and checked.
func (b *SoundBackend) files(ctx context.Context, m *Model, emit Emit) (map[string]string, error) {
	if b.fetch != nil {
		return b.fetch(ctx, m, emit)
	}
	dirs := map[string]string{}
	var total int64
	for _, hr := range m.repos {
		hm, err := b.Store.FromHub(ctx, m.Name, hr.repo, hr.rev, soundKeep(hr))
		if errors.Is(err, models.ErrGated) {
			return nil, GatedError(hr.repo)
		}
		if err != nil {
			return nil, err
		}
		total += hm.Size()
		dir, err := fetch(ctx, b.Store, hm, emit)
		if err != nil {
			return nil, err
		}
		dirs[hr.key] = dir
	}
	m.Size, m.Here, m.Where = total, true, b.Store.Dir
	return dirs, nil
}

func (b *SoundBackend) Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if strings.TrimSpace(r.Prompt) == "" {
		return errors.New("describe the sound")
	}
	want := r.Duration
	r.Duration = clampSoundDuration(r.Duration)
	if want > 0 && want != r.Duration {
		emit.Log(fmt.Sprintf("%s makes %g–%g s: %g s instead of %g s", m.Name, soundMinSeconds, soundMaxSeconds, r.Duration, want))
	}
	if r.Seed == 0 {
		r.Seed = rand.Int64N(1_000_000_000)
	}
	dirs, err := b.files(ctx, m, emit)
	if err != nil {
		return err
	}

	if b.worker == nil || !b.worker.running() {
		env := soundEnv
		if b.env != nil {
			env = *b.env
		}
		b.worker = &pyWorker{env: env, script: "sound.py"}
		if err := b.worker.start(ctx, b.Root, emit); err != nil {
			return err
		}
	}
	w := b.worker

	out := filepath.Join(outDir, fmt.Sprintf("%s-%d.wav", time.Now().Format("20060102-150405"), r.Seed))
	emit(Event{Type: "stats", Data: map[string]any{"seed": r.Seed, "duration": r.Duration}})
	req := map[string]any{"model": m.workerModel, "dirs": dirs, "prompt": r.Prompt, "duration": r.Duration,
		"seed": r.Seed, "output": out}
	b.state.set(m.ID) // loading counts as loaded: it takes the memory
	ev, err := w.call(ctx, req, emit)
	if err != nil {
		if !w.running() {
			b.state.set("")
		}
		return err
	}
	emit(Event{Type: "file", File: ev.File})
	stats := map[string]any{"seconds": ev.Seconds}
	if ev.Duration > 0 {
		stats["duration"] = ev.Duration
	}
	if ev.PeakGB > 0 {
		stats["peakGB"] = ev.PeakGB
	}
	emit(Event{Type: "stats", Data: stats})
	emit.Log(fmt.Sprintf("%.1f s of sound in %.1f s", ev.Duration, ev.Seconds))
	return nil
}
