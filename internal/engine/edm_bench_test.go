//go:build edmbench

package engine

// The EDM benchmark: the same electronic prompts through several song models and settings, the files kept to listen
// to and to measure (tempo, drums share, pulse clarity: scratch analysis script). Heavy (GPU, minutes per song) and
// it downloads models, so it runs only when asked for:
//
//	MLXLAB_BENCH_HOME=<a home with models/ and envs/> MLXLAB_BENCH_OUT=<dir> \
//	  go test -tags edmbench -run TestEDMBench -timeout 0 -v ./internal/engine/
//
// MLXLAB_BENCH_CONFIGS picks configurations (comma separated, default all of edmConfigs); MLXLAB_BENCH_WAIT=<history.json>
// waits while that job history has a job running or queued (somebody else's worker on the same GPU).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
)

var edmPrompts = []struct{ id, style string }{
	{"trance-138", "uplifting trance, 138 bpm, four-on-the-floor kick drum, punchy kick, rolling offbeat bass, supersaw lead, driving hi-hats, energetic, electronic, instrumental"},
	{"techno-130", "techno, 130 bpm, pounding four-on-the-floor kick drum, driving hi-hats, rumbling bass, hypnotic synth stabs, dark warehouse, electronic, instrumental"},
	{"proghouse-124", "progressive house, 124 bpm, four-on-the-floor kick drum, punchy kick, rolling bassline, plucked synth arpeggios, wide pads, electronic, instrumental"},
	{"dnb-174", "drum and bass, 174 bpm, fast breakbeat drums, punchy kick and snare, deep reese bass, energetic, electronic, instrumental"},
}

var edmSeeds = []int64{11, 22}

type edmConfig struct {
	id, model string
	planner   string
	lock      bool // tempo lock (off: the song as the model made it)
	takes     int
	only      []string // prompt ids (none: all)
	tweak     func(req map[string]any)
}

// oldACE is what mlxlab sent ACE-Step before the EDM tuning: no tempo, the planner rewriting the caption at its
// default temperature, the base model at 32 steps and the library's CFG 7.
func oldACE(req map[string]any) {
	for _, k := range []string{"bpm", "cot_caption", "lm_temperature", "guidance"} {
		delete(req, k)
	}
	if req["model"] == "ace-step-1.5-base" {
		req["steps"] = 32
	}
}

var edmConfigs = []edmConfig{
	{id: "sa3-small", model: "stable-audio-3-small"},
	{id: "sa3-medium", model: "stable-audio-3-medium"},
	{id: "ace-base-old", model: "ace-step-1.5-base", tweak: oldACE},
	{id: "ace-base-new", model: "ace-step-1.5-base"},
	{id: "ace-xlturbo-new", model: "ace-step-1.5-xl-turbo"},
	// extras (by name in MLXLAB_BENCH_CONFIGS)
	{id: "ace-xlturbo-old", model: "ace-step-1.5-xl-turbo", tweak: oldACE},
	{id: "ace-base-new-4b", model: "ace-step-1.5-base", planner: "4b"},
	{id: "ace-xlturbo-new-4b", model: "ace-step-1.5-xl-turbo", planner: "4b"},
	{id: "ace-xlturbo-techno-lora", model: "ace-step-1.5-xl-turbo-techno", only: []string{"techno-130"}},
	{id: "ace-xlbase-new", model: "ace-step-1.5-xl-base"},
	// tried for EDM, measured no better than the planner's defaults (kept to compare): the caption as written
	// (cot_caption false) and planner temperature 0.65 on top of the shipped settings
	// tempo lock and best of N (the shipped defaults: lock on when there is a tempo)
	{id: "ace-base-old-lock", model: "ace-step-1.5-base", tweak: oldACE, lock: true},
	{id: "ace-xlturbo-new-lock", model: "ace-step-1.5-xl-turbo", lock: true},
	{id: "ace-xlturbo-new-best3", model: "ace-step-1.5-xl-turbo", lock: true, takes: 3},
	{id: "sa3-medium-lock", model: "stable-audio-3-medium", lock: true},
	{id: "ace-xlturbo-nocot", model: "ace-step-1.5-xl-turbo", tweak: noCoT},
	{id: "ace-base-nocot", model: "ace-step-1.5-base", tweak: noCoT},
	{id: "sa3-small-tags", model: "stable-audio-3-small", tweak: sa3Tags},
	{id: "sa3-medium-tags", model: "stable-audio-3-medium", tweak: sa3Tags},
	{id: "sa3-small-cfg", model: "stable-audio-3-small", tweak: sa3CFG},
	{id: "sa3-medium-cfg", model: "stable-audio-3-medium", tweak: sa3CFG},
}

