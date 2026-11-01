package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/models"
)

// onPlatform makes this test see the runtimes as on platform p: most environments exist only for Apple silicon, the
// machine running the tests may be another.
func onPlatform(t *testing.T, p string) {
	old := Platform
	Platform = func() string { return p }
	t.Cleanup(func() { Platform = old })
}

func TestPlatformOf(t *testing.T) {
	none := func(string) bool { return false }
	only := func(k string) func(string) bool { return func(x string) bool { return x == k } }
	for _, c := range []struct {
		goos, goarch, gpu string
		have              func(string) bool
		want              string
	}{
		{"darwin", "arm64", "", none, "darwin-arm64"},
		{"darwin", "arm64", "cuda", none, "darwin-arm64"},
		{"linux", "amd64", "", only("cuda"), "linux-cuda"},
		{"linux", "amd64", "", only("rocm"), "linux-rocm"},
		{"linux", "amd64", "", none, "linux-cpu"},
		{"linux", "amd64", "cpu", only("cuda"), "linux-cpu"},
		{"windows", "amd64", "", only("rocm"), "windows-rocm"},
	} {
		if got := platformOf(c.goos, c.goarch, c.gpu, c.have); got != c.want {
			t.Errorf("%s/%s gpu=%q: %s, want %s", c.goos, c.goarch, c.gpu, got, c.want)
		}
	}
}

// The runtimes keep the environments they had in Go: the same directory (name and hash), so nothing is installed
// again. The hashes are those of the environments on a Mac that ran them.
func TestRuntimeEnvsKept(t *testing.T) {
	for name, dir := range map[string]string{"mflux": "mflux-bf654883", "acestep": "acestep-303fb642"} {
		e, ok := envFor(name, "darwin-arm64")
		if !ok || filepath.Base(e.Dir("/r")) != dir {
			t.Errorf("%s: %s, want %s", name, e.Dir("/r"), dir)
		}
	}
	// BiRefNet runs on Linux (CUDA, ROCm with PyTorch's wheels, the CPU); the MLX runtimes only on Apple silicon
	if _, ok := envFor("cutout", "linux-rocm"); !ok {
		t.Error("cutout has no linux-rocm environment")
	}
	if e, _ := envFor("cutout", "linux-rocm"); !strings.Contains(e.Index, "rocm") {
		t.Errorf("cutout on ROCm without PyTorch's ROCm index: %+v", e)
	}
	for _, n := range []string{"mflux", "speech", "acestep"} {
		if _, ok := envFor(n, "linux-cuda"); ok {
			t.Errorf("%s on linux-cuda: it is MLX", n)
		}
	}
	// every model's runtime is there, and every runtime with a script has it in workers/
	for _, n := range runtimeNames() {
		rt := runtimeManifest(n)
		if rt.Script != "" && rt.dir == "" {
			if _, err := workerFiles.ReadFile("workers/" + rt.Script); err != nil {
				t.Errorf("%s: %v", n, err)
			}
		}
	}
	for _, mf := range builtinManifests() {
		if n := runtimeName(mf.model(nil, "")); runtimeManifest(n) == nil {
			t.Errorf("%s: no runtime %q", mf.ID, n)
		}
	}
	if c := packageCommit("longcat-avatar", "longcat-video-avatar-mlx"); len(c) != 40 {
		t.Errorf("longcat commit %q", c)
	}
}

func TestParseRuntime(t *testing.T) {
	ok := `{"name": "spandrel", "script": "upscale.py", "generic": true, "kinds": ["image"], "output": "png",
	  "envs": {"darwin-arm64": {"python": "3.12", "packages": ["spandrel==0.4.1"]}}}`
	if _, err := ParseRuntime([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ from, to, want string }{
		{`"darwin-arm64"`, `"amiga"`, "platform"},
		{`"generic": true, `, `"generic": true, "build": "make", `, "build step"},
		{`"output": "png",`, ``, "generic runtime needs"},
		{`"python": "3.12", `, ``, "python"},
		{`"name": "spandrel"`, `"name": "Spandrel!"`, "name"},
		{`"kinds"`, `"kindz"`, "unknown field"},
	} {
		if _, err := ParseRuntime([]byte(strings.Replace(ok, c.from, c.to, 1))); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s → %s: %v, want %q", c.from, c.to, err, c.want)
		}
	}
}

