package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/pyenv"
	"github.com/philipparndt/ai-connector/internal/styles"
)

// fakeACEStep stands in for both ACE-Step scripts (it is given the script's path): the trainer reports captions,
// steps, checkpoints and samples like train_acestep.py and stops at {"stop": true}; the song worker writes the
// request it got into the output file.
const fakeACEStep = `
import json, os, sys, threading, time
ev = os.fdopen(3, "w", buffering=1)
def emit(**kw): ev.write(json.dumps(kw) + "\n")
emit(type="ready")
if not any("train_acestep" in a or "train_sa3" in a for a in sys.argv):
    for line in sys.stdin:
        req = json.loads(line)
        with open(req["output"], "w") as f:
            json.dump(req, f)
        emit(type="result", file=req["output"], seconds=1.0, duration=req["duration"])
    sys.exit(0)
req = json.loads(sys.stdin.readline())
stop = threading.Event()
def watch():
    for line in sys.stdin:
        if json.loads(line).get("stop"):
            stop.set()
threading.Thread(target=watch, daemon=True).start()
sd = req["style_dir"]
os.makedirs(os.path.join(sd, "work"), exist_ok=True)
with open(os.path.join(sd, "work", "request.json"), "w") as f:
    json.dump(req, f)
ck = os.path.join(sd, "work", "checkpoint")
step = int(open(ck).read()) if os.path.exists(ck) else 0
emit(type="status", text="listening to segment 1 of 2 (captions) …")
emit(type="captions", captions={os.path.basename(s["file"]): "a fake caption" for s in req.get("segments") or req["tracks"]})
delay = float(os.environ.get("FAKE_STEP_DELAY", "0"))
while step < req["steps"]:
    if stop.is_set():
        open(ck, "w").write(str(step))
        emit(type="checkpoint", step=step, dir=ck)
        emit(type="result", file="", step=step, stopped=True, seconds=1)
        sys.exit(0)
    time.sleep(delay)
    step += 1
    emit(type="train", step=step, total=req["steps"], loss=1.0 / step, lr=1e-4, sps=0.5, eta=0.5 * (req["steps"] - step))
    emit(type="progress", step=step, total=req["steps"])
    if step % req["checkpoint_every"] == 0:
        open(ck, "w").write(str(step))
        emit(type="checkpoint", step=step, dir=ck)
    if req["sample_every"] and step % req["sample_every"] == 0:
        f = os.path.join(sd, "samples", "step-%06d.wav" % step)
        os.makedirs(os.path.dirname(f), exist_ok=True)
        open(f, "w").write("RIFF")
        emit(type="sample", step=step, file=f)
lora = os.path.join(sd, "lora")
os.makedirs(lora, exist_ok=True)
for f in ("adapter_config.json", "adapter_model.safetensors", "style.safetensors"):
    open(os.path.join(lora, f), "w").write("{}")
emit(type="result", file=lora, step=step, stopped=False, seconds=2)
`

// fakeMusicBackend is a MusicBackend whose ACE-Step runtime is the fake above (needs python3, no uv, no model);
// segments are "cut" by copying.
func fakeMusicBackend(t *testing.T) (*MusicBackend, *Model) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	root := t.TempDir()
	env := pyenv.Env{Name: "fake-acestep", Python: "x"}
	dir := env.Dir(root)
	must(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o755))
	fake := filepath.Join(root, "fake.py")
	must(t, os.WriteFile(fake, []byte(fakeACEStep), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "bin", "python"), []byte("#!/bin/sh\nexec "+py+" "+fake+" \"$@\"\n"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, ".mlxlab-ready"), nil, 0o644))
	b := &MusicBackend{Root: root, Store: &models.Store{Dir: filepath.Join(root, "models")},
		Runtimes: map[string]*musicRuntime{"acestep": {name: "acestep", env: env, script: "acestep_worker.py"},
			"stableaudio3": {name: "stableaudio3", env: env, script: "stableaudio3.py",
				build: func(context.Context, string, Emit) ([]string, error) { return []string{"/sa3-code"}, nil }}}}
	old := sa3TrainEnv
	sa3TrainEnv = env
	t.Cleanup(func() { sa3TrainEnv = old })
	m := &Model{ID: "ace-step-1.5-turbo", Name: "ACE-Step 1.5 turbo", Kind: Music, Backend: "music", runtime: "acestep",
		workerModel: "ace-step-1.5-turbo", Styles: true, Defaults: Request{Duration: 30}}
	return b, m
}

