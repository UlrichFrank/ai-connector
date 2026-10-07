package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Runtime is a local OpenAI-compatible server. URL is its API base, ending in /v1.
type Runtime struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Key  string `json:"key,omitempty"`
}

// servedModel is a model one runtime serves, under the runtime's own name for it.
type servedModel struct {
	*Runtime
	Model string
}

// KnownRuntimes are looked for on this machine when nothing else is configured.
var KnownRuntimes = []Runtime{
	{Name: "ollama", URL: "http://127.0.0.1:11434/v1"},
	{Name: "lmstudio", URL: "http://127.0.0.1:1234/v1"},
	{Name: "llamacpp", URL: "http://127.0.0.1:8080/v1"},
	{Name: "vllm", URL: "http://127.0.0.1:8000/v1"},
}

// ParseRuntimes reads $MLXLAB_RUNTIMES: "" = the known ones on this machine, "none" = no runtimes, else a comma
// list of name=URL (an API base, /v1 is added when missing; a key after a #: name=URL#key) or names of known ones.
func ParseRuntimes(spec string) ([]Runtime, error) {
	spec = strings.TrimSpace(spec)
	switch spec {
	case "":
		return append([]Runtime(nil), KnownRuntimes...), nil
	case "none", "off":
		return nil, nil
	}
	var out []Runtime
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, u, ok := strings.Cut(part, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		if !ok {
			found := false
			for _, k := range KnownRuntimes {
				if k.Name == name {
					out, found = append(out, k), true
				}
			}
			if !found {
				return nil, fmt.Errorf("MLXLAB_RUNTIMES: unknown runtime %q (known: ollama, lmstudio, llamacpp, vllm; or name=URL)", name)
			}
			continue
		}
		u, key, _ := strings.Cut(strings.TrimSpace(u), "#")
		pu, err := url.Parse(u)
		if err != nil || pu.Scheme == "" || pu.Host == "" || name == "" {
			return nil, fmt.Errorf("MLXLAB_RUNTIMES: %q is not name=http://host:port[/v1]", part)
		}
		u = strings.TrimRight(u, "/")
		if !strings.HasSuffix(u, "/v1") {
			u += "/v1"
		}
		out = append(out, Runtime{Name: name, URL: u, Key: key})
	}
	return out, nil
}

// discover asks every runtime for its models (in parallel; a runtime that does not answer quickly has none).
func discover(ctx context.Context, c *http.Client, rts []Runtime) []*Model {
	var mu sync.Mutex
	var out []*Model
	var wg sync.WaitGroup
	for i := range rts {
		rt := &rts[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			ms := probeRuntime(ctx, c, rt)
			mu.Lock()
			out = append(out, ms...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == Chat
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// runtimeInfo is what a runtime says about one model beyond its id.
type runtimeInfo struct {
	kind Kind
	size int64
}

// probeRuntime lists one runtime's models: their ids from /v1/models, whether each chats or embeds from the
// runtime's own API when it has one (LM Studio /api/v0/models, Ollama /api/tags and /api/show), else from the name.
func probeRuntime(ctx context.Context, c *http.Client, rt *Runtime) []*Model {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if getJSON(ctx, c, rt, rt.URL+"/models", &list) != nil {
		return nil
	}
	root := strings.TrimSuffix(rt.URL, "/v1")
	info := lmStudioInfo(ctx, c, rt, root)
	if info == nil {
		info = ollamaInfo(ctx, c, rt, root)
	}
	var out []*Model
	for _, d := range list.Data {
		if d.ID == "" {
			continue
		}
		in, ok := info[d.ID]
		if !ok {
			in.kind = Chat
			if strings.Contains(strings.ToLower(d.ID), "embed") {
				in.kind = Embed
			}
		}
		if in.kind != Chat && in.kind != Embed {
			continue
		}
		about := fmt.Sprintf("%s, served by %s (%s)", d.ID, rt.Name, root)
		out = append(out, &Model{ID: strings.ToLower(rt.Name + "/" + d.ID), Name: d.ID + " · " + rt.Name, Kind: in.kind,
			Backend: BackendOpenAI, About: about, Size: in.size, Here: true, Where: rt.Name + " (" + root + ")",
			served: &servedModel{Runtime: rt, Model: d.ID}})
	}
	return out
}

// lmStudioInfo: LM Studio's own model list types its models (llm, vlm, embeddings); nil when it is not LM Studio.
func lmStudioInfo(ctx context.Context, c *http.Client, rt *Runtime, root string) map[string]runtimeInfo {
	var l struct {
		Data []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"data"`
	}
	if getJSON(ctx, c, rt, root+"/api/v0/models", &l) != nil || len(l.Data) == 0 {
		return nil
	}
	out := map[string]runtimeInfo{}
	for _, d := range l.Data {
		k := Chat
		if d.Type == "embeddings" {
			k = Embed
		}
		out[d.ID] = runtimeInfo{kind: k}
	}
	return out
}

// ollamaInfo: Ollama names each model's capabilities (/api/show) and its size (/api/tags); nil when it is not Ollama.
func ollamaInfo(ctx context.Context, c *http.Client, rt *Runtime, root string) map[string]runtimeInfo {
	var tags struct {
		Models []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"models"`
	}
	if getJSON(ctx, c, rt, root+"/api/tags", &tags) != nil || tags.Models == nil {
		return nil
	}
	out := map[string]runtimeInfo{}
	for _, t := range tags.Models {
		in := runtimeInfo{kind: Chat, size: t.Size}
		var show struct {
			Capabilities []string `json:"capabilities"`
		}
		if postJSON(ctx, c, rt, root+"/api/show", map[string]string{"model": t.Name}, &show) == nil && len(show.Capabilities) > 0 {
			in.kind = ""
			for _, cp := range show.Capabilities {
				switch cp {
				case "completion":
					in.kind = Chat
				case "embedding":
					if in.kind == "" {
						in.kind = Embed
					}
				}
			}
		} else if strings.Contains(strings.ToLower(t.Name), "embed") {
			in.kind = Embed
		}
		out[t.Name] = in
	}
	return out
}

func getJSON(ctx context.Context, c *http.Client, rt *Runtime, u string, v any) error {
	return doJSON(ctx, c, rt, http.MethodGet, u, nil, v)
}

func postJSON(ctx context.Context, c *http.Client, rt *Runtime, u string, body, v any) error {
	return doJSON(ctx, c, rt, http.MethodPost, u, body, v)
}

func doJSON(ctx context.Context, c *http.Client, rt *Runtime, method, u string, body, v any) error {
	var rd *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	var req *http.Request
	var err error
	if rd != nil {
		req, err = http.NewRequestWithContext(ctx, method, u, rd)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, u, nil)
	}
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if rt.Key != "" {
		req.Header.Set("Authorization", "Bearer "+rt.Key)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
