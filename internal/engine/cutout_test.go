package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/models"
)

func TestCutoutCatalog(t *testing.T) {
	store := &models.Store{Dir: t.TempDir()}
	ms := cutoutCatalog(store, "")
	if len(ms) != 3 || ms[0].ID != "birefnet" || ms[1].ID != "birefnet-hr-matting" || ms[2].ID != "birefnet-lite" {
		t.Fatalf("%+v", ms)
	}
	for _, m := range ms {
		if m.Kind != Image || m.Backend != "cutout" || !strings.HasPrefix(m.repo, "ZhengPeng7/BiRefNet") || m.Here ||
			m.Licence != "MIT" || m.NonCommercial || len(m.Tasks) != 1 || m.Tasks[0] != TaskCutout || m.Size < 160<<20 || m.Size > 450<<20 {
			t.Errorf("%s: %+v", m.ID, m)
		}
		if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(m.rev) {
			t.Errorf("%s: the revision is pinned", m.ID)
		}
		if env, build := runtimeOf(m); env != &CutoutEnv || build != nil {
			t.Errorf("%s: runtime %v", m.ID, env)
		}
		if r := SetupRole(m.Kind, m.Tasks); r != "image/cutout" {
			t.Errorf("%s: role %q", m.ID, r)
		}
	}
	if ms[0].side != 1024 || ms[1].side != 2048 || ms[2].side != 1024 {
		t.Errorf("sides %d %d %d", ms[0].side, ms[1].side, ms[2].side)
	}
	if recommendedOf("image/cutout", 16*gb) != "birefnet" || recommendedOf(string(Image), 16*gb) != "flux2-klein-4b" {
		t.Error("recommended")
	}
	// the code, the config and the weights; not the README, the handler or the requirements
	keep := func(p string) bool { return models.Matches(p, ms[0].files...) }
	for _, p := range []string{"birefnet.py", "BiRefNet_config.py", "config.json", "model.safetensors"} {
		if !keep(p) {
			t.Errorf("%s is not fetched", p)
		}
	}
	for _, p := range []string{"README.md", "handler.py", "requirements.txt", ".gitattributes"} {
		if keep(p) {
			t.Errorf("%s is fetched", p)
		}
	}
	// every package of the runtime pinned
	for _, p := range CutoutEnv.Packages {
		if !strings.Contains(p, "==") {
			t.Errorf("%s is not pinned", p)
		}
	}

	// here: BiRefNet with its weights and code verified in the store; the others not
	d := filepath.Join(store.Dir, "ZhengPeng7", "BiRefNet", ms[0].rev)
	must(t, os.MkdirAll(d, 0o755))
	for _, f := range []string{"model.safetensors.sha256", "birefnet.py.sha256"} {
		must(t, os.WriteFile(filepath.Join(d, f), []byte("x"), 0o644))
	}
	ms = cutoutCatalog(store, "")
	if !ms[0].Here || ms[0].Where != d || ms[1].Here || ms[2].Here {
		t.Fatalf("here: %v %q, %v, %v", ms[0].Here, ms[0].Where, ms[1].Here, ms[2].Here)
	}
	// the setup lists them as image models with the task, BiRefNet recommended among the cutouts beside FLUX
	dl := &Downloads{Catalog: &Catalog{Models: ms}, Store: store, Free: func() int64 { return 100 * gb }}
	got := dl.Models(context.Background(), 16*gb, false)
	if len(got) != 3 || got[0].Kind != Image || !got[0].Recommended || got[1].Recommended || got[0].Licence != "MIT" || !got[2].Fits ||
		len(got[0].Tasks) != 1 || got[0].Tasks[0] != TaskCutout {
		t.Fatalf("setup %+v", got)
	}
}

