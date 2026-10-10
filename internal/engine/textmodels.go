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
// Laya classifier. Both stay loaded (about 2 GB together; one copy of each).
//
// Each model has its own lock, so an embedding never waits for a classification and the other way round. Jobs work
// in parts (textJobPart texts at a time) and let go of the model between parts; a direct call (Embed, Classify,
// Choose: the API's direct path, a chat's guard) waiting for the model goes first at the next part — it never waits
// behind a long batch job, only for the part that runs.
type TextBackend struct {
	Store *models.Store

	embLock, clsLock prioLock // who uses the embedder, the classifier

	mu    sync.Mutex   // emb and cls (set with their lock held as well)
	emb   textEmbedder // *qwen3.Model
	cls   textChooser  // *laya.Model
	state status
}

// textEmbedder and textChooser are what the backend uses of the loaded models (tests put others in their place).
type (
	textEmbedder interface {
		Embed(texts []string, batch int) [][]float32
	}
	textChooser interface {
		Choose(states []string, instructions string, options []laya.Option, batch int) ([]laya.Answer, error)
	}
)

// textJobPart: texts (or states) a job hands to a model at once; a multiple of the models' own batch (32), so the
// batches and the results are the same as in one call.
var textJobPart = 128

// prioLock is a lock that direct calls (high) get before jobs (low) waiting for it.
type prioLock struct {
	mu   sync.Mutex
	cond *sync.Cond
	held bool
	high int // direct calls waiting
}

func (l *prioLock) lock(high bool) {
	l.mu.Lock()
	if l.cond == nil {
		l.cond = sync.NewCond(&l.mu)
	}
	if high {
		l.high++
	}
	for l.held || !high && l.high > 0 {
		l.cond.Wait()
	}
	if high {
		l.high--
	}
	l.held = true
	l.mu.Unlock()
}

func (l *prioLock) unlock() {
	l.mu.Lock()
	l.held = false
	if l.cond != nil {
		l.cond.Broadcast()
	}
	l.mu.Unlock()
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
	b.embLock.lock(true)
	defer b.embLock.unlock()
	b.clsLock.lock(true)
	defer b.clsLock.unlock()
	b.mu.Lock()
	b.emb, b.cls = nil, nil
	b.report()
	b.mu.Unlock()
	debug.FreeOSMemory()
}

func (b *TextBackend) ensure(ctx context.Context, m *Model, emit Emit) (string, error) {
	return fetch(ctx, b.Store, *m.pinned, emit)
}

func (b *TextBackend) Run(ctx context.Context, m *Model, r Request, _ string, emit Emit) error {
	switch m.Kind {
	case Embed:
		return b.embed(ctx, m, r, emit)
	case Classify:
		return b.classify(ctx, m, r, emit)
	}
	return fmt.Errorf("%s: not a text model", m.ID)
}

// embedder is the loaded embedder; it fetches and loads it first (call with embLock held).
func (b *TextBackend) embedder(ctx context.Context, m *Model, emit Emit) (textEmbedder, error) {
	b.mu.Lock()
	loaded := b.emb
	b.mu.Unlock()
	if loaded != nil {
		return loaded, nil
	}
	dir, err := b.ensure(ctx, m, emit)
	if err != nil {
		return nil, err
	}
	emit.Status("loading " + m.Name + " …")
	start := time.Now()
	emb, err := qwen3.Load(dir)
	if err != nil {
		return nil, err
	}
	emb.Embed([]string{"warm up"}, 1)
	b.mu.Lock()
	b.emb = emb
	b.report()
	b.mu.Unlock()
	emit.Log(fmt.Sprintf("loaded %s in %.1f s (matrix products: %s)", m.Name, time.Since(start).Seconds(), tensor.Backend()))
	return emb, nil
}

// classifier is the loaded classifier; it fetches and loads it first (call with clsLock held). m nil: the pinned
// Laya model (Choose).
func (b *TextBackend) classifier(ctx context.Context, m *Model, emit Emit) (textChooser, error) {
	b.mu.Lock()
	loaded := b.cls
	b.mu.Unlock()
	if loaded != nil {
		return loaded, nil
	}
	var dir string
	var err error
	name := "Laya"
	if m != nil {
		dir, err = b.ensure(ctx, m, emit)
		name = m.Name
	} else {
		dir, err = fetch(ctx, b.Store, models.Laya, emit)
	}
	if err != nil {
		return nil, err
	}
	emit.Status("loading " + name + " …")
	start := time.Now()
	cls, err := laya.Load(dir)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.cls = cls
	b.report()
	b.mu.Unlock()
	emit.Log(fmt.Sprintf("loaded %s in %.1f s (matrix products: %s)", name, time.Since(start).Seconds(), tensor.Backend()))
	return cls, nil
}

