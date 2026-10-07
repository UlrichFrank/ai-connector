package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/styles"
)

// fakeHF is a Hugging Face stand-in: repositories by name (files by path) at one commit each; the store's client
// sends every huggingface.co request to it.
type fakeHF struct {
	sha   string
	repos map[string]map[string]string
	hits  map[string]int
}

func (h *fakeHF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.hits[r.URL.Path]++
	p := r.URL.Path
	if rest, ok := strings.CutPrefix(p, "/api/models/"); ok {
		for name, files := range h.repos {
			switch {
			case rest == name || rest == name+"/revision/main" || rest == name+"/revision/"+h.sha:
				_ = json.NewEncoder(w).Encode(map[string]any{"id": name, "sha": h.sha, "gated": false, "author": "someone", "cardData": map[string]any{}})
				return
			case rest == name+"/tree/"+h.sha:
				var out []map[string]any
				for path, body := range files {
					sum := sha256.Sum256([]byte(body))
					out = append(out, map[string]any{"type": "file", "path": path, "size": len(body),
						"lfs": map[string]any{"oid": hex.EncodeToString(sum[:]), "size": len(body)}})
				}
				_ = json.NewEncoder(w).Encode(out)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	for name, files := range h.repos {
		if path, ok := strings.CutPrefix(p, "/"+name+"/resolve/"+h.sha+"/"); ok {
			if body, ok := files[path]; ok {
				_, _ = w.Write([]byte(body))
				return
			}
		}
	}
	http.NotFound(w, r)
}

type toServer struct{ base *url.URL }

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = t.base.Scheme, t.base.Host
	return http.DefaultTransport.RoundTrip(r)
}

const deepHouseMeta = `{"schema_version": 1, "name": "Deep House", "description": "Deep House-style generation.", "author": "RyanOnTheInside",
 "model": {"type": "lora", "base_model": "AceStep v1.5 Turbo", "base_model_scale": "2B"},
 "inference": {"primary_trigger_word": "roti-d33ph0us", "trigger_words": [], "recommended_strength": 1.0,
   "tested_strength_range": [0.3, 1.0], "recommended_steps": 8, "recommended_shift": 3.0, "recommended_guidance": 7.0,
   "example_prompts": [{"caption": "deep house, four on the floor kick, warm sub bass, jazzy keys, soulful vocals", "bpm": 122.0, "key": "A minor"}]},
 "compatibility": {"base_model_required": "AceStep v1.5 Turbo (2B)"},
 "files": {"weights": "deep_house-v1.safetensors"}, "distribution": {"license": null}}`

const peftLoRA = `{"peft_type": "LORA", "r": 64, "lora_alpha": 128, "target_modules": ["q_proj", "k_proj", "v_proj", "o_proj"],
 "use_dora": false, "base_model_name_or_path": "/root/checkpoints/acestep-v15-turbo"}`

func withFakeHF(t *testing.T, b *MusicBackend) *fakeHF {
	h := &fakeHF{sha: strings.Repeat("ab", 20), hits: map[string]int{}, repos: map[string]map[string]string{
		"someone/deep-house": {"adapter_config.json": peftLoRA, "deep_house-v1.safetensors": "WEIGHTS", "deep_house-v1.metadata.json": deepHouseMeta, "README.md": "# hi"},
		"someone/bare":       {"adapter_config.json": strings.Replace(peftLoRA, "acestep-v15-turbo", "my-model", 1), "adapter_model.safetensors": "W2"},
		"someone/dora":       {"adapter_config.json": strings.Replace(peftLoRA, `"use_dora": false`, `"use_dora": true`, 1), "adapter_model.safetensors": "W3"},
		"someone/notlora":    {"config.json": "{}", "model.safetensors": "x"},
	}}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	t.Setenv("HF_HUB_CACHE", t.TempDir()) // nothing to link from the user's cache
	b.Store.Client = &http.Client{Transport: toServer{u}}
	b.Store.Sources = []models.Source{models.HuggingFace()}
	return h
}

func TestCommunityStyle(t *testing.T) {
	b, m := fakeMusicBackend(t)
	defer b.Unload()
	h := withFakeHF(t, b)
	ctx := context.Background()

	l, err := b.LookupCommunityStyle(ctx, "https://huggingface.co/someone/deep-house")
	must(t, err)
	if l.Rev != h.sha || l.Trigger != "roti-d33ph0us" || l.BaseModel != "ace-step-1.5-turbo" || l.BaseText != "AceStep v1.5 Turbo (2B)" ||
		l.WeightsFile != "deep_house-v1.safetensors" || l.Recommended != (styles.Recommended{Steps: 8, Guidance: 7, Shift: 3}) ||
		len(l.Examples) != 1 || l.Examples[0].BPM != 122 || l.Rank != 64 || l.Alpha != 128 || l.Licence != "" || l.Name != "Deep House" {
		t.Fatalf("lookup %+v", l)
	}
	// only the small files came down
	if h.hits["/someone/deep-house/resolve/"+h.sha+"/deep_house-v1.safetensors"] != 0 {
		t.Fatal("the weights were fetched by a lookup")
	}

	st, err := b.AddCommunityStyle(ctx, CommunityAdd{Repo: "someone/deep-house"})
	must(t, err)
	if !st.Community() || st.Source.Spec() != "someone/deep-house@"+h.sha || st.Status != styles.Trained || st.Trigger != "roti-d33ph0us" ||
		st.BaseModel != m.ID || st.Recommended == nil || st.Recommended.Steps != 8 {
		t.Fatalf("style %+v", st)
	}
	again, err := b.AddCommunityStyle(ctx, CommunityAdd{Repo: "someone/deep-house@" + h.sha})
	must(t, err)
	if again.ID != st.ID {
		t.Fatal("added twice")
	}
	// not trainable, no tracks
	if _, _, err := b.StyleLibrary().AddTrack(ctx, st.ID, "a.wav", strings.NewReader("aaaaaaaaaa"), false); err == nil {
		t.Fatal("a track on a community style")
	}
	if err := b.Run(ctx, m, Request{Task: TrainStyle, Style: st.ID}, t.TempDir(), (&recorder{}).emit); err == nil ||
		!strings.Contains(err.Error(), "cannot be trained") {
		t.Fatalf("train: %v", err)
	}

	// a song with it: the weights fetched (pinned), the LoRA, trigger and recommended settings reach the worker
	song := func(r Request) map[string]any {
		t.Helper()
		rec := &recorder{}
		must(t, b.Run(ctx, m, r, t.TempDir(), rec.emit))
		for _, e := range rec.evs {
			if e.Type == "file" {
				raw, err := os.ReadFile(e.File)
				must(t, err)
				var v map[string]any
				must(t, json.Unmarshal(raw, &v))
				return v
			}
		}
		t.Fatal("no file")
		return nil
	}
	got := song(Request{Prompt: "deep house, warm sub bass", Style: st.ID, Seed: 1})
	dir := filepath.Join(b.Store.Dir, "someone", "deep-house", h.sha)
	lora, _ := got["lora"].(map[string]any)
	if dirs, _ := got["dirs"].(map[string]any); dirs["lora"] != dir || lora["file"] != "deep_house-v1.safetensors" || lora["scale"] != 1.0 ||
		got["prompt"] != "roti-d33ph0us, deep house, warm sub bass" || got["steps"] != 8.0 || got["shift"] != 3.0 || got["guidance"] != 7.0 {
		t.Fatalf("song request %v", got)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "deep_house-v1.safetensors")); err != nil || string(b) != "WEIGHTS" {
		t.Fatalf("weights %q %v", b, err)
	}
	// what the request sets wins over the recommendation; no style: none of it
	got = song(Request{Prompt: "x", Style: st.ID, Steps: 4, Shift: 2, StyleStrength: 0.5, Seed: 1})
	if got["steps"] != 4.0 || got["shift"] != 2.0 || got["lora"].(map[string]any)["scale"] != 0.5 {
		t.Fatalf("overrides %v", got)
	}
	got = song(Request{Prompt: "x", Seed: 1})
	if _, ok := got["shift"]; ok || got["lora"] != nil {
		t.Fatalf("no style %v", got)
	}
	// another model: refused
	other := *m
	other.ID = "ace-step-1.5-xl-turbo"
	if err := b.Run(ctx, &other, Request{Prompt: "x", Style: st.ID}, t.TempDir(), (&recorder{}).emit); err == nil ||
		!strings.Contains(err.Error(), "made for ace-step-1.5-turbo") {
		t.Fatalf("other model: %v", err)
	}
	if _, err := b.AddCommunityStyle(ctx, CommunityAdd{Repo: "someone/deep-house", BaseModel: "ace-step-1.5-xl-turbo"}); err == nil {
		t.Fatal("a 2B LoRA for the XL")
	}

	// no metadata: the user names the trigger and the model
	if _, err := b.AddCommunityStyle(ctx, CommunityAdd{Repo: "someone/bare"}); err == nil || !strings.Contains(err.Error(), "which model") {
		t.Fatalf("bare without a model: %v", err)
	}
	if _, err := b.AddCommunityStyle(ctx, CommunityAdd{Repo: "someone/bare", BaseModel: m.ID}); err == nil || !strings.Contains(err.Error(), "trigger") {
		t.Fatalf("bare without a trigger: %v", err)
	}
	if _, err := b.AddCommunityStyle(ctx, CommunityAdd{Repo: "someone/bare", BaseModel: "stable-audio-3-medium", Trigger: "x"}); err == nil {
		t.Fatal("a PEFT LoRA for Stable Audio 3")
	}
	bare, err := b.AddCommunityStyle(ctx, CommunityAdd{Repo: "someone/bare", BaseModel: m.ID, Trigger: "bare-1", Name: "Bare"})
	must(t, err)
	if bare.WeightsFile != "adapter_model.safetensors" || bare.Recommended != nil || bare.Name != "Bare" {
		t.Fatalf("bare %+v", bare)
	}
	for _, repo := range []string{"someone/dora", "someone/notlora", "someone/missing", "not a repo"} {
		if _, err := b.LookupCommunityStyle(ctx, repo); err == nil {
			t.Errorf("%s: no error", repo)
		}
	}
}

func TestCommunityBaseAndRepo(t *testing.T) {
	for in, want := range map[string]string{
		"AceStep v1.5 Turbo (2B)":              "ace-step-1.5-turbo",
		"AceStep v1.5 Turbo 4B":                "ace-step-1.5-xl-turbo",
		"ACE-Step 1.5 XL base":                 "ace-step-1.5-xl-base",
		"/root/checkpoints/acestep-v15-turbo":  "ace-step-1.5-turbo",
		"acestep-v15-xl-turbo":                 "ace-step-1.5-xl-turbo",
		"ACE-Step v1 3.5B":                     "",
		"stabilityai/stable-audio-3-medium":    "",
		"AceStep v1.5 (no variant named here)": "",
	} {
		if got := CommunityBase(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string][2]string{
		"ryanontheinside/deep_house-acestep1.5-v1": {"ryanontheinside/deep_house-acestep1.5-v1", ""},
		"a/b@abc123":                                                        {"a/b", "abc123"},
		"https://huggingface.co/a/b":                                        {"a/b", ""},
		"https://huggingface.co/a/b/tree/main":                              {"a/b", "main"},
		"hf.co/a/b/blob/0123/adapter_config.json":                           {"a/b", "0123"},
		" https://huggingface.co/ryanontheinside/deep_house-acestep1.5-v1 ": {"ryanontheinside/deep_house-acestep1.5-v1", ""},
	} {
		repo, rev, err := ParseRepo(in)
		if err != nil || repo != want[0] || rev != want[1] {
			t.Errorf("%q: %q %q %v", in, repo, rev, err)
		}
	}
	for _, bad := range []string{"", "x", "../a/b", "a/b c", "a/b@x/y"} {
		if _, _, err := ParseRepo(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	if c := curated("RyanOnTheInside/deep_house-acestep1.5-v1"); c == nil || c.Rev != "746cf4500a242c741d28577b0d85fe579b830133" || !CommunityModel(c.BaseModel) {
		t.Fatalf("curated %+v", c)
	}
}
