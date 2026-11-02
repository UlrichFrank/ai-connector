package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
)

// The person's setup (docs/api.md "Setting up models"), for a device or a client token — what the token acts for
// (userOf): their workers' models, the downloads on them.
//
//	GET    /api/my/setup[?access=1]           the workers, the models on them and to download, the downloads
//	POST   /api/my/downloads {model, worker?}  starts one (the worker: its id or name; else the best place)
//	GET    /api/my/downloads[?model=&worker=]  all downloads, or one
//	DELETE /api/my/downloads?model=[&worker=]  stops one

type idName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type mySetupWorker struct {
	idName
	Memory      int64           `json:"memory"`
	Free        int64           `json:"free"`
	HuggingFace map[string]bool `json:"huggingFace"`
}

type mySetupModel struct {
	setupModel
	Worker idName `json:"worker"`
}

type myDownload struct {
	downloadState
	Worker idName `json:"worker"`
}

// userNodes are the person's connected workers, the longest connected first.
func (s *Server) userNodes(ctx context.Context, user string) []*node {
	routed, _ := s.fleet.pick(ctx)
	var out []*node
	for _, n := range routed {
		if s.owns(user, n.id) {
			out = append(out, n)
		}
	}
	return out
}

// myUser is the person the request acts for; false (answered): there is none.
func myUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	if u := userOf(r); u != "" {
		return u, true
	}
	msg := "models are set up on your own workers: sign in with your user name"
	if viaOf(r) == viaClient {
		msg = "models are set up on the workers of the client token's owner, and this token has none: make a new one in the console"
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": msg})
	return "", false
}

// mySetup: GET /api/my/setup — at most one model recommended per role (setupRole), on the worker with the most
// memory.
func (s *Server) mySetup(w http.ResponseWriter, r *http.Request) {
	user, ok := myUser(w, r)
	if !ok {
		return
	}
	nodes := s.userNodes(r.Context(), user)
	setups := s.workerSetups(r.Context(), nodes, r.URL.Query().Get("access") != "")
	out := struct {
		User      string          `json:"user"`
		SetupURL  string          `json:"setupUrl,omitempty"`
		Workers   []mySetupWorker `json:"workers"`
		Models    []mySetupModel  `json:"models"`
		Downloads []myDownload    `json:"downloads"`
	}{User: user, SetupURL: s.setupURL(), Workers: []mySetupWorker{}, Models: []mySetupModel{}, Downloads: []myDownload{}}
	best := map[string]int{}      // role → the model recommended
	bestMem := map[string]int64{} // role → its worker's memory
	for i, ws := range setups {
		if ws == nil { // an older worker: it downloads nothing on request
			continue
		}
		n := nodes[i]
		wk := idName{n.id, n.name}
		out.Workers = append(out.Workers, mySetupWorker{wk, ws.Memory, ws.Free, map[string]bool{"token": ws.HuggingFace.Token}})
		for _, m := range ws.Models {
			if role := setupRole(m.Kind, m.Tasks); m.Recommended {
				m.Recommended = false
				if j, ok := best[role]; !ok || ws.Memory > bestMem[role] {
					if ok {
						out.Models[j].Recommended = false
					}
					best[role], bestMem[role] = len(out.Models), ws.Memory
					m.Recommended = true
				}
			}
			out.Models = append(out.Models, mySetupModel{m, wk})
		}
		for _, d := range ws.Downloads {
			out.Downloads = append(out.Downloads, myDownload{d, wk})
		}
		s.offerDone(r.Context(), n, ws.Downloads)
	}
	writeJSON(w, http.StatusOK, out)
}

// offerDone asks a worker for its models again when a download of it is done that the fleet does not know as here
// yet: the model is offered then (GET /api/capabilities).
func (s *Server) offerDone(ctx context.Context, n *node, ds []downloadState) {
	for _, d := range ds {
		if d.State == "done" && !nodeHas(n, d.Model) {
			s.refreshNode(ctx, n)
			return
		}
	}
}

// startMyDownload: POST /api/my/downloads.
func (s *Server) startMyDownload(w http.ResponseWriter, r *http.Request) {
	user, ok := myUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Model  string `json:"model"`
		Worker string `json:"worker"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil || body.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": `{"model", "worker"?} expected`})
		return
	}
	n, err := s.downloadNode(r.Context(), s.userNodes(r.Context(), user), user, body.Model, body.Worker)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no-worker", "message": err.Error()})
		return
	}
	b, _ := json.Marshal(map[string]string{"model": body.Model})
	st, err := s.downloadCall(r.Context(), n, http.MethodPost, "/api/downloads", b)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "worker", "message": n.name + ": " + err.Error()})
		return
	}
	s.offerDone(r.Context(), n, []downloadState{st})
	writeJSON(w, http.StatusOK, myDownload{st, idName{n.id, n.name}})
}

// myDownloads: GET /api/my/downloads — every download on the person's workers, or one model's (?model=, on
// ?worker= or the first that has one of it; 404: none).
func (s *Server) myDownloads(w http.ResponseWriter, r *http.Request) {
	user, ok := myUser(w, r)
	if !ok {
		return
	}
	model, worker := r.URL.Query().Get("model"), r.URL.Query().Get("worker")
	nodes := s.userNodes(r.Context(), user)
	all := []myDownload{}
	for i, ws := range s.workerSetups(r.Context(), nodes, false) {
		if ws == nil || worker != "" && nodes[i].id != worker {
			continue
		}
		s.offerDone(r.Context(), nodes[i], ws.Downloads)
		for _, d := range ws.Downloads {
			if model == "" || d.Model == model {
				all = append(all, myDownload{d, idName{nodes[i].id, nodes[i].name}})
			}
		}
	}
	if model == "" {
		writeJSON(w, http.StatusOK, all)
		return
	}
	if len(all) == 0 {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, all[0])
}

// cancelMyDownload: DELETE /api/my/downloads?model=[&worker=] — on the worker named, else wherever it runs.
func (s *Server) cancelMyDownload(w http.ResponseWriter, r *http.Request) {
	user, ok := myUser(w, r)
	if !ok {
		return
	}
	model, worker := r.URL.Query().Get("model"), r.URL.Query().Get("worker")
	if model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": "?model= expected"})
		return
	}
	nodes := s.userNodes(r.Context(), user)
	stopped := false
	for i, ws := range s.workerSetups(r.Context(), nodes, false) {
		if ws == nil || worker != "" && nodes[i].id != worker {
			continue
		}
		for _, d := range ws.Downloads {
			if d.Model != model || d.State == "done" || d.State == "failed" || d.State == "canceled" {
				continue
			}
			a := s.send(r.Context(), nodes[i], http.MethodDelete, "/api/downloads?model="+url.QueryEscape(model), nil, "")
			if !a.ok() {
				a.write(w)
				return
			}
			stopped = true
		}
	}
	if !stopped {
		notFound(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
