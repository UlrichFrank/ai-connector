package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/llm"
	"github.com/philipparndt/ai-connector/internal/models"
)

// hfToken points the Hugging Face token at a file of the test ("" none) and HFEndpoint at answer.
func hfToken(t *testing.T, token string, answer func(repo string) int) {
	t.Helper()
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HUGGING_FACE_HUB_TOKEN", "")
	p := filepath.Join(t.TempDir(), "token")
	t.Setenv("HF_TOKEN_PATH", p)
	if token != "" {
		os.WriteFile(p, []byte(token), 0o600)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo := r.URL.Path[len("/api/models/") : len(r.URL.Path)-len("/auth-check")]
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(answer(repo))
	}))
	t.Cleanup(srv.Close)
	old := HFEndpoint
	HFEndpoint = srv.URL
	t.Cleanup(func() { HFEndpoint = old })
}

func waitState(t *testing.T, d *Downloads, id string, ok func(DownloadState) bool) DownloadState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _ := d.Status(id)
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %+v", id, st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDownloadsOneAtATime(t *testing.T) {
	hfToken(t, "", nil)
	chat := llm.Qwen35_4B
	cat := &Catalog{Models: []*Model{
		{ID: "qwen3.5-4b", Name: "Qwen3.5 4B", Kind: Chat, Backend: "mlx-go", Size: 3 * gb, chat: &chat},
		{ID: "qwen3-embedding-0.6b", Name: "Embedder", Kind: Embed, Backend: "go", Size: gb, pinned: &models.Qwen3Embedding},
		{ID: "big", Name: "Big", Kind: Image, Backend: "mflux", Size: 60 * gb, repo: "x/big"},
		{ID: "gated", Name: "Gated", Kind: Image, Backend: "mflux", Size: gb, repo: "x/gated", Gated: true},
		{ID: "avatar", Name: "Avatar", Kind: Avatar, Backend: "avatar", Size: gb, repo: "x/avatar"},
	}}
	release := make(chan struct{})
	var order []string
	d := &Downloads{Catalog: cat, Store: &models.Store{Dir: t.TempDir()}, Free: func() int64 { return 50 * gb },
		Fetch: func(ctx context.Context, m *Model, report func(done, total int64)) error {
			order = append(order, m.ID)
			report(m.Size/2, 0)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			report(m.Size, 0)
			return nil
		}}
	st, err := d.Start("qwen3.5-4b", 64*gb)
	if err != nil || st.State != DownloadQueued {
		t.Fatalf("start %+v %v", st, err)
	}
	waitState(t, d, "qwen3.5-4b", func(s DownloadState) bool { return s.State == DownloadDownloading && s.Bytes == 3*gb/2 })
	// a second one waits for the first; asking again joins it
	if st, _ := d.Start("qwen3-embedding-0.6b", 64*gb); st.State != DownloadQueued {
		t.Fatalf("second %+v", st)
	}
	if st, _ := d.Start("qwen3.5-4b", 64*gb); st.State != DownloadDownloading {
		t.Fatalf("joined %+v", st)
	}
	close(release)
	waitState(t, d, "qwen3.5-4b", func(s DownloadState) bool { return s.State == DownloadDone })
	waitState(t, d, "qwen3-embedding-0.6b", func(s DownloadState) bool { return s.State == DownloadDone })
	if len(order) != 2 || order[0] != "qwen3.5-4b" || !cat.Models[0].Here || !cat.Models[1].Here {
		t.Fatalf("order %v, here %v %v", order, cat.Models[0].Here, cat.Models[1].Here)
	}
	if st, _ := d.Start("qwen3.5-4b", 64*gb); st.State != DownloadDone {
		t.Fatalf("here already %+v", st)
	}

	// what only the person can do
	if st, _ := d.Start("big", 64*gb); st.State != DownloadQueued {
		t.Fatalf("big %+v", st)
	}
	if st := waitState(t, d, "big", func(s DownloadState) bool { return !s.Active() }); st.Need != NeedSpace || st.Error == "" {
		t.Fatalf("space %+v", st)
	}
	if st, _ := d.Start("big", 16*gb); st.Need != NeedMemory {
		t.Fatalf("memory %+v", st)
	}
	d.Start("gated", 64*gb)
	if st := waitState(t, d, "gated", func(s DownloadState) bool { return !s.Active() }); st.Need != NeedToken {
		t.Fatalf("token %+v", st)
	}
	if _, err := d.Start("avatar", 64*gb); err == nil {
		t.Fatal("an avatar model through the setup")
	}
	if _, err := d.Start("nope", 64*gb); err == nil {
		t.Fatal("an unknown model")
	}
}