func TestCheckParams(t *testing.T) {
	one, four := 1.0, 4.0
	m := &Model{ID: "x", Params: map[string]ParamSpec{
		"scale": {Type: "integer", Minimum: &one, Maximum: &four, Default: 2.0},
		"face":  {Type: "boolean"},
		"mode":  {Type: "string", Enum: []any{"fast", "fine"}, Default: "fine"},
	}}
	got, err := CheckParams(m, map[string]any{"scale": 3.0})
	if err != nil || got["scale"] != 3.0 || got["mode"] != "fine" || got["face"] != nil {
		t.Errorf("%v %v", got, err)
	}
	for in, want := range map[string]string{`{"scale": 5}`: "above 4", `{"scale": 2.5}`: "whole number", `{"scale": "2"}`: "no number",
		`{"mode": "slow"}`: "none of", `{"face": 1}`: "true or false", `{"seed": 1}`: `no param "seed" (it takes: face, mode, scale)`} {
		var p map[string]any
		json.Unmarshal([]byte(in), &p)
		if _, err := CheckParams(m, p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", in, err, want)
		}
	}
}

// A machine's own runtime (runtimes.d) and model (models.d), no Go: the worker script gets the request with the
// model's defaults, its params and the model's folder, and its result becomes the job's file.
func TestLocalGenericRuntime(t *testing.T) {
	m, b, out := localRuntime(t, "echo-test", "image", "txt", `import json, os, sys
ev = os.fdopen(3, "w", buffering=1)
ev.write(json.dumps({"type": "ready"}) + "\n")
for line in sys.stdin:
    req = json.loads(line)
    open(req["output"], "w").write(json.dumps(req))
    ev.write(json.dumps({"type": "result", "file": req["output"], "seconds": 0.1}) + "\n")
`, `{"id": "echo-1", "name": "Echo", "kind": "image", "backend": "echo-test",
	  "about": "a test", "licence": "MIT", "size": 1, "tasks": ["upscale"],
	  "defaults": {"scale": 2, "steps": 4},
	  "params": {"tile": {"type": "integer", "default": 256, "minimum": 64}},
	  "runtimeArgs": {"workerModel": "echo"}}`)
	if m.Params["tile"].Default != 256.0 {
		t.Fatalf("%+v", m)
	}
	var files []string
	emit := func(e Event) {
		if e.Type == "file" {
			files = append(files, e.File)
		}
	}
	if err := b.Run(context.Background(), m, Request{Model: "echo-1", Prompt: "x", Steps: 8}, out, emit); err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !strings.HasSuffix(files[0], "-echo-1.txt") {
		t.Fatalf("files %v", files)
	}
	var req map[string]any
	raw, _ := os.ReadFile(files[0])
	json.Unmarshal(raw, &req)
	p, _ := req["params"].(map[string]any)
	if req["model"] != "echo" || req["prompt"] != "x" || req["steps"] != 8.0 || req["scale"] != 2.0 || p["tile"] != 256.0 {
		t.Errorf("the worker got %s", raw)
	}
	if b.Loaded() != "echo-1" {
		t.Errorf("loaded %q", b.Loaded())
	}
	// a wrong param fails before the worker is asked
	if err := b.Run(context.Background(), m, Request{Model: "echo-1", Params: map[string]any{"tile": 8.0}}, out, emit); err == nil || !strings.Contains(err.Error(), "below 64") {
		t.Errorf("tile 8: %v", err)
	}
}

