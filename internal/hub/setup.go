package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Setting up models on a person's workers (docs/api.md "Setting up models"): what each has and could download, and the
// downloads. The workers do the work (internal/web/downloads.go: GET /api/setup, /api/downloads); the hub asks
// those of the person a token acts for (the device's user, a client token's owner) and merges them — nobody fills
// another person's disk.

// setupAsk: a worker's setup or download is waited for this long.
var setupAsk = 20 * time.Second

// workerSetup is a worker's GET /api/setup.
type workerSetup struct {
	Memory      int64 `json:"memory"`
	Free        int64 `json:"free"`
	HuggingFace struct {
		Token bool `json:"token"`
	} `json:"huggingFace"`
	Models    []setupModel    `json:"models"`
	Downloads []downloadState `json:"downloads"`
}

type setupModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	About         string   `json:"about"`
	Here          bool     `json:"here"`
	Size          int64    `json:"size"`
	Licence       string   `json:"licence,omitempty"`
	NonCommercial bool     `json:"nonCommercial"`
	Gated         bool     `json:"gated"`
	Access        string   `json:"access,omitempty"`
	URL           string   `json:"url,omitempty"`
	Fits          bool     `json:"fits"`
	Recommended   bool     `json:"recommended"`
	Tasks         []string `json:"tasks,omitempty"`
	Languages     []string `json:"languages,omitempty"`
}

// downloadState is a worker's download (engine.DownloadState).
type downloadState struct {
	Model string `json:"model"`
	State string `json:"state"`
	Bytes int64  `json:"bytes"`
	Total int64  `json:"total"`
	ETA   int    `json:"eta"`
	Error string `json:"error"`
	Need  string `json:"need"`
	URL   string `json:"url"`
}

// workerSetup asks a worker for its setup (access: ask Hugging Face about the gated models too).
func (s *Server) workerSetup(ctx context.Context, n *node, access bool) (*workerSetup, error) {
	ctx, cancel := context.WithTimeout(ctx, setupAsk)
	defer cancel()
	p := "/api/setup"
	if access {
		p += "?access=1"
	}
	var ws workerSetup
	if err := s.fleet.getJSON(ctx, n, p, &ws); err != nil {
		return nil, err
	}
	return &ws, nil
}

// workerSetups asks the workers at once; a worker that does not answer (an older one) is left out.
func (s *Server) workerSetups(ctx context.Context, nodes []*node, access bool) []*workerSetup {
	out := make([]*workerSetup, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], _ = s.workerSetup(ctx, n, access)
		}()
	}
	wg.Wait()
	return out
}

// nodeHas: the worker lists the model as here.
func nodeHas(n *node, id string) bool {
	_, _, _, ms := n.snapshot()
	for _, m := range ms {
		if m.ID == id && m.Here {
			return true
		}
	}
	return false
}

// refreshNode asks a worker for its models now and tells everyone who waits for a change of the fleet.
func (s *Server) refreshNode(ctx context.Context, n *node) {
	if err := s.fleet.probe(ctx, n); err != nil {
		log.Printf("worker %q: %v", n.name, err)
	}
	s.fleet.notify()
}

// RefreshWorkers asks every worker for its models now (the standalone hub, when its worker downloaded one).
func (s *Server) RefreshWorkers() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, n := range s.fleet.list() {
		_ = s.fleet.probe(ctx, n)
	}
	s.fleet.notify()
}

// setupRole is what a model is recommended among (engine.SetupRole): its kind, or kind/task for an upscaler
// ("image/upscale"), a cutout model ("image/cutout"), the transcriber ("speech/transcribe") and the aligner ("speech/align"), which do not compete
// with the kind's generators.
func setupRole(kind string, tasks []string) string {
	for _, t := range tasks {
		if t == "upscale" || t == "cutout" || t == "transcribe" || t == "align" {
			return kind + "/" + t
		}
	}
	return kind
}

// downloadCall is a call to a worker's /api/downloads.
func (s *Server) downloadCall(ctx context.Context, n *node, method, path string, body []byte) (downloadState, error) {
	cctx, cancel := context.WithTimeout(ctx, setupAsk)
	defer cancel()
	var rd io.Reader
	ctype := ""
	if body != nil {
		rd, ctype = bytes.NewReader(body), "application/json"
	}
	res, err := s.fleet.request(cctx, n, method, path, rd, ctype)
	if err != nil {
		return downloadState{}, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(b))
		if res.StatusCode == http.StatusNotFound && strings.Contains(msg, "404 page not found") {
			msg = "this worker cannot download models on request (an older ai-connector: update it)"
		}
		return downloadState{}, fmt.Errorf("%s", msg)
	}
	var st downloadState
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	return st, nil
}

// downloadNode is the worker of nodes (one person's, who) a download of model goes to: the one named (its id or
// name), else the only one, else one that has it already, else the one where it fits with the most free disk.
func (s *Server) downloadNode(ctx context.Context, nodes []*node, who, model, worker string) (*node, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no computer of %s is connected to download %s to", who, model)
	}
	if worker != "" {
		for _, n := range nodes {
			if n.id == worker || strings.EqualFold(n.name, worker) || strings.EqualFold(trimRunes(n.name, agentChars), worker) {
				return n, nil
			}
		}
		return nil, fmt.Errorf("%q is not a connected computer of %s", worker, who)
	}
	if len(nodes) == 1 {
		return nodes[0], nil
	}
	var pick *node
	var pickFree int64 = -1
	listed := (*node)(nil)
	for i, ws := range s.workerSetups(ctx, nodes, false) {
		if ws == nil {
			continue
		}
		for _, x := range ws.Models {
			if x.ID != model {
				continue
			}
			if listed == nil {
				listed = nodes[i]
			}
			if x.Here {
				return nodes[i], nil // already there: done at once
			}
			if x.Fits && ws.Free > pickFree {
				pick, pickFree = nodes[i], ws.Free
			}
		}
	}
	switch {
	case pick != nil:
		return pick, nil
	case listed != nil:
		return listed, nil // the worker says why it cannot
	}
	return nodes[0], nil
}
