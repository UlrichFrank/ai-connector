package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/llm"
)

// echo is a chat backend that answers and returns its history.
type echo struct{}

func (echo) Name() string   { return "echo" }
func (echo) Heavy() bool    { return false }
func (echo) Loaded() string { return "" }
func (echo) Unload()        {}
func (echo) Run(_ context.Context, _ *engine.Model, r engine.Request, _ string, emit engine.Emit) error {
	emit(engine.Event{Type: "result", Data: map[string]any{"messages": append(r.Messages, llm.Message{Role: "assistant", Content: "a"})}})
	return nil
}

func TestApproveAndZone(t *testing.T) {
	cat := &engine.Catalog{Models: []*engine.Model{{ID: "chat", Kind: engine.Chat, Backend: "echo"}}}
	m := jobs.New(cat, []engine.Backend{echo{}}, t.TempDir())
	t.Cleanup(func() { // the last job's save must not land in the removed directory
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !m.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	srv := httptest.NewServer((&Server{Jobs: m}).Handler())
	defer srv.Close()
	post := func(path, body string) int {
		res, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	j, err := m.Submit(engine.Request{Model: "chat", Zone: "web", Messages: []llm.Message{{Role: "user", Content: "q"}}})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if d, _ := m.Job(j.ID); !d.Active() {
			break
		}
	}
	if c := post("/api/jobs/nope/approve", `{"call":"c","allow":true}`); c != http.StatusNotFound {
		t.Fatal(c)
	}
	if c := post("/api/jobs/"+j.ID+"/approve", `{}`); c != http.StatusBadRequest {
		t.Fatal(c)
	}
	if c := post("/api/jobs/"+j.ID+"/approve", `{"call":"c","allow":true}`); c != http.StatusConflict {
		t.Fatal(c) // the job is over: nothing waits
	}
	// a web chat stays a web chat
	if c := post("/api/jobs", `{"model":"chat","zone":"home","parent":"`+j.ID+`","messages":[{"Role":"user","Content":"q"}]}`); c != http.StatusBadRequest {
		t.Fatal(c)
	}
	if c := post("/api/jobs", `{"model":"chat","parent":"`+j.ID+`","messages":[{"Role":"user","Content":"q"}]}`); c != http.StatusOK {
		t.Fatal(c)
	}
}
