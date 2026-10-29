package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
)

// ComfyBackend runs ComfyUI's workflows as models (runtimes/comfyui.json; docs/models.md, "ComfyUI"): a model is a
// workflow in API format, where the request's fields go in it, and its files. The worker starts ComfyUI (its pinned
// source, its environment) on a free port without custom nodes, with the model's folders of the store as its model
// paths (an extra_model_paths.yaml; nothing is written into ComfyUI's folder), and outputs in a folder of its own;
// fills the workflow, queues it (/prompt), waits for its result (/history), fetches the picture or video (/view) and
// deletes it and the prompt's history from ComfyUI.
type ComfyBackend struct {
	srv   *ServerBackend
	paths map[string]map[string]bool // the model folders of the running ComfyUI: folder → directories
	last  string                     // the model of the last workflow (another one: ComfyUI frees its models first)
}

// NewComfyBackend is the backend of the ComfyUI runtime name.
func NewComfyBackend(root string, store *models.Store, name string) *ComfyBackend {
	return &ComfyBackend{srv: &ServerBackend{Root: root, Store: store, Runtime: name}}
}

func (b *ComfyBackend) Name() string   { return b.srv.Runtime }
func (b *ComfyBackend) Heavy() bool    { return true }
func (b *ComfyBackend) Loaded() string { return b.srv.Loaded() }
func (b *ComfyBackend) Unload()        { b.srv.Unload(); b.paths, b.last = nil, "" }

func (b *ComfyBackend) Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	s := b.srv
	s.mu.Lock()
	defer s.mu.Unlock()
	rt := runtimeManifest(s.Runtime)
	dir, err := s.files(ctx, m, emit)
	if err != nil {
		return err
	}
	need := modelPaths(dir, m.folders)
	if !s.running() || !covers(b.paths, need) {
		// a model of folders the running ComfyUI does not know: started again with them (it reads its paths at start)
		paths := union(b.paths, need)
		s.stop()
		args, err := b.startArgs(paths)
		if err != nil {
			return err
		}
		s.ExtraArgs = args
		if err := s.start(ctx, rt, dir, dir, emit); err != nil {
			return err
		}
		s.model = s.Runtime // one ComfyUI for all its models: it loads what a workflow names
		b.paths, b.last = paths, ""
	}
	if b.last != "" && b.last != m.ID {
		b.post(ctx, "/free", map[string]any{"unload_models": true, "free_memory": true}) // only one model's weights in memory
	}
	b.last = m.ID
	wf, seed, err := fillWorkflow(m, r)
	if err != nil {
		return err
	}
	s.state.set(m.ID)
	emit(Event{Type: "stats", Data: map[string]any{"seed": seed}})
	t0 := time.Now()
	id, err := b.queue(ctx, wf)
	if err != nil {
		return err
	}
	emit(Event{Type: "status", Text: "ComfyUI runs the workflow …"})
	out, err := b.wait(ctx, id)
	if err != nil {
		return err
	}
	file, err := b.fetch(ctx, out, outDir, m.ID)
	// ComfyUI keeps nothing of the job: its copy of the result, the prompt in its history
	os.Remove(filepath.Join(b.outputDir(), filepath.FromSlash(out.Subfolder), filepath.Base(out.Filename)))
	b.post(ctx, "/history", map[string]any{"delete": []string{id}})
	if err != nil {
		return err
	}
	emit(Event{Type: "file", File: file})
	emit(Event{Type: "stats", Data: map[string]any{"seconds": time.Since(t0).Seconds()}})
	return nil
}

// modelPaths are the folders ComfyUI must search for the model's files: folder → the directories they lie in.
func modelPaths(dir string, folders map[string][]string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for folder, files := range folders {
		for _, f := range files {
			if out[folder] == nil {
				out[folder] = map[string]bool{}
			}
			out[folder][filepath.Dir(filepath.Join(dir, filepath.FromSlash(f)))] = true
		}
	}
	return out
}

func covers(have, need map[string]map[string]bool) bool {
	for folder, dirs := range need {
		for d := range dirs {
			if !have[folder][d] {
				return false
			}
		}
	}
	return true
}

func union(a, b map[string]map[string]bool) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, m := range []map[string]map[string]bool{a, b} {
		for folder, dirs := range m {
			if out[folder] == nil {
				out[folder] = map[string]bool{}
			}
			for d := range dirs {
				out[folder][d] = true
			}
		}
	}
	return out
}

// runDir is ComfyUI's own folder of this worker (<config dir>/run/<runtime>): its model paths, outputs, inputs, temp.
func (b *ComfyBackend) runDir() string    { return filepath.Join(b.srv.Root, "run", b.srv.Runtime) }
func (b *ComfyBackend) outputDir() string { return filepath.Join(b.runDir(), "output") }

