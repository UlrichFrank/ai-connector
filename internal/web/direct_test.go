package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
)

// textFake embeds and classifies directly; its jobs wait until release is closed.
type textFake struct{ release chan struct{} }

func (textFake) Name() string   { return "text" }
func (textFake) Heavy() bool    { return false }
func (textFake) Loaded() string { return "" }
func (textFake) Unload()        {}
func (b textFake) Run(ctx context.Context, _ *engine.Model, _ engine.Request, _ string, _ engine.Emit) error {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}
func (textFake) Embed(_ context.Context, _ *engine.Model, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if t == "fail" {
			return nil, errors.New("broken")
		}
		out[i] = []float32{3, 4} // not normalised: the handler does it
	}
	return out, nil
}
func (textFake) Classify(_ context.Context, _ *engine.Model, states []string, _ string, opts []engine.Option) ([]engine.Answer, error) {
	out := make([]engine.Answer, len(states))
	for i := range states {
		out[i] = engine.Answer{Choice: opts[len(opts)-1].Label, Probabilities: map[string]float64{opts[len(opts)-1].Label: 0.9}, Confidence: 0.9}
	}
	return out, nil
}

func TestDirect(t *testing.T) {
	cat := &engine.Catalog{Models: []*engine.Model{
		{ID: "emb", Kind: engine.Embed, Backend: "text"},
		{ID: "cls", Kind: engine.Classify, Backend: "text"},
	}}
	be := textFake{release: make(chan struct{})}
	m := jobs.New(cat, []engine.Backend{be}, t.TempDir())
	t.Cleanup(func() {
		close(be.release)
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !m.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	srv := httptest.NewServer((&Server{Jobs: m}).Handler())
	defer srv.Close()
	// a job holds the queue: the direct path does not wait for it
	if _, err := m.Submit(engine.Request{Model: "emb", Texts: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	post := func(path, body string, v any) int {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set(RouteHeader, "m1") // as the server sends it
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		json.NewDecoder(res.Body).Decode(v)
		return res.StatusCode
	}
	var emb struct {
		Model   string
		Dim     int
		Vectors [][]float32
	}
	if c := post("/api/direct/embed", `{"model":"em","texts":["a","/files/m1/x"]}`, &emb); c != 200 || emb.Model != "emb" || emb.Dim != 2 ||
		len(emb.Vectors) != 2 || emb.Vectors[0][0] != 0.6 || emb.Vectors[0][1] != 0.8 {
		t.Fatalf("embed %d %+v", c, emb)
	}
	var cls struct {
		Model   string
		Answers []engine.Answer
	}
	if c := post("/api/direct/classify", `{"model":"cls","states":["s"],"instructions":"q","options":[{"label":"a"},{"label":"b"}]}`, &cls); c != 200 ||
		len(cls.Answers) != 1 || cls.Answers[0].Choice != "b" || cls.Answers[0].Confidence != 0.9 {
		t.Fatalf("classify %d %+v", c, cls)
	}
	var e map[string]string
	for _, c := range []struct {
		path, body string
		status     int
		code       string
	}{
		{"/api/direct/embed", `{"model":"nope","texts":["a"]}`, 404, "model"},
		{"/api/direct/embed", `{"model":"cls","texts":["a"]}`, 400, "kind"},
		{"/api/direct/embed", `{"model":"emb","texts":[]}`, 400, "request"},
		{"/api/direct/embed", `{"model":"emb","texts":["fail"]}`, 500, "run"},
		{"/api/direct/classify", `{"model":"emb","states":["s"],"options":[{"label":"a"}]}`, 400, "kind"},
		{"/api/direct/classify", `{"model":"cls","states":["s"]}`, 400, "request"},
	} {
		e = nil
		if got := post(c.path, c.body, &e); got != c.status || e["error"] != c.code {
			t.Errorf("%s %s: %d %v", c.path, c.body, got, e)
		}
	}
	if len(m.Jobs()) != 1 {
		t.Errorf("the direct path made jobs: %d", len(m.Jobs()))
	}
}
