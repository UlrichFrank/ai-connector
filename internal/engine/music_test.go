package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestStyleBPM(t *testing.T) {
	for style, want := range map[string]int{
		"uplifting trance, 138 bpm, supersaw lead":   138,
		"drum and bass, 174BPM":                      174,
		"techno at 130 Bpm":                          130,
		"house 124.5 bpm":                            124,
		"lo-fi, 80-bpm, vinyl":                       80,
		"jazz ballad, slow":                          0,
		"1000 bpm noise":                             0,
		"rock, 20 bpm":                               0,
		"track 2, 128 beats per minute (not parsed)": 0,
		// MiniMax Music 3's structured captions
		"Genre: synth-pop. BPM: 120. Key: A minor": 120,
		"Basic Attributes: bpm is 92. key is E":    92,
		"bpm=140, bpm of 1000":                     140,
		"BPM 96, warm":                             96,
		"bpm is fast":                              0,
	} {
		if got := styleBPM(style); got != want {
			t.Errorf("styleBPM(%q) = %d, want %d", style, got, want)
		}
	}
}

func TestWithBPM(t *testing.T) {
	if got := withBPM("techno, pounding kick.", 130); got != "techno, pounding kick, 130 BPM" {
		t.Errorf("got %q", got)
	}
	if got := withBPM("techno, 128 bpm", 130); got != "techno, 128 bpm" {
		t.Errorf("a tempo in the prompt stays: got %q", got)
	}
	if got := withBPM("techno", 0); got != "techno" {
		t.Errorf("got %q", got)
	}
}

func findMusic(t *testing.T, id string) *Model {
	t.Helper()
	for _, m := range musicModels() {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("no music model %s", id)
	return nil
}

func TestMusicRequestACEStep(t *testing.T) {
	m := findMusic(t, "ace-step-1.5-base")
	r := Request{Prompt: "uplifting trance, 138 bpm, punchy kick", Instrumental: true, Duration: 45, Steps: 50, Guidance: 7.5, Seed: 3}
	req, err := musicRequest(m, r, "generate", map[string]string{"main": "/m"}, "/o.wav")
	if err != nil {
		t.Fatal(err)
	}
	if req["bpm"] != 138 || req["guidance"] != 7.5 || req["steps"] != 50 {
		t.Errorf("EDM request: %v", req)
	}
	r.BPM = 140
	if req, _ = musicRequest(m, r, "generate", nil, "/o.wav"); req["bpm"] != 140 {
		t.Errorf("the request's BPM wins over the style's: %v", req["bpm"])
	}
	// the planner keeps its defaults (its caption, its temperature)
	r = Request{Prompt: "rock ballad, 72 bpm", Duration: 30}
	req, _ = musicRequest(m, r, "generate", nil, "/o.wav")
	if _, ok := req["cot_caption"]; ok {
		t.Errorf("rock: %v", req)
	}
	if req["bpm"] != 72 {
		t.Errorf("rock bpm: %v", req["bpm"])
	}
	r.Planner = "off"
	if req, _ = musicRequest(m, r, "generate", nil, "/o.wav"); req["lm"] != false {
		t.Errorf("planner off: %v", req)
	}
}

func TestMusicRequestLoRA(t *testing.T) {
	m := findMusic(t, "ace-step-1.5-xl-turbo-techno")
	req, _ := musicRequest(m, Request{Prompt: "dark techno, 130 bpm"}, "generate", nil, "/o.wav")
	if req["prompt"] != "roti-t3kn0, dark techno, 130 bpm" || req["model"] != "ace-step-1.5-xl-turbo" {
		t.Errorf("LoRA request: %v", req)
	}
	if l, ok := req["lora"].(map[string]any); !ok || l["file"] != "techno-xl-v1.safetensors" {
		t.Errorf("LoRA: %v", req["lora"])
	}
}

func TestMusicRequestStableAudio(t *testing.T) {
	m := findMusic(t, "stable-audio-3-medium")
	r := Request{Prompt: "techno, pounding kick", Lyrics: "[verse]\nla la", BPM: 130, Duration: 45, Seed: 1}
	req, _ := musicRequest(m, r, "generate", nil, "/o.wav")
	if req["prompt"] != "techno, pounding kick, 130 BPM" || req["model"] != "medium" {
		t.Errorf("SA3 request: %v", req)
	}
	if _, ok := req["lyrics"]; ok {
		t.Errorf("SA3 gets no lyrics: %v", req)
	}
	req, _ = musicRequest(m, Request{Prompt: "x", Source: "/s.wav", Strength: 0.5}, "cover", nil, "/o.wav")
	if req["task"] != "cover" || req["strength"] != 0.5 || req["src_audio"] != "/s.wav" {
		t.Errorf("SA3 remix: %v", req)
	}
	if m.maxSeconds != 380 || findMusic(t, "stable-audio-3-small").maxSeconds != 120 {
		t.Error("SA3 length limits")
	}
	if m.Licence == "" || m.NonCommercial {
		t.Error("SA3 licence: shown, commercial use below $1M revenue")
	}
}

func tgz(t *testing.T, files ...string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(name)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(name))
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestUntarSubdir(t *testing.T) {
	dir := t.TempDir()
	n, err := untarSubdir(tgz(t, "stable-audio-3-abc/README.md", "stable-audio-3-abc/optimized/mlx/scripts/sa3_mlx.py",
		"stable-audio-3-abc/optimized/mlx/models/defs/x.py", "stable-audio-3-abc/optimized/tflite/scripts/other.py"), "optimized/mlx/", dir)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "optimized", "mlx", "scripts", "sa3_mlx.py")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err == nil {
		t.Error("only the sub folder")
	}
	if _, err := untarSubdir(tgz(t, "x/optimized/mlx/../../../../evil.txt"), "optimized/mlx/", dir); err == nil {
		t.Error("a path out of the folder must fail")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.txt")); err == nil {
		t.Error("wrote outside the folder")
	}
}