// startArgs writes the model paths and gives ComfyUI's arguments: no custom nodes, those paths, its own folders.
func (b *ComfyBackend) startArgs(paths map[string]map[string]bool) ([]string, error) {
	run := b.runDir()
	os.RemoveAll(run) // the last start's outputs and temp files
	for _, d := range []string{"output", "input", "temp"} {
		if err := os.MkdirAll(filepath.Join(run, d), 0o700); err != nil {
			return nil, err
		}
	}
	var y strings.Builder
	y.WriteString("ai-connector:\n")
	var folders []string
	for f := range paths {
		folders = append(folders, f)
	}
	sort.Strings(folders)
	for _, f := range folders {
		y.WriteString("  " + f + ": |\n")
		var dirs []string
		for d := range paths[f] {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		for _, d := range dirs {
			y.WriteString("    " + d + "\n")
		}
	}
	cfg := filepath.Join(run, "extra_model_paths.yaml")
	if err := os.WriteFile(cfg, []byte(y.String()), 0o600); err != nil {
		return nil, err
	}
	return []string{"--disable-all-custom-nodes", "--extra-model-paths-config", cfg, "--output-directory", filepath.Join(run, "output"),
		"--input-directory", filepath.Join(run, "input"), "--temp-directory", filepath.Join(run, "temp")}, nil
}

// post sends a small request to ComfyUI and forgets the answer (freeing memory, deleting history).
func (b *ComfyBackend) post(ctx context.Context, path string, body any) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.srv.base+path, bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := localHTTP.Do(req); err == nil {
		resp.Body.Close()
	}
}

// fillWorkflow puts the request's fields (over the model's defaults) into the workflow where its inputs say; a
// seed it lacks is drawn.
func fillWorkflow(m *Model, r Request) (map[string]any, int64, error) {
	var wf map[string]any
	if err := json.Unmarshal(m.workflow, &wf); err != nil {
		return nil, 0, err
	}
	req, err := mergedRequest(m.Defaults, r)
	if err != nil {
		return nil, 0, err
	}
	seed := r.Seed
	if seed == 0 {
		seed = rand.Int64N(1 << 50)
	}
	req["seed"] = seed
	for field, path := range m.inputs {
		v, ok := req[field]
		if !ok {
			continue
		}
		if err := setPath(wf, strings.Split(path, "."), v); err != nil {
			return nil, 0, fmt.Errorf("input %s (%s): %w", field, path, err)
		}
	}
	if p, _ := req["prompt"].(string); p == "" && m.inputs["prompt"] != "" {
		return nil, 0, fmt.Errorf("no prompt")
	}
	return wf, seed, nil
}

func setPath(node map[string]any, path []string, v any) error {
	for i, k := range path {
		if i == len(path)-1 {
			node[k] = v
			return nil
		}
		next, ok := node[k].(map[string]any)
		if !ok {
			return fmt.Errorf("no %s", k)
		}
		node = next
	}
	return nil
}

func (b *ComfyBackend) queue(ctx context.Context, wf map[string]any) (string, error) {
	body, _ := json.Marshal(map[string]any{"prompt": wf, "client_id": randomKey()})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.srv.base+"/prompt", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := localHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		ID         string          `json:"prompt_id"`
		Error      json.RawMessage `json:"error"`
		NodeErrors json.RawMessage `json:"node_errors"`
	}
	json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusOK || out.ID == "" {
		return "", fmt.Errorf("ComfyUI refused the workflow: %s", strings.TrimSpace(string(raw)))
	}
	return out.ID, nil
}

// comfyOutput is one file a workflow's node wrote.
type comfyOutput struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// wait polls the prompt's history until it is done; canceled, it interrupts ComfyUI.
func (b *ComfyBackend) wait(ctx context.Context, id string) (comfyOutput, error) {
	for {
		select {
		case <-ctx.Done():
			stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			b.post(stop, "/interrupt", map[string]any{})
			cancel()
			return comfyOutput{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		if !b.srv.running() {
			return comfyOutput{}, fmt.Errorf("ComfyUI stopped:\n%s", b.srv.lastLogs())
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.srv.base+"/history/"+url.PathEscape(id), nil)
		resp, err := localHTTP.Do(req)
		if err != nil {
			continue
		}
		var h map[string]struct {
			Status struct {
				Str      string            `json:"status_str"`
				Messages []json.RawMessage `json:"messages"`
			} `json:"status"`
			Outputs map[string]map[string]json.RawMessage `json:"outputs"`
		}
		json.NewDecoder(resp.Body).Decode(&h)
		resp.Body.Close()
		e, ok := h[id]
		if !ok {
			continue
		}
		if e.Status.Str == "error" {
			msgs := make([]string, len(e.Status.Messages))
			for i, m := range e.Status.Messages {
				msgs[i] = string(m)
			}
			return comfyOutput{}, fmt.Errorf("the workflow failed: %s", strings.Join(msgs, "; "))
		}
		for _, node := range e.Outputs {
			for _, key := range []string{"images", "gifs", "videos", "audio"} {
				var files []comfyOutput
				if json.Unmarshal(node[key], &files) == nil && len(files) > 0 && files[0].Type == "output" {
					return files[0], nil
				}
			}
		}
		if e.Status.Str == "success" {
			return comfyOutput{}, fmt.Errorf("the workflow wrote no file")
		}
	}
}

func (b *ComfyBackend) fetch(ctx context.Context, o comfyOutput, outDir, id string) (string, error) {
	q := url.Values{"filename": {o.Filename}, "subfolder": {o.Subfolder}, "type": {o.Type}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.srv.base+"/view?"+q.Encode(), nil)
	resp, err := http.DefaultClient.Do(req) // a video may take longer than localHTTP's timeout; ctx ends it
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ComfyUI /view: %s", resp.Status)
	}
	out := filepath.Join(outDir, fmt.Sprintf("%s-%s%s", time.Now().Format("20060102-150405"), id, filepath.Ext(o.Filename)))
	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return "", err
	}
	return out, f.Close()
}