// A runtime whose output is "json" (detect): the file it writes is also the job's result.
func TestLocalJSONRuntime(t *testing.T) {
	m, b, out := localRuntime(t, "boxes-test", "detect", "json", `import json, os, sys
ev = os.fdopen(3, "w", buffering=1)
ev.write(json.dumps({"type": "ready"}) + "\n")
for line in sys.stdin:
    req = json.loads(line)
    res = {"images": [{"image": 0, "detections": [{"text": t, "box": [1, 2, 3, 4], "score": 0.9} for t in req["texts"]]}]}
    open(req["output"], "w").write(json.dumps(res))
    ev.write(json.dumps({"type": "result", "file": req["output"], "seconds": 0.1}) + "\n")
`, `{"id": "boxes-1", "name": "Boxes", "kind": "detect", "backend": "boxes-test",
	  "about": "a test", "licence": "MIT", "size": 1, "runtimeArgs": {"workerModel": "boxes"}}`)
	var files []string
	var result map[string]any
	emit := func(e Event) {
		switch e.Type {
		case "file":
			files = append(files, e.File)
		case "result":
			result = e.Data
		}
	}
	if err := b.Run(context.Background(), m, Request{Model: "boxes-1", Texts: []string{"ball", "player"}}, out, emit); err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !strings.HasSuffix(files[0], "-boxes-1.json") {
		t.Fatalf("files %v", files)
	}
	imgs, _ := result["images"].([]any)
	if len(imgs) != 1 {
		t.Fatalf("result %v", result)
	}
	dets, _ := imgs[0].(map[string]any)["detections"].([]any)
	if len(dets) != 2 || dets[1].(map[string]any)["text"] != "player" {
		t.Errorf("detections %v", dets)
	}
}

// localRuntime makes a runtime of runtimes.d (its script, the system's python) and one model of models.d for it.
func localRuntime(t *testing.T, name, kind, output, script, manifest string) (*Model, *RuntimeBackend, string) {
	t.Helper()
	onPlatform(t, "darwin-arm64")
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 runs the worker")
	}
	cfg, root, out := t.TempDir(), t.TempDir(), t.TempDir()
	d := filepath.Join(cfg, "runtimes.d", name)
	os.MkdirAll(d, 0o755)
	envs := `{"python": "3.12"}`
	var all []string
	for _, p := range Platforms {
		if !strings.HasPrefix(p, "windows") { // no generic runtime on Windows (fd 3)
			all = append(all, `"`+p+`": `+envs)
		}
	}
	os.WriteFile(filepath.Join(d, "runtime.json"), []byte(`{"name": "`+name+`", "script": "worker.py", "generic": true,
	  "kinds": ["`+kind+`"], "output": "`+output+`", "envs": {`+strings.Join(all, ", ")+`}}`), 0o644)
	os.WriteFile(filepath.Join(d, "worker.py"), []byte(script), 0o644)
	if errs := AddLocalRuntimes(filepath.Join(cfg, "runtimes.d")); len(errs) > 0 {
		t.Fatal(errs)
	}
	t.Cleanup(func() {
		runtimesMu.Lock()
		delete(runtimes(), name)
		runtimesMu.Unlock()
	})
	if !RuntimeRuns(name) || !strings.Contains(strings.Join(GenericRuntimes(), ","), name) {
		t.Fatal("the local runtime does not run here")
	}
	// its environment: python is the system's (no packages to install)
	env := runtimeEnv(name)
	os.MkdirAll(filepath.Dir(env.Bin(root, "python")), 0o755)
	os.WriteFile(filepath.Join(env.Dir(root), ".mlxlab-ready"), nil, 0o644)
	os.Symlink(py, env.Bin(root, "python"))

	store := &models.Store{Dir: filepath.Join(root, "models")}
	md := filepath.Join(cfg, "models.d")
	os.MkdirAll(md, 0o755)
	var id struct{ ID string }
	json.Unmarshal([]byte(manifest), &id)
	os.WriteFile(filepath.Join(md, id.ID+".json"), []byte(manifest), 0o644)
	c := &Catalog{}
	if errs := c.AddLocal(md, store); len(errs) > 0 {
		t.Fatal(errs)
	}
	m := c.Find(id.ID)
	if m == nil || !ModelRuns(m) {
		t.Fatalf("%+v", m)
	}
	b := &RuntimeBackend{Root: root, Store: store, Runtime: name}
	t.Cleanup(b.Unload)
	return m, b, out
}
