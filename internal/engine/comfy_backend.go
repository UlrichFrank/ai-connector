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
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
)

// ComfyBackend runs ComfyUI's workflows as models (runtimes/comfyui.json; docs/models.md, "ComfyUI"): a model is a
// workflow in API format, where the request's fields go in it, and its files. The worker starts ComfyUI (its pinned
// source, its environment) on a free port, links the model's files from the store into ComfyUI's models folders, fills
// the workflow, queues it (/prompt), waits for its result (/history) and fetches the picture or video (/view).
type ComfyBackend struct {
	srv *ServerBackend
}

// NewComfyBackend is the backend of the ComfyUI runtime name.
func NewComfyBackend(root string, store *models.Store, name string) *ComfyBackend {
	return &ComfyBackend{srv: &ServerBackend{Root: root, Store: store, Runtime: name}}
}

func (b *ComfyBackend) Name() string   { return b.srv.Runtime }
func (b *ComfyBackend) Heavy() bool    { return true }
func (b *ComfyBackend) Loaded() string { return b.srv.Loaded() }
func (b *ComfyBackend) Unload()        { b.srv.Unload() }

func (b *ComfyBackend) Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	s := b.srv
	s.mu.Lock()
	defer s.mu.Unlock()
	rt := runtimeManifest(s.Runtime)
	dir, err := s.files(ctx, m, emit)
	if err != nil {
		return err
	}
	if !s.running() {
		s.stop()
		if err := s.start(ctx, rt, dir, dir, emit); err != nil {
			return err
		}
		s.model = s.Runtime // one ComfyUI for all its models: it loads what a workflow names
	}
	if err := linkModelFiles(dir, filepath.Join(s.app, "models"), m.folders); err != nil {
		return err
	}
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
	if err != nil {
		return err
	}
	emit(Event{Type: "file", File: file})
	emit(Event{Type: "stats", Data: map[string]any{"seconds": time.Since(t0).Seconds()}})
	return nil
}

// linkModelFiles links the model's files (in its folder of the store) into ComfyUI's models folders by their names.
func linkModelFiles(dir, modelsDir string, folders map[string][]string) error {
	for folder, files := range folders {
		for _, f := range files {
			src := filepath.Join(dir, filepath.FromSlash(f))
			if _, err := os.Stat(src); err != nil {
				return fmt.Errorf("the model has no file %s: %w", f, err)
			}
			dst := filepath.Join(modelsDir, folder, filepath.Base(f))
			if cur, err := os.Readlink(dst); err == nil && cur == src {
				continue
			}
			os.Remove(dst)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(src, dst); err != nil {
				return err
			}
		}
	}
	return nil
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
	resp, err := http.DefaultClient.Do(req)
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
			req, _ := http.NewRequest(http.MethodPost, b.srv.base+"/interrupt", nil)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
			return comfyOutput{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		if !b.srv.running() {
			return comfyOutput{}, fmt.Errorf("ComfyUI stopped:\n%s", b.srv.lastLogs())
		}
		resp, err := http.Get(b.srv.base + "/history/" + url.PathEscape(id))
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
	resp, err := http.DefaultClient.Do(req)
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
