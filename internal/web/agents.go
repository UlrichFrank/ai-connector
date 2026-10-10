package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/coder/websocket"

	"github.com/philipparndt/ai-connector/internal/agents"
)

// Claude Code sessions on this worker (internal/agents; docs/api.md, "Agents (Claude Code)"):
//
//	GET    /api/agents/sessions           the sessions
//	POST   /api/agents/sessions           {"model","cwd"?,"owner"?} a tty session for a person → the session (201)
//	DELETE /api/agents/sessions/{id}      stop it (204)
//	GET    /api/agents/sessions/{id}/tty  WebSocket: the terminal of a tty session
//
// Session ids are lower-case letters and digits; through the hub they carry the worker's route like job ids
// ("<rid>.<id>", route.go).

func (s *Server) agentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agents/sessions", s.listSessions)
	mux.HandleFunc("POST /api/agents/sessions", s.openSession)
	mux.HandleFunc("DELETE /api/agents/sessions/{id}", s.stopSession)
	mux.HandleFunc("GET /api/agents/sessions/{id}/tty", s.attachSession)
}

func agentError(w http.ResponseWriter, status int, code string, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "message": err.Error()})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	if s.Agents == nil {
		writeJSON(w, []agents.Info{})
		return
	}
	writeJSON(w, s.Agents.List())
}

func (s *Server) openSession(w http.ResponseWriter, r *http.Request) {
	if s.Agents == nil {
		agentError(w, http.StatusNotFound, "agents", agents.ErrUnavailable)
		return
	}
	var body struct {
		Model string `json:"model"`
		Cwd   string `json:"cwd"`
		Owner string `json:"owner"` // the hub sets it (the signed-in person)
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		agentError(w, http.StatusBadRequest, "bad request", err)
		return
	}
	model := strings.TrimPrefix(body.Model, "claude-code/")
	in, err := s.Agents.OpenTTY(body.Owner, model, body.Cwd)
	switch {
	case errors.Is(err, agents.ErrUnavailable):
		agentError(w, http.StatusNotFound, "agents", err)
	case errors.Is(err, agents.ErrTooMany):
		agentError(w, http.StatusConflict, "busy", err)
	case errors.Is(err, agents.ErrTTYWindows):
		agentError(w, http.StatusNotImplemented, "tty", err)
	case err != nil:
		agentError(w, http.StatusBadRequest, "session", err)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(in)
	}
}

func (s *Server) stopSession(w http.ResponseWriter, r *http.Request) {
	if s.Agents == nil || s.Agents.Stop(r.PathValue("id")) != nil {
		agentError(w, http.StatusNotFound, "not found", agents.ErrNoSession)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// attachSession: the terminal of a tty session over a WebSocket — binary frames are the terminal's bytes (both
// ways), a text frame {"type":"resize","cols","rows"} sets its size. Several viewers may attach; each first gets
// what the terminal showed so far.
func (s *Server) attachSession(w http.ResponseWriter, r *http.Request) {
	if s.Agents == nil {
		agentError(w, http.StatusNotFound, "not found", agents.ErrNoSession)
		return
	}
	term, err := s.Agents.Terminal(r.PathValue("id"))
	switch {
	case errors.Is(err, agents.ErrNotTTY):
		agentError(w, http.StatusConflict, "tty", err)
		return
	case err != nil:
		agentError(w, http.StatusNotFound, "not found", err)
		return
	}
	// the default origin check stays: mlxlab serve listens on localhost, and a web page must not drive a terminal
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	snap, out, detach := term.Attach()
	defer detach()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer cancel()
		if len(snap) > 0 && c.Write(ctx, websocket.MessageBinary, snap) != nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case b, ok := <-out:
				if !ok {
					c.Close(websocket.StatusNormalClosure, "the session ended")
					return
				}
				if c.Write(ctx, websocket.MessageBinary, b) != nil {
					return
				}
			}
		}
	}()
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			if term.Write(b) != nil {
				c.Close(websocket.StatusNormalClosure, "the session ended")
				return
			}
			continue
		}
		var msg struct {
			Type       string
			Cols, Rows int
		}
		if json.Unmarshal(b, &msg) == nil && msg.Type == "resize" {
			term.Resize(msg.Cols, msg.Rows)
		}
	}
}
