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

var redPNG = []byte("\x89PNG\r\n\x1a\nred")

// fakeLMStudio answers like LM Studio 0.4: /v1/models, /api/v0/models (as it lists the Qwen3.6 MLX checkpoints with
// their vision tower: type "vlm"; capabilities name only tool_use), streamed /v1/chat/completions.
func fakeLMStudio(t *testing.T, gotChat *map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"qwen/qwen3.6-27b"},{"id":"qwen3.6-35b-a3b"},{"id":"plain-llm"},{"id":"text-embedding-nomic-embed-text-v1.5"}]}`)
	})
	mux.HandleFunc("GET /api/v0/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"qwen/qwen3.6-27b","object":"model","type":"vlm","arch":"qwen3_5","compatibility_type":"mlx","capabilities":["tool_use"]},
			{"id":"qwen3.6-35b-a3b","object":"model","type":"vlm","arch":"qwen3_5_moe","compatibility_type":"mlx","capabilities":["tool_use"]},
			{"id":"plain-llm","object":"model","type":"llm","capabilities":["tool_use"]},
			{"id":"text-embedding-nomic-embed-text-v1.5","object":"model","type":"embeddings"}],"object":"list"}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(gotChat)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{`{"choices":[{"delta":{"content":"Red."},"finish_reason":"stop"}]}`, `[DONE]`} {
			fmt.Fprintf(w, "data: %s\n\n", d)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeOllamaVision answers like Ollama with a vision model (capabilities completion + vision) and a text one.
func fakeOllamaVision(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"gemma3:4b"},{"id":"qwen3:8b"}]}`)
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[{"name":"gemma3:4b","size":3300000000},{"name":"qwen3:8b","size":5200000000}]}`)
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Model string }
		json.NewDecoder(r.Body).Decode(&b)
		caps := map[string]string{"gemma3:4b": `["completion","vision"]`, "qwen3:8b": `["completion","tools","thinking"]`}[b.Model]
		fmt.Fprintf(w, `{"capabilities":%s}`, caps)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The runtimes say which models see: LM Studio's type "vlm", Ollama's capability "vision".
func TestRuntimeImagesCapability(t *testing.T) {
	var got map[string]any
	lms, oll := fakeLMStudio(t, &got), fakeOllamaVision(t)
	cat := &Catalog{}
	b := &OpenAIBackend{Catalog: cat, Runtimes: []Runtime{{Name: "lmstudio", URL: lms.URL + "/v1"}, {Name: "ollama", URL: oll.URL + "/v1"}}}
	b.Discover(context.Background())
	want := map[string]bool{"lmstudio/qwen/qwen3.6-27b": true, "lmstudio/qwen3.6-35b-a3b": true, "lmstudio/plain-llm": false,
		"lmstudio/text-embedding-nomic-embed-text-v1.5": false, "ollama/gemma3:4b": true, "ollama/qwen3:8b": false}
	for id, images := range want {
		m := cat.Find(id)
		if m == nil || m.Images != images {
			t.Errorf("%s: %+v, want images %v", id, m, images)
		}
	}
	// in the catalogue's JSON (GET /api/models)
	raw, _ := json.Marshal(cat.Find("lmstudio/qwen/qwen3.6-27b"))
	if !strings.Contains(string(raw), `"images":true`) {
		t.Errorf("json %s", raw)
	}
}

// A message's images go to the runtime as image_url parts (data URLs) where their tags are; a model that does not
// see refuses them.
func TestOpenAIBackendImages(t *testing.T) {
	var got map[string]any
	srv := fakeLMStudio(t, &got)
	cat := &Catalog{}
	b := &OpenAIBackend{Catalog: cat, Runtimes: []Runtime{{Name: "lmstudio", URL: srv.URL + "/v1"}}}
	b.Discover(context.Background())
	msgs := []llm.Message{{Role: "user", Content: "what colour is [image-1]?", Images: []llm.Image{{ID: "image-1", MIME: "image/png", Data: redPNG}}}}
	var text strings.Builder
	emit := Emit(func(e Event) {
		if e.Type == "text" {
			text.WriteString(e.Text)
		}
	})
	if err := b.Run(context.Background(), cat.Find("lmstudio/qwen/qwen3.6-27b"), Request{Messages: msgs}, "", emit); err != nil {
		t.Fatal(err)
	}
	sent := got["messages"].([]any)
	user := sent[len(sent)-1].(map[string]any)
	parts, _ := json.Marshal(user["content"])
	want := `[{"text":"what colour is [image-1]","type":"text"},{"image_url":{"url":"` + (llm.Image{MIME: "image/png", Data: redPNG}).DataURL() +
		`"},"type":"image_url"},{"text":"?","type":"text"}]`
	if string(parts) != want || text.String() != "Red." {
		t.Fatalf("parts %s\nwant  %s (answer %q)", parts, want, text.String())
	}
	// a system message stays a text
	if sys := sent[0].(map[string]any); sys["role"] != "system" {
		t.Fatalf("first %v", sys)
	} else if _, ok := sys["content"].(string); !ok {
		t.Fatalf("system content %v", sys["content"])
	}
	if err := b.Run(context.Background(), cat.Find("lmstudio/plain-llm"), Request{Messages: msgs}, "", emit); err == nil ||
		!strings.Contains(err.Error(), "does not take images") {
		t.Fatalf("a text model: %v", err)
	}
}

// Claude Code's turn: the last user message's images go with the prompt; the opening (a session new on the worker)
// carries every message's, an earlier message's image renamed where its id repeats.
func TestClaudePromptImages(t *testing.T) {
	im := func(id string) llm.Image { return llm.Image{ID: id, MIME: "image/png", Data: redPNG} }
	r := Request{Session: "s", Messages: []llm.Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "look at [image-1]", Images: []llm.Image{im("image-1")}},
		{Role: "assistant", Content: "a red square"},
		{Role: "user", Content: "and [image-1] and [image-2]?", Images: []llm.Image{im("image-1"), im("image-2")}},
	}}
	prompt, opening, system, images, openingImages := claudePrompt(r)
	if system != "be brief" || prompt != "and [image-1] and [image-2]?" || len(images) != 2 || images[0].ID != "image-1" {
		t.Fatalf("prompt %q images %+v", prompt, images)
	}
	if !strings.Contains(opening, "User: look at [m1-image-1]") || !strings.HasSuffix(opening, "and [image-1] and [image-2]?") {
		t.Fatalf("opening %q", opening)
	}
	var ids []string
	for _, x := range openingImages {
		ids = append(ids, x.ID)
	}
	if strings.Join(ids, " ") != "m1-image-1 image-1 image-2" {
		t.Fatalf("opening images %v", ids)
	}
	// a one-off: the transcript is the prompt, with every image
	r.Session = ""
	prompt, opening, _, images, openingImages = claudePrompt(r)
	if opening != "" || !strings.Contains(prompt, "[m1-image-1]") || len(images) != 3 || openingImages != nil {
		t.Fatalf("one-off %q %d %v", prompt, len(images), openingImages)
	}
}
