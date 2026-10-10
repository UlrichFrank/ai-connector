package engine

import (
	"context"
	"testing"
)

// DiscoverNew adds the models a runtime has now and takes none away (a runtime that did not answer in time).
func TestDiscoverNewAdds(t *testing.T) {
	var gotChat map[string]any
	srv := fakeOllama(t, &gotChat)
	cat := &Catalog{}
	cat.SetRuntime(BackendOpenAI, []*Model{{ID: "lmstudio/kept", Kind: Chat, Backend: BackendOpenAI}})
	b := &OpenAIBackend{Catalog: cat, Runtimes: []Runtime{{Name: "ollama", URL: srv.URL + "/v1"}}}
	if n := b.DiscoverNew(context.Background()); n != 2 {
		t.Fatalf("added %d", n)
	}
	if cat.Find("lmstudio/kept") == nil || cat.Find("ollama/qwen3:8b") == nil || cat.Find("ollama/nomic-embed-text:latest") == nil {
		t.Fatalf("catalogue %v", cat.List())
	}
	if n := b.DiscoverNew(context.Background()); n != 0 || len(cat.List()) != 3 {
		t.Fatalf("again: added %d, %d models", n, len(cat.List()))
	}
}
