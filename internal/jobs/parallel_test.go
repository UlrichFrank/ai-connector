package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
)

// agentFake runs beside the queue (engine.Parallel); while hold is open its jobs wait for it (or a cancel).
type agentFake struct {
	fake
	hold chan struct{}
}

func (f *agentFake) Parallel() bool { return true }

func (f *agentFake) Run(ctx context.Context, m *engine.Model, r engine.Request, out string, emit engine.Emit) error {
	if r.Prompt == "hold" {
		select {
		case <-f.hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	emit(engine.Event{Type: "text", Text: "agent:" + r.Prompt})
	return nil
}

// A Parallel backend's jobs start at once, beside a job that holds the queue, several at a time; they are in the
// history and can be canceled like any other.
func TestParallelLane(t *testing.T) {
	gpu := &fake{name: "a", block: true}
	ag := &agentFake{fake: fake{name: "p"}, hold: make(chan struct{})}
	cat := &engine.Catalog{Models: []*engine.Model{
		{ID: "chat-a", Kind: engine.Chat, Backend: "a"},
		{ID: "agent-p", Kind: engine.Chat, Backend: "p"},
	}}
	m := New(cat, []engine.Backend{gpu, ag}, t.TempDir())
	settle(t, m)

	long, _ := m.Submit(engine.Request{Model: "chat-a", Prompt: "x"})
	queued, _ := m.Submit(engine.Request{Model: "chat-a", Prompt: "y"})
	if m.Beside(long) || !m.Beside(&Job{Model: "agent-p"}) {
		t.Fatal("Beside")
	}
	waitState := func(id string, st State) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if j, _ := m.Job(id); j != nil && j.State == st {
				return
			}
		}
		j, _ := m.Job(id)
		t.Fatalf("job %s: %s, want %s", id, j.State, st)
	}
	waitState(long.ID, Running)

	// the queue is held; an agent job still runs and finishes
	quick, err := m.Submit(engine.Request{Model: "agent-p", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if j := wait(t, m, quick.ID); j.State != Done || j.Text != "agent:hi" {
		t.Fatalf("agent job %+v", j)
	}
	// two at once, both running beside the held queue
	a, _ := m.Submit(engine.Request{Model: "agent-p", Prompt: "hold"})
	b, _ := m.Submit(engine.Request{Model: "agent-p", Prompt: "hold"})
	waitState(a.ID, Running)
	waitState(b.ID, Running)
	if j, _ := m.Job(queued.ID); j.State != Queued {
		t.Fatalf("the queued GPU job: %s", j.State)
	}
	// cancel one; the other finishes when let go
	if err := m.Cancel(a.ID); err != nil {
		t.Fatal(err)
	}
	if j := wait(t, m, a.ID); j.State != Canceled {
		t.Fatalf("canceled: %s", j.State)
	}
	close(ag.hold)
	if j := wait(t, m, b.ID); j.State != Done {
		t.Fatalf("b: %s", j.State)
	}
	// all in the history
	ids := map[string]bool{}
	for _, j := range m.Jobs() {
		ids[j.ID] = true
	}
	for _, id := range []string{quick.ID, a.ID, b.ID, long.ID, queued.ID} {
		if !ids[id] {
			t.Fatalf("%s not listed", id)
		}
	}
	m.Cancel(queued.ID)
	m.Cancel(long.ID)
	wait(t, m, long.ID)
}