// The request mapping: the one image goes to the worker with the snapshot's folder, the side the model sees and the
// PNG limit; no prompt is needed; the size it made comes back in the stats; a WebP result is the job's file as it is.
// Wrong requests are refused before anything is loaded.
func TestCutoutRun(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 runs the fake worker")
	}
	root, out := t.TempDir(), t.TempDir()
	env := CutoutEnv.Dir(root)
	must(t, os.MkdirAll(filepath.Join(env, "bin"), 0o755))
	must(t, os.WriteFile(filepath.Join(env, ".mlxlab-ready"), nil, 0o644))
	reqs := filepath.Join(root, "requests.jsonl")
	fake := filepath.Join(root, "fake.py")
	// the fake worker logs the request; an image named *.big.png is "too large": it answers with a WebP
	must(t, os.WriteFile(fake, []byte(`import json, os, sys
ev = os.fdopen(3, "w", buffering=1)
ev.write(json.dumps({"type": "ready"}) + "\n")
for line in sys.stdin:
    req = json.loads(line)
    open(`+pyStr(reqs)+`, "a").write(line)
    ev.write(json.dumps({"type": "progress", "step": 1, "total": 2}) + "\n")
    out = req["output"]
    if req["image"].endswith(".big.png"):
        out = out[:-4] + ".webp"
    open(out, "wb").write(b"cut")
    ev.write(json.dumps({"type": "result", "file": out, "seconds": 0.7, "width": 64, "height": 48, "peakGB": 1.5}) + "\n")
`), 0o644))
	must(t, os.WriteFile(filepath.Join(env, "bin", "python"), []byte("#!/bin/sh\nexec "+py+" "+fake+"\n"), 0o755))

	// the store has BiRefNet's files (verified): nothing is fetched
	store := &models.Store{Dir: filepath.Join(root, "models")}
	m := cutoutCatalog(store, "")[1] // the HR matting model: it sees 2048²
	dir := filepath.Join(store.Dir, filepath.FromSlash(m.repo), m.rev)
	must(t, os.MkdirAll(dir, 0o755))
	var files []models.File
	for _, f := range []string{"birefnet.py", "BiRefNet_config.py", "config.json", "model.safetensors", "README.md"} {
		files = append(files, models.File{Path: f, Size: 3, SHA256: "s" + f})
		must(t, os.WriteFile(filepath.Join(dir, f), []byte("abc"), 0o644))
		must(t, os.WriteFile(filepath.Join(dir, f+".sha256"), []byte("s"+f), 0o644))
	}
	list, _ := json.Marshal(files)
	must(t, os.WriteFile(filepath.Join(dir, ".files.json"), list, 0o644))

	be := &CutoutBackend{Root: root, Store: store}
	t.Cleanup(be.Unload)
	src := filepath.Join(root, "in.png")
	rec := &recorder{}
	if err := be.Run(context.Background(), m, Request{Images: []string{src}, Prompt: "ignored"}, out, rec.emit); err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(readFile(t, reqs)), &req); err != nil {
		t.Fatal(err)
	}
	if req["model"] != "birefnet-hr-matting" || req["path"] != dir || req["size"] != 2048.0 || req["image"] != src ||
		req["maxBytes"] != float64(CutoutMaxPNG) || !strings.HasPrefix(req["output"].(string), out) || !strings.HasSuffix(req["output"].(string), ".png") {
		t.Fatalf("request %v", req)
	}
	var file string
	var last map[string]any
	for _, e := range rec.evs {
		switch e.Type {
		case "file":
			file = e.File
		case "stats":
			last = e.Data
		}
	}
	if file != req["output"] || last["width"] != 64 || last["height"] != 48 || last["peakGB"] != 1.5 || be.Loaded() != m.ID {
		t.Fatalf("file %q stats %v loaded %q", file, last, be.Loaded())
	}
	if !rec.has("log", "cut out 64x48") {
		t.Fatalf("events %v", rec.evs)
	}

	// too large as a PNG: the worker's WebP is the file
	rec = &recorder{}
	if err := be.Run(context.Background(), m, Request{Images: []string{filepath.Join(root, "in.big.png")}}, out, rec.emit); err != nil {
		t.Fatal(err)
	}
	for _, e := range rec.evs {
		if e.Type == "file" && !strings.HasSuffix(e.File, ".webp") {
			t.Fatalf("file %q", e.File)
		}
	}

	// refused before the worker is asked
	for _, r := range []Request{{}, {Images: []string{src, src}}, {Images: []string{""}}} {
		if err := be.Run(context.Background(), m, r, out, rec.emit); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if n := len(strings.Split(strings.TrimSpace(readFile(t, reqs)), "\n")); n != 2 {
		t.Fatalf("%d requests reached the worker", n)
	}
}