func fakeTools(t *testing.T) {
	cut, conv, dur := styles.Cutter, styles.Converter, styles.Duration
	t.Cleanup(func() { styles.Cutter, styles.Converter, styles.Duration = cut, conv, dur })
	copyFile := func(in, out string) error {
		b, err := os.ReadFile(in)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	}
	styles.Cutter = func(_ context.Context, in string, _, _ float64, out string) error { return copyFile(in, out) }
	styles.Converter = func(_ context.Context, in, out string) error { return copyFile(in, out) }
	styles.Duration = func(p string) float64 {
		b, _ := os.ReadFile(p)
		return float64(len(b)) // a "track" of n bytes lasts n seconds
	}
}

func TestTrainableModels(t *testing.T) {
	for _, m := range musicModels() {
		if want := (strings.HasPrefix(m.ID, "ace-step-1.5") && !strings.HasSuffix(m.ID, "-techno")) || m.ID == "stable-audio-3-medium"; m.Styles != want {
			t.Errorf("%s: styles %v", m.ID, m.Styles)
		}
	}
	if !CanTrain("ace-step-1.5-xl-turbo") || CanTrain("yue2-3b") || StepSeconds("ace-step-1.5-turbo") <= 0 {
		t.Fatal("trainable")
	}
}

func TestTrainStyleAndUseIt(t *testing.T) {
	fakeTools(t)
	b, m := fakeMusicBackend(t)
	defer b.Unload()
	lib := b.StyleLibrary()
	st, err := lib.Create("Deep Trance", m.ID, styles.Defaults())
	must(t, err)
	_, _, err = lib.AddTrack(context.Background(), st.ID, "one.wav", strings.NewReader(strings.Repeat("a", 70)), false)
	must(t, err)
	set := styles.Defaults()
	set.Steps, set.CheckpointEvery, set.SampleEvery = 6, 2, 3
	_, err = lib.Update(st.ID, styles.Patch{Settings: &set})
	must(t, err)

	// a song with the style before it is trained
	err = b.Run(context.Background(), m, Request{Prompt: "x", Style: st.ID}, t.TempDir(), (&recorder{}).emit)
	if err == nil || !strings.Contains(err.Error(), "not trained yet") {
		t.Fatalf("untrained: %v", err)
	}

	rec := &recorder{}
	must(t, b.Run(context.Background(), m, Request{Task: TrainStyle, Style: st.ID}, t.TempDir(), rec.emit))
	got, err := lib.Get(st.ID)
	must(t, err)
	if got.Status != styles.Trained || got.Weights != filepath.Join(lib.Path(st.ID), "lora") || got.Progress.Step != 6 ||
		got.Progress.Checkpoint != 6 || len(got.Progress.Losses) != 6 || len(got.Progress.Samples) != 2 ||
		got.Captions["fake"] != "" || len(got.Captions) != 3 {
		t.Fatalf("trained style %+v", got)
	}
	// what the trainer was asked: 70 s at 30 s segments = 2 full ones and one ending at the end
	var req map[string]any
	raw, err := os.ReadFile(filepath.Join(lib.Path(st.ID), "work", "request.json"))
	must(t, err)
	must(t, json.Unmarshal(raw, &req))
	if req["trigger"] != got.Trigger || req["steps"] != 6.0 || req["rank"] != 64.0 || len(req["segments"].([]any)) != 3 ||
		req["model"] != "ace-step-1.5-turbo" {
		t.Fatalf("request %v", req)
	}
	if !rec.has("log", "trained") {
		t.Fatalf("events %v", rec.evs)
	}

	// a song with it: the LoRA and the trigger word reach the worker
	out := t.TempDir()
	rec = &recorder{}
	must(t, b.Run(context.Background(), m, Request{Prompt: "dark, driving", Style: st.ID, StyleStrength: 0.7, Seed: 1}, out, rec.emit))
	var file string
	for _, e := range rec.evs {
		if e.Type == "file" {
			file = e.File
		}
	}
	raw, err = os.ReadFile(file)
	must(t, err)
	var song map[string]any
	must(t, json.Unmarshal(raw, &song))
	lora, _ := song["lora"].(map[string]any)
	if dirs, _ := song["dirs"].(map[string]any); dirs["lora"] != got.Weights || lora["file"] != "adapter_model.safetensors" ||
		lora["scale"] != 0.7 || song["prompt"] != got.Trigger+", dark, driving" {
		t.Fatalf("song request %v", song)
	}
	// the style's own strength when the request names none; no style: no LoRA
	must(t, b.Run(context.Background(), m, Request{Prompt: got.Trigger + " pads", Style: st.ID, Seed: 1}, out, rec.emit))
	must(t, b.Run(context.Background(), m, Request{Prompt: "plain", Seed: 2}, out, rec.emit))
	var reqs []map[string]any
	for _, e := range rec.evs {
		if e.Type == "file" {
			raw, _ := os.ReadFile(e.File)
			var r map[string]any
			_ = json.Unmarshal(raw, &r)
			reqs = append(reqs, r)
		}
	}
	if l := reqs[1]["lora"].(map[string]any); l["scale"] != 1.0 || reqs[1]["prompt"] != got.Trigger+" pads" {
		t.Fatalf("default strength %v", reqs[1])
	}
	if _, ok := reqs[2]["lora"]; ok {
		t.Fatalf("no style, a LoRA: %v", reqs[2])
	}

	// another model: a clear error
	other := *m
	other.ID = "ace-step-1.5-xl-turbo"
	if err := b.Run(context.Background(), &other, Request{Prompt: "x", Style: st.ID}, out, rec.emit); err == nil ||
		!strings.Contains(err.Error(), "trained for ace-step-1.5-turbo") {
		t.Fatalf("other model: %v", err)
	}
	yue := &Model{ID: "yue2-3b", Name: "YuE2", Kind: Music, runtime: "yue2"}
	b.Runtimes["yue2"] = &musicRuntime{name: "yue2"}
	if err := b.Run(context.Background(), yue, Request{Prompt: "x", Style: st.ID}, out, rec.emit); err == nil ||
		!strings.Contains(err.Error(), "cannot use a trained style") {
		t.Fatalf("yue: %v", err)
	}
	if err := b.Run(context.Background(), m, Request{Prompt: "x", Style: "nope"}, out, rec.emit); err == nil ||
		!strings.Contains(err.Error(), "no style") {
		t.Fatalf("unknown: %v", err)
	}
}

