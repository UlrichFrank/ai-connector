package engine

import (
	"context"
	"fmt"
	"math"
	"runtime/debug"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/laya"
	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/qwen3"
	"github.com/philipparndt/ai-connector/internal/tensor"
)

// TextBackend runs vboard's small models in Go (matrix products on Accelerate or Metal): the Qwen3 embedder and the
// Laya classifier. Both stay loaded (about 2 GB together).
type TextBackend struct {
	Store *models.Store

	mu    sync.Mutex
	emb   *qwen3.Model
	cls   *laya.Model
	state status
}

func (b *TextBackend) Name() string { return "go" }
func (b *TextBackend) Heavy() bool  { return false }

func (b *TextBackend) Loaded() string { return b.state.get() }

// report sets what Loaded says (call with b.mu held).
func (b *TextBackend) report() {
	switch {
	case b.emb != nil && b.cls != nil:
		b.state.set("qwen3-embedding-0.6b, laya-typed-decisions")
	case b.emb != nil:
		b.state.set("qwen3-embedding-0.6b")
	case b.cls != nil:
		b.state.set("laya-typed-decisions")
	default:
		b.state.set("")
	}
}

func (b *TextBackend) Unload() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.emb, b.cls = nil, nil
	b.report()
	debug.FreeOSMemory()
}

func (b *TextBackend) ensure(ctx context.Context, m *Model, emit Emit) (string, error) {
	return fetch(ctx, b.Store, *m.pinned, emit)
}

func (b *TextBackend) Run(ctx context.Context, m *Model, r Request, _ string, emit Emit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch m.Kind {
	case Embed:
		return b.embed(ctx, m, r, emit)
	case Classify:
		return b.classify(ctx, m, r, emit)
	}
	return fmt.Errorf("%s: not a text model", m.ID)
}

func (b *TextBackend) embed(ctx context.Context, m *Model, r Request, emit Emit) error {
	if len(r.Texts) == 0 {
		return fmt.Errorf("no texts")
	}
	if b.emb == nil {
		dir, err := b.ensure(ctx, m, emit)
		if err != nil {
			return err
		}
		emit.Status("loading " + m.Name + " …")
		start := time.Now()
		if b.emb, err = qwen3.Load(dir); err != nil {
			return err
		}
		b.emb.Embed([]string{"warm up"}, 1)
		b.report()
		emit.Log(fmt.Sprintf("loaded %s in %.1f s (matrix products: %s)", m.Name, time.Since(start).Seconds(), tensor.Backend()))
	}
	emit.Status(fmt.Sprintf("embedding %d texts …", len(r.Texts)))
	start := time.Now()
	vecs := b.emb.Embed(r.Texts, 32)
	took := time.Since(start)
	emit(embedResult(vecs, r.Texts))
	emit(Event{Type: "stats", Data: map[string]any{"seconds": took.Seconds(), "textsPerSecond": float64(len(r.Texts)) / took.Seconds()}})
	emit.Log(fmt.Sprintf("embedded %d texts in %d ms", len(r.Texts), took.Milliseconds()))
	return nil
}

func (b *TextBackend) classify(ctx context.Context, m *Model, r Request, emit Emit) error {
	if len(r.States) == 0 || len(r.Options) == 0 {
		return fmt.Errorf("a question needs texts and options")
	}
	if b.cls == nil {
		dir, err := b.ensure(ctx, m, emit)
		if err != nil {
			return err
		}
		emit.Status("loading " + m.Name + " …")
		start := time.Now()
		if b.cls, err = laya.Load(dir); err != nil {
			return err
		}
		b.report()
		emit.Log(fmt.Sprintf("loaded %s in %.1f s (matrix products: %s)", m.Name, time.Since(start).Seconds(), tensor.Backend()))
	}
	emit.Status(fmt.Sprintf("asking about %d texts …", len(r.States)))
	start := time.Now()
	opts := make([]laya.Option, len(r.Options))
	for i, o := range r.Options {
		opts[i] = laya.Option{Label: o.Label, Description: o.Description}
	}
	ans, err := b.cls.Choose(r.States, r.Instructions, opts, 32)
	if err != nil {
		return err
	}
	took := time.Since(start)
	rows := make([]map[string]any, len(ans))
	for i, a := range ans {
		rows[i] = map[string]any{"text": r.States[i], "choice": a.Choice, "confidence": a.AnswerConfidence, "probabilities": a.Probabilities}
	}
	emit(Event{Type: "result", Data: map[string]any{"answers": rows}})
	emit(Event{Type: "stats", Data: map[string]any{"seconds": took.Seconds()}})
	emit.Log(fmt.Sprintf("answered %d in %d ms", len(ans), took.Milliseconds()))
	return nil
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

// Choose asks the Laya classifier directly, in this process: for callers inside a running job (the guard of a chat
// with tools) — submitting a classify job from there would wait behind the job that submits it. The classifier is
// fetched and loaded on first use and stays loaded, as for classify jobs; calls take turns with them.
func (b *TextBackend) Choose(ctx context.Context, states []string, instructions string, options []laya.Option) ([]laya.Answer, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cls == nil {
		dir, err := fetch(ctx, b.Store, models.Laya, func(Event) {})
		if err != nil {
			return nil, err
		}
		if b.cls, err = laya.Load(dir); err != nil {
			return nil, err
		}
		b.report()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.cls.Choose(states, instructions, options, 32)
}