func TestDownloadsCancelAndAccess(t *testing.T) {
	access := map[string]int{"x/gated": 403}
	hfToken(t, "hf_abc", func(repo string) int { return access[repo] })
	cat := &Catalog{Models: []*Model{
		{ID: "gated", Name: "Gated", Kind: Image, Backend: "mflux", Size: gb, repo: "x/gated", Gated: true},
	}}
	fetched := make(chan struct{}, 4)
	d := &Downloads{Catalog: cat, Store: &models.Store{Dir: t.TempDir()}, Free: func() int64 { return 50 * gb },
		Fetch: func(ctx context.Context, m *Model, report func(done, total int64)) error {
			fetched <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}}
	d.Start("gated", 64*gb)
	st := waitState(t, d, "gated", func(s DownloadState) bool { return !s.Active() })
	if st.Need != NeedLicence || st.URL != "https://huggingface.co/x/gated" {
		t.Fatalf("licence %+v", st)
	}
	if ms := d.Models(context.Background(), 64*gb, true); len(ms) != 1 || ms[0].Access != "needed" || !ms[0].Gated {
		t.Fatalf("models %+v", ms)
	}
	access["x/gated"] = 200 // accepted: the next download asks again
	d.Start("gated", 64*gb)
	<-fetched
	if !d.Cancel("gated") {
		t.Fatal("not canceled")
	}
	if st := waitState(t, d, "gated", func(s DownloadState) bool { return !s.Active() }); st.State != DownloadCanceled {
		t.Fatalf("canceled %+v", st)
	}
}

func TestSetupModelsRecommended(t *testing.T) {
	hfToken(t, "", nil)
	var cs []*Model
	for i := range llm.Catalogue {
		c := llm.Catalogue[i]
		cs = append(cs, &Model{ID: c.Short, Name: c.Name, Kind: Chat, Backend: "mlx-go", Size: c.Size(), chat: &c})
	}
	cs = append(cs, &Model{ID: "claude-code/sonnet", Kind: Chat, Backend: BackendClaudeCode, Here: true},
		&Model{ID: "flux2-klein-4b", Kind: Image, Backend: "mflux", Size: 16 * gb, repo: "b/f"})
	d := &Downloads{Catalog: &Catalog{Models: cs}, Store: &models.Store{Dir: t.TempDir()}, Free: func() int64 { return 500 * gb }}
	for _, x := range []struct {
		mem        int64
		chat, flux bool
		want       string
	}{{64 * gb, true, true, "qwen3.6-35b-a3b"}, {96 * gb, true, true, "qwen3.6-35b-a3b"}, {32 * gb, true, true, "qwen3.5-9b"},
		{48 * gb, true, true, "qwen3.5-9b"}, {16 * gb, true, false, "qwen3.5-4b"}, {8 * gb, true, false, "qwen3.5-4b"}} {
		got := ""
		for _, m := range d.Models(context.Background(), x.mem, false) {
			if m.ID == "claude-code/sonnet" {
				t.Fatal("Claude Code in the setup's list")
			}
			if m.Recommended && m.Kind == Chat {
				if got != "" {
					t.Fatalf("%d GB: two recommended", x.mem/gb)
				}
				got = m.ID
			}
			if m.ID == "flux2-klein-4b" && (m.Recommended != x.flux || m.Licence != "Apache-2.0" || m.URL != "https://huggingface.co/b/f") {
				t.Fatalf("%d GB: flux %+v", x.mem/gb, m)
			}
		}
		if got != x.want || RecommendedChat(x.mem) != x.want {
			t.Errorf("%d GB: recommended %q, want %q", x.mem/gb, got, x.want)
		}
	}
}

