package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// Claude Code agents (docs/api.md, "Agents (Claude Code)"). Claude Code runs with the subscription of whoever is
// logged in on a worker, so everything about it stays with the worker's owner: a claude-code job goes only to a
// connected worker whose Owner is the signed-in person (the server sets the job's owner; a caller's is dropped),
// and only the owner lists, stops and attaches to the sessions of their workers.

// claudePrefix is the model id prefix of the Claude Code models (engine.BackendClaudeCode).
const claudePrefix = "claude-code/"

// isClaude: the model is a Claude Code model (its id, or what a worker resolves it to).
func isClaude(nodes []*node, model string) bool {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), claudePrefix) {
		return true
	}
	m, ok := resolve(nodes, model)
	return ok && (m.Backend == "claude-code" || strings.HasPrefix(m.ID, claudePrefix))
}

// ownerOf is the owner of a worker id ("" for none, the legacy worker, an unknown id).
func (s *Server) ownerOf(id string) string {
	for _, wk := range s.workers.list() {
		if wk.ID == id {
			return wk.Owner
		}
	}
	return ""
}

// owns: the person (a user name) owns the worker.
func (s *Server) owns(user, workerID string) bool {
	return user != "" && s.ownerOf(workerID) == user
}

// listsClaude: the worker offers Claude Code (a claude-code model).
func listsClaude(n *node) bool {
	_, _, _, models := n.snapshot()
	for _, m := range models {
		if strings.HasPrefix(m.ID, claudePrefix) {
			return true
		}
	}
	return false
}

// claudeWorker picks the worker for a claude-code job of the signed-in person: one of their connected workers that
// offers Claude Code (pin: that one). nil: the answer is written (not theirs, offline, none).
func (s *Server) claudeWorker(w http.ResponseWriter, r *http.Request, nodes []*node, pin string) *node {
	me := deviceOf(r).User
	if me == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden",
			"message": "Claude Code runs on your own worker: sign in with your user name"})
		return nil
	}
	if pin != "" {
		if !s.owns(me, pin) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "Claude Code runs only on your own workers"})
			return nil
		}
		n := s.fleet.node(pin)
		if n == nil || !n.isRouted() {
			name, _ := s.findWorker(pin)
			offlineMac(w, name)
			return nil
		}
		return n
	}
	var mine []*node
	for _, n := range nodes {
		if s.owns(me, n.id) {
			if listsClaude(n) {
				return n // the longest connected first
			}
			mine = append(mine, n)
		}
	}
	owned := 0
	for _, wk := range s.workers.list() {
		if wk.Owner == me {
			owned++
		}
	}
	switch {
	case owned == 0:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no-worker",
			"message": me + " has no worker with Claude Code: connect a computer of yours where Claude Code is installed and logged in"})
	case len(mine) == owned:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no-worker",
			"message": "None of " + me + "'s workers has Claude Code: install it there and log in with your subscription"})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "offline", "message": me + "'s Claude Code worker is offline"})
	}
	return nil
}

// submitClaude: POST /api/jobs of a claude-code model, to the person's own worker, with the owner set.
func (s *Server) submitClaude(w http.ResponseWriter, r *http.Request, nodes []*node, req map[string]json.RawMessage, pin string) {
	to := s.claudeWorker(w, r, nodes, pin)
	if to == nil {
		return
	}
	req["owner"], _ = json.Marshal(deviceOf(r).User)
	out, _ := json.Marshal(req)
	s.forwardBody(w, r, to, out)
}

// submitOffline: POST /api/jobs while no worker is connected — a claude-code job says whose worker it waits for.
func (s *Server) submitOffline(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) == nil && isClaude(nil, req.Model) {
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.claudeWorker(w, r, nil, "")
		return
	}
	offline(w)
}

// agentsAPI: /api/agents/… — the sessions of the person's own workers.
func (s *Server) agentsAPI(w http.ResponseWriter, r *http.Request, nodes []*node) {
	me := deviceOf(r).User
	p := r.URL.Path
	switch {
	case p == "/api/agents/sessions" && r.Method == http.MethodGet:
		var mine []*node
		for _, n := range nodes {
			if s.owns(me, n.id) {
				mine = append(mine, n)
			}
		}
		out := []json.RawMessage{}
		for _, b := range s.each(r.Context(), mine, "/api/agents/sessions") {
			var list []json.RawMessage
			if b != nil && json.Unmarshal(b, &list) == nil {
				out = append(out, list...)
			}
		}
		writeJSON(w, http.StatusOK, out)
	case p == "/api/agents/sessions" && r.Method == http.MethodPost:
		to := s.claudeWorker(w, r, nodes, r.URL.Query().Get("worker")) // ?worker=<id>, else one of theirs
		if to == nil {
			return
		}
		var body map[string]json.RawMessage
		b, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if len(bytes.TrimSpace(b)) > 0 && json.Unmarshal(b, &body) != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": "a JSON object"})
			return
		}
		if body == nil {
			body = map[string]json.RawMessage{}
		}
		body["owner"], _ = json.Marshal(me) // the server says whose it is
		out, _ := json.Marshal(body)
		r.Header.Set("Content-Type", "application/json")
		s.forwardBody(w, r, to, out)
	case strings.HasPrefix(p, "/api/agents/sessions/"):
		id, _, _ := strings.Cut(strings.TrimPrefix(p, "/api/agents/sessions/"), "/")
		n, name, known := s.byRoute(jobRoute(id))
		switch {
		case !known:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "message": "no session " + id})
		case !s.owns(me, s.workers.idOfRoute(jobRoute(id))):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "only the worker's owner may use its Claude Code sessions"})
		case n == nil:
			offlineMac(w, name)
		default:
			if r.Header.Get("Upgrade") != "" {
				log.Printf("%s attaches to the Claude Code session %s on %q", me, id, n.name)
			}
			s.forward(w, r, n) // a WebSocket (the tty) passes through the proxy as an upgrade
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// patchWorker: PATCH /api/workers/{id} {"owner"} — owner devices only (like PATCH /api/devices).
func (s *Server) patchWorker(w http.ResponseWriter, r *http.Request) {
	if !deviceOf(r).Owner {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "Only devices signed in with the password may change workers"})
		return
	}
	var body struct {
		Owner *string `json:"owner"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	if body.Owner == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": `{"owner": "<user>"}`})
		return
	}
	wk, ok, err := s.workers.setOwner(r.PathValue("id"), cleanUser(*body.Owner))
	switch {
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
	case !ok:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		log.Printf("worker %q (%s): owner %q, set by %q", wk.Name, wk.ID, wk.Owner, deviceOf(r).Name)
		writeJSON(w, http.StatusOK, wk)
	}
}

// setOwner changes an enrolled worker's owner ("" none).
func (ws *workers) setOwner(id, owner string) (Worker, bool, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, s := range ws.byHash {
		if s.ID == id {
			old := s.Owner
			s.Owner = owner
			if err := ws.saveLocked(); err != nil {
				s.Owner = old
				return Worker{}, false, fmt.Errorf("workers.json: %w", err)
			}
			return s.Worker, true, nil
		}
	}
	return Worker{}, false, nil
}

// idOfRoute is the worker id of a route ("" when unknown).
func (ws *workers) idOfRoute(route string) string {
	id, _ := ws.byRoute(route)
	return id
}
