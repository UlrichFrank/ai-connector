package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
)

// runBackend answers with err and counts how often it was unloaded.
type runBackend struct {
	err      error
	unloaded atomic.Int32
}

func (b *runBackend) Name() string { return "b" }
func (b *runBackend) Run(context.Context, *engine.Model, engine.Request, string, engine.Emit) error {
	return b.err
}
func (b *runBackend) Loaded() string { return "" }
func (b *runBackend) Heavy() bool    { return true }
func (b *runBackend) Unload()        { b.unloaded.Add(1) }

// A one-off job leaves nothing behind in memory, whether it worked or not: llama-server stayed with its model.
func TestRunUnloads(t *testing.T) {
	for _, want := range []error{nil, errors.New("no")} {
		b := &runBackend{err: want}
		cat := &engine.Catalog{Models: []*engine.Model{{ID: "m", Kind: engine.Image, Backend: "b"}}}
		mgr := jobs.New(cat, []engine.Backend{b}, t.TempDir())
		o := &runOptions{quiet: true}
		if err := o.run(mgr, []string{"m", "a fox"}); (err != nil) != (want != nil) {
			t.Errorf("run: %v, want %v", err, want)
		}
		if b.unloaded.Load() == 0 {
			t.Errorf("not unloaded after a job that ended with %v", want)
		}
	}
}
