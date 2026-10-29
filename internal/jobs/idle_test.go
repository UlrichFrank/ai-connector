package jobs

import (
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
)

// A model with no job for the idle time leaves memory, a pinned one stays, the running job's backend is not touched,
// and a backend loaded without a job here starts its idle time when it is first seen.
func TestUnloadIdle(t *testing.T) {
	a, b, c := &fake{name: "a"}, &fake{name: "b"}, &fake{name: "c"}
	m := setup(t, a, b, c)
	t0 := time.Now()
	a.loaded, b.loaded, c.loaded = "chat-a", "img-b", "pinned-c"
	m.mu.Lock()
	m.lastUsed = map[string]time.Time{"a": t0, "b": t0, "c": t0}
	m.mu.Unlock()
	pin := map[string]bool{"pinned-c": true}
	if got := m.UnloadIdle(10*time.Minute, pin, t0.Add(9*time.Minute)); len(got) != 0 {
		t.Fatalf("unloaded before the idle time: %v", got)
	}
	// b's job runs: only a goes
	m.mu.Lock()
	m.busy, m.current = true, "b"
	m.mu.Unlock()
	got := m.UnloadIdle(10*time.Minute, pin, t0.Add(11*time.Minute))
	if len(got) != 1 || got[0] != "chat-a" || a.loaded != "" || b.loaded != "img-b" || c.loaded != "pinned-c" {
		t.Fatalf("unloaded %v; loaded a=%q b=%q c=%q", got, a.loaded, b.loaded, c.loaded)
	}
	// a backend loaded but never seen: its idle time begins now
	m.mu.Lock()
	m.busy, m.current = false, ""
	delete(m.lastUsed, "b")
	m.mu.Unlock()
	if got := m.UnloadIdle(10*time.Minute, pin, t0.Add(12*time.Minute)); len(got) != 0 {
		t.Fatalf("unseen backend unloaded at once: %v", got)
	}
	if got := m.UnloadIdle(10*time.Minute, pin, t0.Add(23*time.Minute)); len(got) != 1 || got[0] != "img-b" {
		t.Fatalf("unseen backend after its idle time: %v", got)
	}
}

// A job's end starts its backend's idle time.
func TestIdleAfterJob(t *testing.T) {
	a := &fake{name: "a"}
	m := setup(t, a)
	j, err := m.Submit(engine.Request{Model: "chat-a", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, m, j.ID)
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) { // the queue is free again
		m.mu.Lock()
		free := !m.busy
		m.mu.Unlock()
		if free || time.Now().After(deadline) {
			break
		}
	}
	if got := m.UnloadIdle(time.Hour, nil, time.Now()); len(got) != 0 || a.loaded != "chat-a" {
		t.Fatalf("unloaded right after its job: %v", got)
	}
	if got := m.UnloadIdle(time.Hour, nil, time.Now().Add(2*time.Hour)); len(got) != 1 || a.loaded != "" {
		t.Fatalf("not unloaded after the idle time: %v", got)
	}
}
