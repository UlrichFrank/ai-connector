package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/styles"
)

// fakeCommunity is a music backend that adds community styles (no Hugging Face: a fixed lookup).
type fakeCommunity struct {
	fakeMusic
	lib *styles.Library
}

func (f fakeCommunity) LookupCommunityStyle(_ context.Context, spec string) (engine.CommunityLookup, error) {
	return engine.CommunityLookup{Repo: spec, Rev: "abc", Trigger: "roti-d33ph0us", BaseModel: "ace-step-1.5-turbo", Name: "Deep House"}, nil
}

func (f fakeCommunity) AddCommunityStyle(_ context.Context, a engine.CommunityAdd) (styles.Style, error) {
	return f.lib.AddCommunity(styles.Style{Name: "Deep House", BaseModel: "ace-step-1.5-turbo", Trigger: "roti-d33ph0us", Weights: "/store/x",
		WeightsFile: "deep_house-v1.safetensors", Recommended: &styles.Recommended{Steps: 8, Shift: 3},
		Examples: []styles.Example{{Prompt: "deep house", BPM: 122}}, Source: &styles.Source{Repo: a.Repo, Rev: "abc"}})
}

func TestCommunityStylesAPI(t *testing.T) {
	dir := t.TempDir()
	cat := &engine.Catalog{Models: []*engine.Model{
		{ID: "ace-step-1.5-turbo", Name: "ACE-Step 1.5 turbo", Kind: engine.Music, Backend: "music", Styles: true}}}
	fc := fakeCommunity{fakeMusic{reqs: make(chan engine.Request, 4), release: make(chan struct{})}, styles.Open(filepath.Join(dir, "styles"))}
	m := jobs.New(cat, []engine.Backend{fc}, dir)
	srv := httptest.NewServer((&Server{Jobs: m}).Handler())
	defer srv.Close()
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var v map[string]any
		_ = json.NewDecoder(res.Body).Decode(&v)
		return res.StatusCode, v
	}
	code, list := call("GET", "/api/styles", "")
	if c, _ := list["community"].([]any); code != 200 || len(c) == 0 {
		t.Fatalf("list %d %v", code, list)
	}
	code, l := call("GET", "/api/community-styles?repo=someone/x", "")
	if code != 200 || l["trigger"] != "roti-d33ph0us" || l["baseModel"] != "ace-step-1.5-turbo" {
		t.Fatalf("lookup %d %v", code, l)
	}
	code, st := call("POST", "/api/styles", `{"repo": "someone/x"}`)
	src, _ := st["source"].(map[string]any)
	if code != 200 || src["repo"] != "someone/x" || st["weights"] != "lora" || st["status"] != "trained" || st["weightsFile"] != "deep_house-v1.safetensors" {
		t.Fatalf("add %d %v", code, st)
	}
	id := st["id"].(string)
	if code, _ := call("POST", "/api/styles/"+id+"/train", ""); code != http.StatusBadRequest {
		t.Fatalf("train a community style: %d", code)
	}
	if code, _ := call("PATCH", "/api/styles/"+id, `{"strength": 0.6}`); code != 200 {
		t.Fatalf("strength: %d", code)
	}
	if code, _ := call("PATCH", "/api/styles/"+id, `{"settings": {"steps": 10}}`); code != http.StatusBadRequest {
		t.Fatalf("settings of a community style: %d", code)
	}
	if code, _ := call("DELETE", "/api/styles/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
}