func TestTrainStyleCancelKeepsTheCheckpoint(t *testing.T) {
	fakeTools(t)
	t.Setenv("FAKE_STEP_DELAY", "0.05")
	b, m := fakeMusicBackend(t)
	defer b.Unload()
	lib := b.StyleLibrary()
	st, err := lib.Create("Techno", m.ID, styles.Defaults())
	must(t, err)
	_, _, err = lib.AddTrack(context.Background(), st.ID, "a.wav", strings.NewReader(strings.Repeat("a", 40)), false)
	must(t, err)
	set := styles.Defaults()
	set.Steps, set.CheckpointEvery, set.SampleEvery = 200, 10, 0
	_, err = lib.Update(st.ID, styles.Patch{Settings: &set})
	must(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			if s, _ := lib.Get(st.ID); s.Progress.Step >= 5 {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	rec := &recorder{}
	err = b.Run(ctx, m, Request{Task: TrainStyle, Style: st.ID}, t.TempDir(), rec.emit)
	if err == nil {
		t.Fatal("a canceled training must say so")
	}
	got, _ := lib.Get(st.ID)
	if got.Status != styles.Paused || got.Progress.Checkpoint < 5 || got.Progress.Checkpoint >= 200 || got.Weights != "" {
		t.Fatalf("paused style %+v", got.Progress)
	}
	stoppedAt := got.Progress.Checkpoint

	// the next training resumes from the checkpoint
	t.Setenv("FAKE_STEP_DELAY", "0")
	rec = &recorder{}
	if err := b.Run(context.Background(), m, Request{Task: TrainStyle, Style: st.ID}, t.TempDir(), rec.emit); err != nil {
		t.Fatalf("%v %v", err, rec.evs)
	}
	got, _ = lib.Get(st.ID)
	if got.Status != styles.Trained || got.Progress.Step != 200 || got.Progress.Losses[0].Step != 1 ||
		len(got.Progress.Losses) != 200 {
		t.Fatalf("resumed from %d: %+v", stoppedAt, got.Progress.Step)
	}
}

// Stable Audio 3: the trainer gets the tracks (not segments) with their measured tempo and the style's description;
// a song with the style gets the one-file LoRA and the trigger word.
func TestTrainSA3Style(t *testing.T) {
	fakeTools(t)
	b, _ := fakeMusicBackend(t)
	defer b.Unload()
	measured := 0
	old := trackTempo
	trackTempo = func(_ context.Context, _ string, files []string, _ Emit) (map[string]float64, error) {
		measured += len(files)
		out := map[string]float64{}
		for _, f := range files {
			out[f] = 137.9
		}
		return out, nil
	}
	t.Cleanup(func() { trackTempo = old })
	m := &Model{ID: "stable-audio-3-medium", Name: "Stable Audio 3 Medium", Kind: Music, Backend: "music", runtime: "stableaudio3",
		workerModel: "medium", Styles: true, Defaults: Request{Duration: 30}}
	lib := b.StyleLibrary()
	st, err := lib.Create("Uplifting", m.ID, StyleDefaults(m.ID))
	must(t, err)
	if st.Settings.Rank != 16 || st.Settings.Alpha != 16 || !StyleDescribed(m.ID) || StyleDescribed("ace-step-1.5-turbo") {
		t.Fatalf("defaults %+v", st.Settings)
	}
	desc := "uplifting trance, supersaw lead"
	_, err = lib.Update(st.ID, styles.Patch{Description: &desc})
	must(t, err)
	for _, n := range []string{"a", "bb"} {
		_, _, err = lib.AddTrack(context.Background(), st.ID, n+".wav", strings.NewReader(strings.Repeat(n, 40)), false)
		must(t, err)
	}
	set := StyleDefaults(m.ID)
	set.Steps, set.CheckpointEvery, set.SampleEvery = 4, 2, 2
	_, err = lib.Update(st.ID, styles.Patch{Settings: &set})
	must(t, err)
	must(t, b.Run(context.Background(), m, Request{Task: TrainStyle, Style: st.ID}, t.TempDir(), (&recorder{}).emit))
	got, err := lib.Get(st.ID)
	must(t, err)
	if got.Status != styles.Trained || got.Tracks[0].BPM != 137.9 || measured != 2 {
		t.Fatalf("trained %+v (measured %d)", got, measured)
	}
	var req map[string]any
	raw, err := os.ReadFile(filepath.Join(lib.Path(st.ID), "work", "request.json"))
	must(t, err)
	must(t, json.Unmarshal(raw, &req))
	tracks := req["tracks"].([]any)
	if req["model"] != "medium" || req["description"] != desc || req["crop_seconds"] != 30.0 || req["rank"] != 16.0 ||
		len(tracks) != 2 || tracks[0].(map[string]any)["bpm"] != 137.9 || req["segments"] != nil {
		t.Fatalf("request %v", req)
	}
	// trained again: the tempo is not measured again
	must(t, b.Run(context.Background(), m, Request{Task: TrainStyle, Style: st.ID}, t.TempDir(), (&recorder{}).emit))
	if measured != 2 {
		t.Fatalf("measured again: %d", measured)
	}

	rec := &recorder{}
	must(t, b.Run(context.Background(), m, Request{Prompt: "anthem", Style: st.ID, StyleStrength: 0.8, Seed: 1}, t.TempDir(), rec.emit))
	var song map[string]any
	for _, e := range rec.evs {
		if e.Type == "file" {
			raw, _ := os.ReadFile(e.File)
			_ = json.Unmarshal(raw, &song)
		}
	}
	lora, _ := song["lora"].(map[string]any)
	if dirs, _ := song["dirs"].(map[string]any); dirs["lora"] != got.Weights || lora["file"] != "style.safetensors" ||
		lora["scale"] != 0.8 || !strings.HasPrefix(song["prompt"].(string), got.Trigger+", anthem") {
		t.Fatalf("song request %v", song)
	}
}
