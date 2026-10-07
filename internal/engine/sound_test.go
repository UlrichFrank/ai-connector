package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/philipparndt/ai-connector/internal/pyenv"
)

func TestClampSoundDuration(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{
		{0, soundDefaultSeconds}, {-3, soundDefaultSeconds}, {0.2, 1}, {1, 1}, {7.5, 7.5}, {30, 30}, {31, 30}, {600, 30},
	} {
		if got := clampSoundDuration(c.in); got != c.want {
			t.Errorf("clampSoundDuration(%g) = %g, want %g", c.in, got, c.want)
		}
	}
}

// fakeSoundWorker speaks the worker protocol: it writes the request it got into the output file and reports a result
// (or an error when the prompt says "fail").
const fakeSoundWorker = `
import json, os, sys
ev = os.fdopen(3, "w", buffering=1)
def emit(**kw): ev.write(json.dumps(kw) + "\n")
emit(type="ready")
for line in sys.stdin:
    req = json.loads(line)
    if req["prompt"] == "fail":
        emit(type="error", text="ValueError: no")
        continue
    print("library output")
    sys.stdout.flush()
    emit(type="status", text="generating")
    emit(type="progress", step=1, total=2)
    with open(req["output"], "w") as f:
        json.dump(req, f)
    emit(type="result", file=req["output"], seconds=1.5, duration=req["duration"], peakGB=6.5, sampleRate=24000)
`

// fakeSoundBackend is a SoundBackend whose runtime is a fake worker (needs python3, no uv, no model).
func fakeSoundBackend(t *testing.T) *SoundBackend {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	root := t.TempDir()
	env := pyenv.Env{Name: "fake-sound", Python: "x"}
	dir := env.Dir(root)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(root, "fake.py")
	must(t, os.WriteFile(fake, []byte(fakeSoundWorker), 0o644))
	// the backend runs <env>/bin/python -u <script>: the fake ignores the script it is given
	must(t, os.WriteFile(filepath.Join(dir, "bin", "python"), []byte("#!/bin/sh\nexec "+py+" -u "+fake+"\n"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, ".mlxlab-ready"), nil, 0o644))
	b := &SoundBackend{Root: root, env: &env}
	b.fetch = func(context.Context, *Model, Emit) (map[string]string, error) {
		return map[string]string{"model": "/m", "codec": "/c"}, nil
	}
	return b
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type recorder struct {
	mu  sync.Mutex
	evs []Event
}

func (r *recorder) emit(e Event) { r.mu.Lock(); r.evs = append(r.evs, e); r.mu.Unlock() }

func (r *recorder) has(typ, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.evs {
		if e.Type == typ && strings.Contains(e.Text, text) {
			return true
		}
	}
	return false
}

func TestSoundBackendWorkerProtocol(t *testing.T) {
	b := fakeSoundBackend(t)
	defer b.Unload()
	m := soundModels()[0]
	out := t.TempDir()
	rec := &recorder{}

	if err := b.Run(context.Background(), m, Request{Prompt: "  "}, out, rec.emit); err == nil {
		t.Fatal("an empty description must be refused")
	}

	// a too long sound is clamped before it reaches the worker
	err := b.Run(context.Background(), m, Request{Prompt: "rain on a tin roof", Duration: 120, Seed: 7}, out, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	var file string
	var stats []map[string]any
	for _, e := range rec.evs {
		switch e.Type {
		case "file":
			file = e.File
		case "stats":
			stats = append(stats, e.Data)
		}
	}
	if file == "" || filepath.Dir(file) != out || !strings.HasSuffix(file, "-7.wav") {
		t.Fatalf("file %q", file)
	}
	raw, err := os.ReadFile(file)
	must(t, err)
	var req map[string]any
	must(t, json.Unmarshal(raw, &req))
	if req["model"] != "moss-soundeffect" || req["prompt"] != "rain on a tin roof" || req["duration"] != 30.0 || req["seed"] != 7.0 {
		t.Fatalf("request %v", req)
	}
	if dirs, _ := req["dirs"].(map[string]any); dirs["model"] != "/m" || dirs["codec"] != "/c" {
		t.Fatalf("dirs %v", req["dirs"])
	}
	if len(stats) != 2 || stats[1]["peakGB"] != 6.5 || stats[1]["duration"] != 30.0 {
		t.Fatalf("stats %v", stats)
	}
	if !rec.has("status", "generating") || !rec.has("log", "30 s instead of 120 s") {
		t.Fatalf("events %v", rec.evs)
	}
	if b.Loaded() != m.ID {
		t.Fatalf("loaded %q", b.Loaded())
	}

	// a worker error is the job's error; the worker keeps running for the next sound
	err = b.Run(context.Background(), m, Request{Prompt: "fail", Duration: 2}, out, rec.emit)
	if err == nil || !strings.Contains(err.Error(), "ValueError: no") {
		t.Fatalf("error %v", err)
	}
	if !b.worker.running() || b.Loaded() != m.ID {
		t.Fatal("the worker stopped after an error")
	}
	if err := b.Run(context.Background(), m, Request{Prompt: "a door creaking"}, out, rec.emit); err != nil {
		t.Fatal(err)
	}

	b.Unload()
	if b.Loaded() != "" || b.worker.running() {
		t.Fatal("not unloaded")
	}
}

func TestSoundCatalogEntries(t *testing.T) {
	for _, m := range soundModels() {
		if m.Kind != Sound || m.Backend != "sound" || m.Licence == "" || len(m.repos) == 0 {
			t.Errorf("%s: %+v", m.ID, m)
		}
		for _, hr := range m.repos {
			if len(hr.rev) != 40 {
				t.Errorf("%s: %s is not pinned", m.ID, hr.repo)
			}
		}
	}
	if len(Examples[Sound]) < 6 {
		t.Error("sound examples missing")
	}
}