// vectors embeds the texts: a direct call (high) at once, a job part by part.
func (b *TextBackend) vectors(ctx context.Context, m *Model, texts []string, emit Emit, direct bool) ([][]float32, error) {
	part := len(texts)
	if !direct {
		part = textJobPart
	}
	out := make([][]float32, 0, len(texts))
	for lo := 0; lo < len(texts); lo += part {
		hi := min(lo+part, len(texts))
		b.embLock.lock(direct)
		emb, err := b.embedder(ctx, m, emit)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			b.embLock.unlock()
			return nil, err
		}
		out = append(out, emb.Embed(texts[lo:hi], 32)...)
		b.embLock.unlock()
	}
	return out, nil
}

// answers asks the question about every state: a direct call at once, a job part by part.
func (b *TextBackend) answers(ctx context.Context, m *Model, states []string, instructions string, options []Option, emit Emit, direct bool) ([]Answer, error) {
	opts := make([]laya.Option, len(options))
	for i, o := range options {
		opts[i] = laya.Option{Label: o.Label, Description: o.Description}
	}
	part := len(states)
	if !direct {
		part = textJobPart
	}
	out := make([]Answer, 0, len(states))
	for lo := 0; lo < len(states); lo += part {
		hi := min(lo+part, len(states))
		b.clsLock.lock(direct)
		cls, err := b.classifier(ctx, m, emit)
		if err == nil {
			err = ctx.Err()
		}
		var ans []laya.Answer
		if err == nil {
			ans, err = cls.Choose(states[lo:hi], instructions, opts, 32)
		}
		b.clsLock.unlock()
		if err != nil {
			return nil, err
		}
		for _, a := range ans {
			out = append(out, Answer{Choice: a.Choice, Probabilities: a.Probabilities, Confidence: a.AnswerConfidence})
		}
	}
	return out, nil
}

// Embed embeds texts directly (Embedder): the embedder is fetched and loaded on first use and stays loaded, as for
// embed jobs; a call goes before the next part of a running embed job.
func (b *TextBackend) Embed(ctx context.Context, m *Model, texts []string) ([][]float32, error) {
	if m.Kind != Embed {
		return nil, fmt.Errorf("%s: not an embedding model", m.ID)
	}
	return b.vectors(ctx, m, texts, func(Event) {}, true)
}

// Classify answers a choice question directly (Classifier), like Embed.
func (b *TextBackend) Classify(ctx context.Context, m *Model, states []string, instructions string, options []Option) ([]Answer, error) {
	if m.Kind != Classify {
		return nil, fmt.Errorf("%s: not a classifier", m.ID)
	}
	return b.answers(ctx, m, states, instructions, options, func(Event) {}, true)
}

func (b *TextBackend) embed(ctx context.Context, m *Model, r Request, emit Emit) error {
	if len(r.Texts) == 0 {
		return fmt.Errorf("no texts")
	}
	b.embLock.lock(false) // loaded before the status says "embedding"
	_, err := b.embedder(ctx, m, emit)
	b.embLock.unlock()
	if err != nil {
		return err
	}
	emit.Status(fmt.Sprintf("embedding %d texts …", len(r.Texts)))
	start := time.Now()
	vecs, err := b.vectors(ctx, m, r.Texts, emit, false)
	if err != nil {
		return err
	}
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
	b.clsLock.lock(false)
	_, err := b.classifier(ctx, m, emit)
	b.clsLock.unlock()
	if err != nil {
		return err
	}
	emit.Status(fmt.Sprintf("asking about %d texts …", len(r.States)))
	start := time.Now()
	ans, err := b.answers(ctx, m, r.States, r.Instructions, r.Options, emit, false)
	if err != nil {
		return err
	}
	took := time.Since(start)
	rows := make([]map[string]any, len(ans))
	for i, a := range ans {
		rows[i] = map[string]any{"text": r.States[i], "choice": a.Choice, "confidence": a.Confidence, "probabilities": a.Probabilities}
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
// fetched and loaded on first use and stays loaded, as for classify jobs; a call goes before the next part of a
// running classify job.
func (b *TextBackend) Choose(ctx context.Context, states []string, instructions string, options []laya.Option) ([]laya.Answer, error) {
	b.clsLock.lock(true)
	defer b.clsLock.unlock()
	cls, err := b.classifier(ctx, nil, func(Event) {})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return cls.Choose(states, instructions, options, 32)
}