// audioCatalog is the catalogue's music, sound and speech models and the upscalers (nothing here), and FLUX.2 klein 4B.
func audioCatalog() *Catalog {
	ms := []*Model{{ID: "flux2-klein-4b", Name: "FLUX.2 [klein] 4b", Kind: Image, Backend: "mflux", Size: 16 * gb, repo: "b/f"}}
	ms = append(ms, seedvr2Catalog(&models.Store{Dir: "/nonexistent"}, "")...)
	ms = append(ms, musicModels()...)
	ms = append(ms, soundModels()...)
	ms = append(ms, speechModels()...)
	return &Catalog{Models: ms}
}

// Music, sound and speech models and the upscalers are offered and downloaded like the others: the files, then the
// runtime of their backend (preparing) — the music runtime's own step too (LeVo2.cpp, Stable Audio 3's code).
func TestDownloadsAudioPrepare(t *testing.T) {
	hfToken(t, "", nil)
	cat := audioCatalog()
	var mu sync.Mutex
	prepared := map[string]string{}
	var states []string
	d := &Downloads{Catalog: cat, Store: &models.Store{Dir: t.TempDir()}, Free: func() int64 { return 500 * gb },
		Fetch: func(ctx context.Context, m *Model, report func(done, total int64)) error {
			report(m.Size, 0)
			return nil
		},
		Prepare: func(ctx context.Context, m *Model, env string, build bool) error {
			mu.Lock()
			defer mu.Unlock()
			prepared[m.ID] = fmt.Sprintf("%s %v", env, build)
			if m.ID == "levo2-medium" {
				return errors.New("cmake is not installed")
			}
			return nil
		},
		Changed: func(st DownloadState) {
			mu.Lock()
			defer mu.Unlock()
			if st.Model == "kokoro-82m" && (len(states) == 0 || states[len(states)-1] != st.State) {
				states = append(states, st.State)
			}
		}}
	want := map[string]string{
		"ace-step-1.5-turbo":   "acestep false",
		"stable-audio-3-small": "stableaudio3 true",
		"levo2-large":          "levo2 true",
		"minimax-music3-4bit":  "minimax3 false",
		"heartmula-3b":         "heartmula false",
		"yue2-3b":              "yue2 false",
		"moss-soundeffect":     "sound false",
		"kokoro-82m":           "speech false",
		"qwen3-tts-1.7b":       "speech false",
		"qwen3-asr":            "speech false",
		"seedvr2-3b":           "mflux false",
	}
	for id := range want {
		if _, err := d.Start(id, 64*gb); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	for id, w := range want {
		st := waitState(t, d, id, func(s DownloadState) bool { return !s.Active() })
		mu.Lock()
		got := prepared[id]
		mu.Unlock()
		if st.State != DownloadDone || got != w || !cat.Find(id).Here {
			t.Errorf("%s: %+v, prepared %q, want %q", id, st, got, w)
		}
	}
	mu.Lock()
	if !slices.Equal(states, []string{DownloadQueued, DownloadDownloading, DownloadVerifying, DownloadPreparing, DownloadDone}) {
		t.Errorf("kokoro's states %v", states)
	}
	mu.Unlock()
	// a runtime that cannot be made: failed, with why
	d.Start("levo2-medium", 64*gb)
	if st := waitState(t, d, "levo2-medium", func(s DownloadState) bool { return !s.Active() }); st.State != DownloadFailed || !strings.Contains(st.Error, "cmake") {
		t.Fatalf("levo2 %+v", st)
	}

	// every model here has its repositories and its runtime
	for _, m := range cat.Models {
		if env, _ := runtimeOf(m); !Downloadable(m) || env == nil || RepoURL(m) == "" {
			t.Errorf("%s: downloadable %v, runtime %v, url %q", m.ID, Downloadable(m), env, RepoURL(m))
		}
	}
}

// The recommendation is per kind, and per kind/task for an upscaler: FLUX.2 klein 4B and SeedVR2 3B both; songs by
// the memory (ACE-Step 1.5 turbo from 32 GB, else Stable Audio 3 Small); MOSS for sounds; Kokoro for speech (not the
// transcriber).
func TestSetupModelsRecommendedAudio(t *testing.T) {
	hfToken(t, "", nil)
	d := &Downloads{Catalog: audioCatalog(), Store: &models.Store{Dir: t.TempDir()}, Free: func() int64 { return 500 * gb }}
	for _, x := range []struct {
		mem  int64
		want []string
	}{
		{64 * gb, []string{"ace-step-1.5-turbo", "flux2-klein-4b", "kokoro-82m", "moss-soundeffect", "seedvr2-3b"}},
		{32 * gb, []string{"ace-step-1.5-turbo", "flux2-klein-4b", "kokoro-82m", "moss-soundeffect", "seedvr2-3b"}},
		{24 * gb, []string{"flux2-klein-4b", "kokoro-82m", "moss-soundeffect", "seedvr2-3b", "stable-audio-3-small"}},
		{16 * gb, []string{"kokoro-82m", "moss-soundeffect", "seedvr2-3b", "stable-audio-3-small"}},
	} {
		var got []string
		roles := map[string]bool{}
		for _, m := range d.Models(context.Background(), x.mem, false) {
			if !m.Recommended {
				continue
			}
			role := SetupRole(m.Kind, m.Tasks)
			if roles[role] {
				t.Errorf("%d GB: two recommended for %s", x.mem/gb, role)
			}
			roles[role] = true
			got = append(got, m.ID)
		}
		slices.Sort(got)
		if !slices.Equal(got, x.want) {
			t.Errorf("%d GB: recommended %v, want %v", x.mem/gb, got, x.want)
		}
	}
	if RecommendedMusic(32*gb) != "ace-step-1.5-turbo" || RecommendedMusic(24*gb) != "stable-audio-3-small" {
		t.Error("RecommendedMusic")
	}
	byID := map[string]SetupModel{}
	for _, m := range d.Models(context.Background(), 64*gb, false) {
		byID[m.ID] = m
	}
	if m := byID["ace-step-1.5-turbo"]; m.Kind != Music || m.Licence != "MIT" || m.URL != "https://huggingface.co/ACE-Step/Ace-Step1.5" || !m.Fits {
		t.Errorf("ace-step %+v", m)
	}
	if m := byID["yue2-3b"]; !m.NonCommercial || m.Licence == "" {
		t.Errorf("yue2 %+v", m)
	}
	if m := byID["kokoro-82m"]; m.Kind != Speech || m.Licence != "Apache-2.0" || m.Here || !slices.Contains(m.Tasks, "presets") {
		t.Errorf("kokoro %+v", m)
	}
	// the languages a speech model speaks (vboard proposes Kokoro's download only for them, not for German)
	if l := byID["kokoro-82m"].Languages; !slices.Contains(l, "en") || slices.Contains(l, "de") {
		t.Errorf("kokoro languages %v", l)
	}
	if l := byID["qwen3-tts-1.7b"].Languages; !slices.Contains(l, "de") {
		t.Errorf("qwen3-tts languages %v", l)
	}
	if byID["ace-step-1.5-turbo"].Languages != nil {
		t.Error("languages on a song model")
	}
	if m := byID["seedvr2-3b"]; m.Kind != Image || !slices.Equal(m.Tasks, []string{TaskUpscale}) || SetupRole(m.Kind, m.Tasks) != "image/upscale" {
		t.Errorf("seedvr2 %+v", m)
	}
	if SetupRole(Speech, byID["qwen3-asr"].Tasks) != "speech/transcribe" {
		t.Errorf("asr %+v", byID["qwen3-asr"])
	}
	// too large for the memory: does not fit (HeartMuLa, 21 GB, wants 42)
	for _, m := range d.Models(context.Background(), 32*gb, false) {
		if m.ID == "heartmula-3b" && m.Fits {
			t.Errorf("heartmula fits 32 GB")
		}
	}
}
