package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/llm"
)

// fakeOllama answers like Ollama: /v1/models, /api/tags, /api/show (capabilities), streamed /v1/chat/completions
// with reasoning, /v1/embeddings.
func fakeOllama(t *testing.T, gotChat *map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"qwen3:8b"},{"id":"nomic-embed-text:latest"},{"id":"llava:7b-vision"}]}`)
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[{"name":"qwen3:8b","size":5200000000},{"name":"nomic-embed-text:latest","size":274000000},{"name":"llava:7b-vision","size":4700000000}]}`)
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Model string }
		json.NewDecoder(r.Body).Decode(&b)
		caps := map[string]string{"qwen3:8b": `["completion","tools","thinking"]`, "nomic-embed-text:latest": `["embedding"]`,
			"llava:7b-vision": `["vision"]`}[b.Model]
		fmt.Fprintf(w, `{"capabilities":%s}`, caps)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(gotChat)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{
			`{"choices":[{"delta":{"reasoning":"Let me think."}}]}`,
			`{"choices":[{"delta":{"content":"Hello"}}]}`,
			`{"choices":[{"delta":{"content":", world"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5}}`,
			`[DONE]`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", d)
		}
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		var data []string
		// answered out of order: the index puts them back
		for i := len(b.Input) - 1; i >= 0; i-- {
			data = append(data, fmt.Sprintf(`{"index":%d,"embedding":[%d,1,0]}`, i, i))
		}
		fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(data, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAIBackendDiscoverChatEmbed(t *testing.T) {
	var gotChat map[string]any
	srv := fakeOllama(t, &gotChat)
	cat := &Catalog{}
	b := &OpenAIBackend{Catalog: cat, Runtimes: []Runtime{{Name: "ollama", URL: srv.URL + "/v1"}}}
	ms := b.Discover(context.Background())
	if len(ms) != 2 {
		t.Fatalf("discovered %d models, want the chat and the embedder (not the vision-only one): %+v", len(ms), ms)
	}
	chat, emb := cat.Find("ollama/qwen3:8b"), cat.Find("ollama/nomic-embed-text:latest")
	if chat == nil || chat.Kind != Chat || chat.Backend != BackendOpenAI || !chat.Here || chat.Size != 5200000000 {
		t.Fatalf("chat model = %+v", chat)
	}
	if emb == nil || emb.Kind != Embed {
		t.Fatalf("embed model = %+v", emb)
	}

	var text strings.Builder
	var stats map[string]any
	emit := Emit(func(e Event) {
		switch e.Type {
		case "text":
			text.WriteString(e.Text)
		case "stats":
			stats = e.Data
		}
	})
	err := b.Run(context.Background(), chat, Request{Messages: []llm.Message{{Role: "user", Content: "hi"}}, MaxTokens: 50, Think: true}, "", emit)
	if err != nil {
		t.Fatal(err)
	}
	if got := text.String(); got != "<think>Let me think.</think>\n\nHello, world" {
		t.Errorf("text = %q", got)
	}
	if gotChat["model"] != "qwen3:8b" || gotChat["max_tokens"] != float64(50) || gotChat["stream"] != true {
		t.Errorf("request = %v", gotChat)
	}
	if stats["tokens"] != 5 || stats["promptTokens"] != 12 {
		t.Errorf("stats = %v", stats)
	}
	if b.Loaded() != "ollama/qwen3:8b" {
		t.Errorf("loaded = %q", b.Loaded())
	}

	vecs, err := b.Embed(context.Background(), emb, []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vecs {
		if int(v[0]) != i {
			t.Errorf("vector %d = %v: not in input order", i, v)
		}
	}
	var res map[string]any
	if err := b.Run(context.Background(), emb, Request{Texts: []string{"a", "b"}}, "", func(e Event) {
		if e.Type == "result" {
			res = e.Data
		}
	}); err != nil {
		t.Fatal(err)
	}
	if res["dim"] != 3 {
		t.Errorf("embed result = %v", res)
	}

	if err := b.Run(context.Background(), chat, Request{Messages: []llm.Message{{Role: "user", Content: "hi"}}, Zone: "web"}, "", emit); err == nil {
		t.Error("a chat with tools ran on a runtime")
	}
}

func TestOpenAIBackendGoneRuntime(t *testing.T) {
	cat := &Catalog{}
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	b := &OpenAIBackend{Catalog: cat, Runtimes: []Runtime{{Name: "lmstudio", URL: srv.URL + "/v1"}}}
	if ms := b.Discover(context.Background()); len(ms) != 0 || len(cat.List()) != 0 {
		t.Errorf("a runtime that is not there has models: %v", ms)
	}
}

func TestCatalogKeepAndRuntime(t *testing.T) {
	cat := &Catalog{Models: []*Model{{ID: "flux", Backend: "mflux"}, {ID: "qwen3-embedding-0.6b", Backend: "go"}}}
	cat.Keep(func(m *Model) bool { return m.Backend == "go" })
	cat.SetRuntime(BackendOpenAI, []*Model{{ID: "ollama/qwen3:8b", Backend: BackendOpenAI}})
	var ids []string
	for _, m := range cat.List() {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, " ") != "qwen3-embedding-0.6b ollama/qwen3:8b" {
		t.Errorf("models = %v", ids)
	}
	if cat.Find("flux") != nil || cat.Find("ollama/qwen3") == nil {
		t.Error("Find does not see the kept and the runtime models")
	}
	cat.SetRuntime(BackendOpenAI, nil)
	if cat.Find("ollama/qwen3:8b") != nil {
		t.Error("a runtime's model stayed after it went")
	}
}

func TestParseRuntimes(t *testing.T) {
	for _, c := range []struct {
		spec string
		want string
		bad  bool
	}{
		{"", "ollama=http://127.0.0.1:11434/v1 lmstudio=http://127.0.0.1:1234/v1 llamacpp=http://127.0.0.1:8080/v1 vllm=http://127.0.0.1:8000/v1", false},
		{"none", "", false},
		{"ollama", "ollama=http://127.0.0.1:11434/v1", false},
		{"gpu=http://10.0.0.5:11434, lmstudio", "gpu=http://10.0.0.5:11434/v1 lmstudio=http://127.0.0.1:1234/v1", false},
		{"cloud=https://example.com/v1/#sk-1", "cloud=https://example.com/v1", false},
		{"nonsense", "", true},
		{"x=not a url", "", true},
	} {
		rts, err := ParseRuntimes(c.spec)
		if (err != nil) != c.bad {
			t.Errorf("%q: err %v", c.spec, err)
			continue
		}
		var got []string
		for _, r := range rts {
			got = append(got, r.Name+"="+r.URL)
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%q = %v, want %s", c.spec, got, c.want)
		}
	}
	rts, _ := ParseRuntimes("cloud=https://example.com/v1#sk-1")
	if rts[0].Key != "sk-1" {
		t.Errorf("key = %q", rts[0].Key)
	}
}
