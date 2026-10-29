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

const comfyTestWorkflow = `{"3": {"class_type": "KSampler", "inputs": {"seed": 0, "steps": 20, "cfg": 7}},
  "5": {"class_type": "EmptyLatentImage", "inputs": {"width": 512, "height": 512}},
  "6": {"class_type": "CLIPTextEncode", "inputs": {"text": ""}}}`

func TestFillWorkflow(t *testing.T) {
	m := &Model{ID: "sd", workflow: json.RawMessage(comfyTestWorkflow), Defaults: Request{Steps: 20, Width: 512},
		inputs: map[string]string{"prompt": "6.inputs.text", "seed": "3.inputs.seed", "steps": "3.inputs.steps", "width": "5.inputs.width"}}
	wf, seed, err := fillWorkflow(m, Request{Prompt: "a fox", Steps: 8})
	if err != nil {
		t.Fatal(err)
	}
	in := func(node string) map[string]any { return wf[node].(map[string]any)["inputs"].(map[string]any) }
	if in("6")["text"] != "a fox" || in("3")["steps"] != 8.0 || in("5")["width"] != 512.0 || seed == 0 || in("3")["seed"] != seed {
		t.Errorf("%v (seed %d)", wf, seed)
	}
	if _, _, err := fillWorkflow(m, Request{}); err == nil || !strings.Contains(err.Error(), "no prompt") {
		t.Errorf("no prompt: %v", err)
	}
	m.inputs["height"] = "9.inputs.height"
	if _, _, err := fillWorkflow(m, Request{Prompt: "x", Height: 64}); err == nil {
		t.Error("an input into a node the workflow lacks")
	}
}

// ComfyUI as a runtime: the worker starts it (its app with the environment's python), links the model's files into
// its models folder, queues the filled workflow and fetches the picture its history names.
func TestComfyBackend(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 plays ComfyUI")
	}
	root := t.TempDir()
	rt := &RuntimeManifest{Name: "comfy-test", Kinds: []Kind{Image},
		Server: &ServerSpec{Protocol: "comfyui", Start: []string{"python", "{app}", "--port", "{port}"}, App: "main.py", Health: "/system_stats"},
		Tools:  map[string][]ToolArchive{Platform(): {{URL: "https://example.invalid/c.tar.gz", SHA256: strings.Repeat("0", 64)}}},
		Envs:   map[string]RuntimeEnv{Platform(): {Python: "3.12"}}}
	runtimesMu.Lock()
	runtimes()[rt.Name] = rt
	runtimesMu.Unlock()
	t.Cleanup(func() {
		runtimesMu.Lock()
		delete(runtimes(), rt.Name)
		runtimesMu.Unlock()
	})
	if !RuntimeRuns(rt.Name) {
		t.Fatal("does not run here")
	}
	// the environment: the system's python; the tools: a fake ComfyUI that records the workflow and draws nothing
	env := runtimeEnv(rt.Name)
	os.MkdirAll(filepath.Dir(env.Bin(root, "python")), 0o755)
	os.WriteFile(filepath.Join(env.Dir(root), ".mlxlab-ready"), nil, 0o644)
	os.Symlink(py, env.Bin(root, "python"))
	tools, _, _ := toolDir(root, rt.Name)
	app := filepath.Join(tools, "ComfyUI-1.0")
	os.MkdirAll(filepath.Join(app, "app"), 0o755)
	os.WriteFile(filepath.Join(tools, ".mlxlab-ready"), nil, 0o644)
	os.WriteFile(filepath.Join(app, "app", "main.py"), []byte("raise SystemExit('not this one')"), 0o644)
	os.WriteFile(filepath.Join(app, "main.py"), []byte(`import http.server, json, os, sys, urllib.parse
port = int(sys.argv[sys.argv.index("--port") + 1])
queued = {}
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, body, ctype="application/json"):
        self.send_response(200); self.send_header("Content-Type", ctype); self.end_headers(); self.wfile.write(body)
    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        if u.path == "/system_stats":
            return self.send(b"{}")
        if u.path.startswith("/history/"):
            pid = u.path.rsplit("/", 1)[1]
            out = {"9": {"images": [{"filename": "aic_00001_.png", "subfolder": "", "type": "output"}]}}
            return self.send(json.dumps({pid: {"status": {"status_str": "success"}, "outputs": out}}).encode())
        if u.path == "/view":
            q = urllib.parse.parse_qs(u.query)
            linked = os.path.islink(os.path.join("models", "checkpoints", "sd.safetensors"))
            return self.send(json.dumps({"file": q["filename"][0], "linked": linked, "workflow": queued.get("wf")}).encode(), "image/png")
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        queued["wf"] = body["prompt"]
        self.send(json.dumps({"prompt_id": "p1"}).encode())
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
`), 0o644)

	store := &models.Store{Dir: filepath.Join(root, "models")}
	m := &Model{ID: "sd-test", Name: "SD", Kind: Image, Backend: rt.Name, repo: "org/sd", rev: strings.Repeat("b", 40),
		files: []string{"sd.safetensors"}, workflow: json.RawMessage(comfyTestWorkflow),
		inputs: map[string]string{"prompt": "6.inputs.text", "seed": "3.inputs.seed"}, folders: map[string][]string{"checkpoints": {"sd.safetensors"}}}
	d := filepath.Join(store.Dir, "org", "sd", m.rev)
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "sd.safetensors"), []byte("abc"), 0o644)
	os.WriteFile(filepath.Join(d, "sd.safetensors.sha256"), []byte("s"), 0o644)
	os.WriteFile(filepath.Join(d, ".files.json"), []byte(`[{"path":"sd.safetensors","size":3,"sha256":"s"}]`), 0o644)

	b := NewComfyBackend(root, store, rt.Name)
	t.Cleanup(b.Unload)
	var files []string
	out := t.TempDir()
	err = b.Run(context.Background(), m, Request{Prompt: "a fox", Seed: 5}, out, func(e Event) {
		if e.Type == "file" {
			files = append(files, e.File)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !strings.HasSuffix(files[0], "-sd-test.png") {
		t.Fatalf("files %v", files)
	}
	var got struct {
		Linked   bool                      `json:"linked"`
		Workflow map[string]map[string]any `json:"workflow"`
	}
	raw, _ := os.ReadFile(files[0])
	json.Unmarshal(raw, &got)
	in := func(node string) map[string]any { return got.Workflow[node]["inputs"].(map[string]any) }
	if !got.Linked || in("6")["text"] != "a fox" || in("3")["seed"] != 5.0 {
		t.Errorf("ComfyUI saw %s", raw)
	}
	if b.Loaded() != "sd-test" {
		t.Errorf("loaded %q", b.Loaded())
	}
}