func noCoT(req map[string]any) { req["cot_caption"], req["lm_temperature"] = false, 0.65 }

// sa3Tags starts the prompt with the AudioSparx tags upstream's prompting guide recommends for music.
func sa3Tags(req map[string]any) {
	req["prompt"] = "TrackType: Music, VocalType: Instrumental, " + req["prompt"].(string)
}

// sa3CFG turns classifier-free guidance on (scale 3, upstream's adaptive projected guidance) with a negative prompt
// against beatless, acoustic results.
func sa3CFG(req map[string]any) {
	req["cfg"], req["negative"] = 3.0, "ambient, beatless, no drums, acoustic, vocals"
}

func TestEDMBench(t *testing.T) {
	home, out := os.Getenv("MLXLAB_BENCH_HOME"), os.Getenv("MLXLAB_BENCH_OUT")
	if home == "" || out == "" {
		t.Skip("MLXLAB_BENCH_HOME and MLXLAB_BENCH_OUT")
	}
	want := map[string]bool{}
	for _, c := range strings.Split(os.Getenv("MLXLAB_BENCH_CONFIGS"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			want[c] = true
		}
	}
	srcs, err := models.Sources(os.Getenv("MLXLAB_MODELS_FROM"), nil)
	if err != nil {
		t.Fatal(err)
	}
	store := &models.Store{Dir: filepath.Join(home, "models"), Sources: srcs}
	b := &MusicBackend{Root: home, Store: store, Runtimes: MusicRuntimes()}
	defer b.Unload()
	byID := map[string]*Model{}
	for _, m := range musicModels() {
		byID[m.ID] = m
	}
	results, err := os.OpenFile(filepath.Join(out, "results.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer results.Close()
	for _, c := range edmConfigs {
		if len(want) > 0 && !want[c.id] || len(want) == 0 && !slices.Contains([]string{"sa3-small", "sa3-medium", "ace-base-old", "ace-base-new", "ace-xlturbo-new"}, c.id) {
			continue
		}
		m := byID[c.model]
		b.tweak = c.tweak
		for _, p := range edmPrompts {
			if len(c.only) > 0 && !slices.Contains(c.only, p.id) {
				continue
			}
			for _, seed := range edmSeeds {
				dst := filepath.Join(out, c.id, fmt.Sprintf("%s-s%d.wav", p.id, seed))
				if _, err := os.Stat(dst); err == nil {
					continue // done in an earlier run
				}
				waitIdle(t, os.Getenv("MLXLAB_BENCH_WAIT"))
				tmp := filepath.Join(out, c.id, "tmp")
				if err := os.MkdirAll(tmp, 0o755); err != nil {
					t.Fatal(err)
				}
				var file string
				stats := map[string]any{}
				var logs []string
				emit := func(e Event) {
					switch e.Type {
					case "file":
						if file == "" { // the first file is the song (a second one: the original before the tempo lock)
							file = e.File
						}
					case "stats":
						for k, v := range e.Data {
							stats[k] = v
						}
					case "log":
						if strings.HasPrefix(e.Text, "LM:") || strings.HasPrefix(e.Text, "LoRA") || strings.Contains(e.Text, "loaded") {
							logs = append(logs, e.Text)
						}
					case "status":
						t.Logf("%s %s s%d: %s", c.id, p.id, seed, e.Text)
					}
				}
				r := Request{Model: m.ID, Prompt: p.style, Instrumental: true, Duration: 45, Seed: seed, Planner: c.planner, LockTempo: &c.lock, Takes: c.takes}
				t0 := time.Now()
				if err := b.Run(context.Background(), m, r, tmp, emit); err != nil {
					t.Errorf("%s %s s%d: %v", c.id, p.id, seed, err)
					continue
				}
				if err := os.Rename(file, dst); err != nil {
					t.Fatal(err)
				}
				line, _ := json.Marshal(map[string]any{"config": c.id, "model": m.ID, "prompt": p.id, "seed": seed, "file": dst,
					"wall": time.Since(t0).Seconds(), "stats": stats, "logs": logs})
				_, _ = results.Write(append(line, '\n'))
				t.Logf("%s %s s%d: %.1f s %v %v", c.id, p.id, seed, time.Since(t0).Seconds(), stats, logs)
			}
		}
	}
}

// TestEDMLockOffline applies the tempo lock (the post-step of a job: measure, stretch when within 12 %) to the songs
// of configurations already made (MLXLAB_BENCH_CONFIGS), into <config>-lock/: the same files a job with lockTempo
// would give for the same seeds, without making the songs again. No GPU.
func TestEDMLockOffline(t *testing.T) {
	home, out := os.Getenv("MLXLAB_BENCH_HOME"), os.Getenv("MLXLAB_BENCH_OUT")
	if home == "" || out == "" {
		t.Skip("MLXLAB_BENCH_HOME and MLXLAB_BENCH_OUT")
	}
	b := &MusicBackend{Root: home}
	results, err := os.OpenFile(filepath.Join(out, "results.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer results.Close()
	for _, c := range strings.Split(os.Getenv("MLXLAB_BENCH_CONFIGS"), ",") {
		for _, p := range edmPrompts {
			bpm := styleBPM(p.style)
			for _, seed := range edmSeeds {
				src := filepath.Join(out, c, fmt.Sprintf("%s-s%d.wav", p.id, seed))
				dst := filepath.Join(out, c+"-lock", fmt.Sprintf("%s-s%d.wav", p.id, seed))
				if _, err := os.Stat(src); err != nil {
					continue
				}
				if _, err := os.Stat(dst); err == nil {
					continue
				}
				_ = os.MkdirAll(filepath.Dir(dst), 0o755)
				tmp := filepath.Join(filepath.Dir(dst), "tmp.wav")
				bs, err := os.ReadFile(src)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(tmp, bs, 0o644); err != nil {
					t.Fatal(err)
				}
				ev, stats, err := b.settleTempo(context.Background(), []takeResult{{file: tmp, seed: seed, ev: workerEvent{File: tmp}}}, bpm, true, tmp, func(Event) {})
				if err != nil {
					t.Fatal(err)
				}
				if f, ok := stats["original"].(string); ok {
					_ = os.Remove(f)
					delete(stats, "original")
				}
				if err := os.Rename(ev.File, dst); err != nil {
					t.Fatal(err)
				}
				line, _ := json.Marshal(map[string]any{"config": c + "-lock", "prompt": p.id, "seed": seed, "file": dst, "stats": stats})
				_, _ = results.Write(append(line, '\n'))
				t.Logf("%s %s s%d: %v", c, p.id, seed, stats)
			}
		}
	}
}

// waitIdle waits while the job history at path has a job running or queued.
func waitIdle(t *testing.T, path string) {
	if path == "" {
		return
	}
	for {
		var jobs []struct {
			State string `json:"state"`
		}
		busy := false
		if bs, err := os.ReadFile(path); err == nil && json.Unmarshal(bs, &jobs) == nil {
			for _, j := range jobs {
				if j.State == "running" || j.State == "queued" {
					busy = true
				}
			}
		}
		if !busy {
			return
		}
		t.Log("the GPU is busy with another job, waiting …")
		time.Sleep(30 * time.Second)
	}
}
