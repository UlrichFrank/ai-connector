package hub

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/web"
)

// fakeRuntime is a local runtime's backend: its model appears once it is "pulled" (DiscoverNew, as the worker's
// GET /api/models?refresh=1 asks), and chats "pulled!".
type fakeRuntime struct {
	cat    *engine.Catalog
	pulled atomic.Bool
	asked  atomic.Int32
}

func (b *fakeRuntime) Name() string   { return "fakert" }
func (b *fakeRuntime) Heavy() bool    { return false }
func (b *fakeRuntime) Loaded() string { return "" }
func (b *fakeRuntime) Unload()        {}
func (b *fakeRuntime) Run(_ context.Context, _ *engine.Model, _ engine.Request, _ string, emit engine.Emit) error {
	emit(engine.Event{Type: "text", Text: "pulled!"})
	return nil
}
func (b *fakeRuntime) DiscoverNew(context.Context) int {
	b.asked.Add(1)
	if !b.pulled.Load() {
		return 0
	}
	return b.cat.AddRuntime("fakert", []*engine.Model{{ID: "ollama/fresh:1b", Name: "fresh", Kind: engine.Chat, Backend: "fakert", Here: true}})
}

// A model pulled into a local runtime a moment ago is found at once: on "not found" the server asks the workers for
// their models again (they ask their runtimes) and places the request; a model that is nowhere is still a 404, and
// asking again and again does not ask the workers each time.
func TestFreshRuntimeModel(t *testing.T) {
	defer func(d time.Duration) { refreshModelsGap = d }(refreshModelsGap)
	refreshModelsGap = 0
	_, srv := newHub(t, t.TempDir())
	base := srv.URL
	device := signIn(t, base, "laptop")
	_, token := makeClient(t, base, device, "app")
	_, wtok := enroll(t, base, device, "Mac")
	cat := &engine.Catalog{Models: []*engine.Model{{ID: "qwen3.6-35b-a3b", Name: "Qwen3.6", Kind: engine.Chat, Backend: "fakechat", Size: gib}}}
	rt := &fakeRuntime{cat: cat}
	dir := t.TempDir()
	mgr := jobs.New(cat, []engine.Backend{&fakeChat{}, rt}, dir)
	api := (&web.Server{Jobs: mgr, Prompts: prompts.Open(dir), Version: "test"}).Handler()
	stop, _ := connectWorkerWith(t, base, wtok, withRAM(api, 64))
	t.Cleanup(func() {
		stop()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !mgr.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	chat := `{"model":"ollama/fresh:1b","messages":[{"role":"user","content":"hi"}]}`

	// not pulled: 404 after asking once
	c, b := post(t, base+"/v1/chat/completions", token, chat)
	if typ, code := openAIError(t, b); c != 404 || code != "model_not_found" || typ != "invalid_request_error" {
		t.Fatalf("before the pull %d %s", c, b)
	}
	if rt.asked.Load() != 1 {
		t.Fatalf("the worker was asked %d times", rt.asked.Load())
	}

	// pulled: found at once (not after the next probe, 30 s away)
	rt.pulled.Store(true)
	start := time.Now()
	c, b = post(t, base+"/v1/chat/completions", token, chat)
	var res struct {
		Model   string
		Choices []struct{ Message struct{ Content string } }
	}
	json.Unmarshal(b, &res)
	if c != 200 || res.Model != "ollama/fresh:1b" || res.Choices[0].Message.Content != "pulled!" || time.Since(start) > 5*time.Second {
		t.Fatalf("after the pull %d %s (%s)", c, b, time.Since(start))
	}

	// asked for nonsense again and again: the workers at most once in refreshModelsGap
	refreshModelsGap = time.Hour
	asked := rt.asked.Load()
	for range 3 {
		if c, _ := post(t, base+"/v1/chat/completions", token, `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`); c != 404 {
			t.Fatalf("nonsense %d", c)
		}
	}
	if got := rt.asked.Load() - asked; got > 1 {
		t.Fatalf("asked %d times", got)
	}
}
