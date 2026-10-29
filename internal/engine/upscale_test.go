package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/models"
)

func TestSeedVR2Catalog(t *testing.T) {
	store := &models.Store{Dir: t.TempDir()}
	ms := manifestModels(store, "", "seedvr2-")
	seedvr2Rev := ms[0].rev
	if len(ms) != 2 || ms[0].ID != "seedvr2-3b" || ms[1].ID != "seedvr2-7b" {
		t.Fatalf("%+v", ms)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(seedvr2Rev) {
		t.Fatal("the revision is pinned")
	}
	for _, m := range ms {
		if m.Kind != Image || m.Backend != "mflux" || m.mflux != m.ID || m.repo != "numz/SeedVR2_comfyUI" || m.Here ||
			m.Licence != "Apache-2.0" || m.NonCommercial || len(m.Tasks) != 1 || m.Tasks[0] != TaskUpscale || m.Defaults.Scale != 2 {
			t.Errorf("%s: %+v", m.ID, m)
		}
		if lic, nc := licenceOf(m); lic != "Apache-2.0" || nc {
			t.Errorf("%s: licence %q", m.ID, lic)
		}
	}
	if ms[0].Size != 7284343622 || ms[1].Size != 16980659238 {
		t.Errorf("sizes %d %d", ms[0].Size, ms[1].Size)
	}
	// only the fp16 transformer of its size and the VAE: not the other model's, the fp8 copies or the rest
	keep3 := func(p string) bool { return models.Matches(p, ms[0].files...) }
	keep7 := func(p string) bool { return models.Matches(p, ms[1].files...) }
	if !keep3("seedvr2_ema_3b_fp16.safetensors") || !keep3("ema_vae_fp16.safetensors") || !keep7("seedvr2_ema_7b_fp16.safetensors") || !keep7("ema_vae_fp16.safetensors") {
		t.Error("a needed file is not fetched")
	}
	for _, p := range []string{"seedvr2_ema_7b_fp16.safetensors", "seedvr2_ema_3b_fp8_e4m3fn.safetensors", "seedvr2_ema_7b_sharp_fp16.safetensors", "README.md", "config.json", ".gitattributes"} {
		if keep3(p) {
			t.Errorf("3B fetches %s", p)
		}
	}
	if keep7("seedvr2_ema_3b_fp16.safetensors") || keep7("seedvr2_ema_7b_sharp_fp16.safetensors") {
		t.Error("7B fetches another transformer")
	}

	// here: the 3B with its transformer and the VAE verified in the store; the 7B not (its transformer is missing)
	d := filepath.Join(store.Dir, "numz", "SeedVR2_comfyUI", seedvr2Rev)
	os.MkdirAll(d, 0o755)
	for _, f := range []string{"seedvr2_ema_3b_fp16.safetensors.sha256", "ema_vae_fp16.safetensors.sha256"} {
		os.WriteFile(filepath.Join(d, f), []byte("x"), 0o644)
	}
	ms = manifestModels(store, "", "seedvr2-")
	if !ms[0].Here || ms[0].Where != d || ms[1].Here {
		t.Fatalf("here: 3B %v %q, 7B %v", ms[0].Here, ms[0].Where, ms[1].Here)
	}
	// the setup lists them as image models (downloadable through mflux)
	dl := &Downloads{Catalog: &Catalog{Models: ms}, Store: store, Free: func() int64 { return 100 * gb }}
	got := dl.Models(context.Background(), 64*gb, false)
	if len(got) != 2 || got[1].ID != "seedvr2-7b" || got[1].Kind != Image || got[1].Licence != "Apache-2.0" || !got[1].Fits || got[1].Recommended {
		t.Fatalf("setup %+v", got)
	}
}

// UpscaleSize as mflux makes it (the worker script's upscale_resolution, then SeedVR2Util.preprocess_image): the
// expected sizes were worked out with that Python code.
func TestUpscaleSize(t *testing.T) {
	for _, c := range []struct{ w, h, scale, wantW, wantH int }{
		{1024, 1024, 2, 2048, 2048},
		{1024, 1024, 4, 4096, 4096},
		{1000, 600, 2, 2000, 1200},
		{2048, 2048, 2, 4096, 4096}, // just fits
		{2048, 1000, 4, 4096, 2000}, // cut to 4096
		{1920, 1080, 4, 4096, 2304},
		{1366, 768, 4, 4094, 2302}, // cut: the shorter side 768 × 4096 / 1366 = 2302, the longer in proportion
		{800, 1200, 4, 2730, 4094},
		{1111, 999, 4, 4094, 3682},
		{2049, 1537, 2, 4094, 3072},
		{333, 2047, 2, 666, 4094},
		{1023, 767, 4, 4092, 3068}, // odd sides: rounded down to even
		{3, 5, 2, 6, 10},
		{0, 5, 2, 0, 0},
		{10, 10, 0, 0, 0},
	} {
		w, h := UpscaleSize(c.w, c.h, c.scale, UpscaleMax)
		if w != c.wantW || h != c.wantH {
			t.Errorf("%d×%d ×%d: %d×%d, want %d×%d", c.w, c.h, c.scale, w, h, c.wantW, c.wantH)
		}
	}
}

// The request mapping: one image and a scale (0: 2) go to the mflux worker with the weights' folder; no prompt is
// needed; the size it made comes back in the stats. Wrong requests are refused before anything is loaded.
func TestUpscaleRun(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 runs the fake worker")
	}
	root, out := t.TempDir(), t.TempDir()
	// a fake mflux environment: its python logs the request and answers with a picture of 64×48
	env := MfluxEnv.Dir(root)
	os.MkdirAll(filepath.Join(env, "bin"), 0o755)
	os.WriteFile(filepath.Join(env, ".mlxlab-ready"), nil, 0o644)
	made := filepath.Join(root, "made.png")
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 64, 48)))
	os.WriteFile(made, b.Bytes(), 0o644)
	reqs := filepath.Join(root, "requests.jsonl")
	fake := filepath.Join(root, "fake.py")
	os.WriteFile(fake, []byte(`import json, os, shutil, sys
ev = os.fdopen(3, "w", buffering=1)
ev.write(json.dumps({"type": "ready"}) + "\n")
for line in sys.stdin:
    req = json.loads(line)
    open(`+pyStr(reqs)+`, "a").write(line)
    ev.write(json.dumps({"type": "progress", "step": 1, "total": 1}) + "\n")
    shutil.copy(`+pyStr(made)+`, req["output"])
    ev.write(json.dumps({"type": "result", "file": req["output"], "seconds": 1.5, "peakGB": 3.0}) + "\n")
`), 0o644)
	os.WriteFile(filepath.Join(env, "bin", "python"), []byte("#!/bin/sh\nexec "+py+" "+fake+"\n"), 0o755)

	// the store has both files of the 3B (verified): nothing is fetched
	store := &models.Store{Dir: filepath.Join(root, "models")}
	dir := filepath.Join(store.Dir, "numz", "SeedVR2_comfyUI", manifestModel(t, store, "", "seedvr2-3b").rev)
	os.MkdirAll(dir, 0o755)
	var files []models.File
	for _, f := range []string{"seedvr2_ema_3b_fp16.safetensors", "ema_vae_fp16.safetensors", "seedvr2_ema_7b_fp16.safetensors", "README.md"} {
		files = append(files, models.File{Path: f, Size: 3, SHA256: "s" + f})
		if f != "seedvr2_ema_7b_fp16.safetensors" {
			os.WriteFile(filepath.Join(dir, f), []byte("abc"), 0o644)
			os.WriteFile(filepath.Join(dir, f+".sha256"), []byte("s"+f), 0o644)
		}
	}
	list, _ := json.Marshal(files)
	os.WriteFile(filepath.Join(dir, ".files.json"), list, 0o644)

	m := manifestModel(t, store, "", "seedvr2-3b")
	be := &FluxBackend{Root: root, Store: store}
	t.Cleanup(be.Unload)
	src := filepath.Join(root, "in.png")
	var evs []Event
	emit := func(e Event) { evs = append(evs, e) }
	if err := be.Run(context.Background(), m, Request{Images: []string{src}, Seed: 5}, out, emit); err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	line, _ := os.ReadFile(reqs)
	if err := json.Unmarshal(line, &req); err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	if req["model"] != "seedvr2-3b" || req["upscale"] != true || req["image"] != src || req["scale"] != 2.0 || req["maxSide"] != 4096.0 ||
		req["seed"] != 5.0 || req["quantize"] != 0.0 || req["path"] != dir || !strings.HasPrefix(req["output"].(string), out) {
		t.Fatalf("request %v", req)
	}
	var file string
	var last map[string]any
	for _, e := range evs {
		switch e.Type {
		case "file":
			file = e.File
		case "stats":
			last = e.Data
		}
	}
	if file != req["output"] || last["width"] != 64 || last["height"] != 48 || be.Loaded() != "seedvr2-3b" {
		t.Fatalf("file %q stats %v loaded %q", file, last, be.Loaded())
	}

	// ×4, 8-bit
	if err := be.Run(context.Background(), m, Request{Images: []string{src}, Scale: 4, Quantize: 8}, out, emit); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(readFile(t, reqs)), "\n")
	json.Unmarshal([]byte(lines[len(lines)-1]), &req)
	if req["scale"] != 4.0 || req["quantize"] != 8.0 || req["seed"].(float64) <= 0 {
		t.Fatalf("request %v", req)
	}

	// refused before the worker is asked
	for _, c := range []struct {
		r    Request
		want string
	}{
		{Request{}, "exactly one"},
		{Request{Images: []string{src, src}}, "exactly one"},
		{Request{Images: []string{src}, Scale: 8}, "scale 8"},
		{Request{Images: []string{src}, Scale: 1}, "scale 1"},
		{Request{Images: []string{src}, Scale: -2}, "scale -2"},
	} {
		if err := be.Run(context.Background(), m, c.r, out, emit); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c.r, err)
		}
	}
	if n := len(strings.Split(strings.TrimSpace(readFile(t, reqs)), "\n")); n != 2 {
		t.Fatalf("%d requests reached the worker", n)
	}
}

func pyStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
