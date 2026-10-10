package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/models"
)

// The setup of models (docs/api.md, "vboard: setting up models from vboard"): what this worker has and could
// download, and the downloads themselves — for the server's vboard connector (through the tunnel) and the vboard
// connector of `ai-connector vboard`.
//
//	GET    /api/setup[?access=1]     {memory, free, huggingFace:{token}, models:[engine.SetupModel], downloads:[…]}
//	POST   /api/downloads {model}    the download's state (a running one is joined; a model that is here is done)
//	GET    /api/downloads[?model=]   every download since the worker started, or one (404: none)
//	DELETE /api/downloads?model=     stops it (the partial files stay)
//
// A worker without Downloads answers 404 (an older worker has none of these paths either).

func (s *Server) downloadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/setup", s.setupInfo)
	mux.HandleFunc("POST /api/downloads", s.startDownload)
	mux.HandleFunc("GET /api/downloads", s.listDownloads)
	mux.HandleFunc("DELETE /api/downloads", s.cancelDownload)
}

// memory is this machine's memory in bytes (MLXLAB_FAKE_RAM_GB counts, as for /api/info).
func (s *Server) memory() int64 { return int64(s.specs().RAMGB) << 30 }

func (s *Server) downloadsOff(w http.ResponseWriter) bool {
	if s.Downloads == nil {
		http.Error(w, "this worker does not download models on request", http.StatusNotFound)
		return true
	}
	return false
}

func (s *Server) setupInfo(w http.ResponseWriter, r *http.Request) {
	if s.downloadsOff(w) {
		return
	}
	d := s.Downloads
	writeJSON(w, map[string]any{
		"memory":      s.memory(),
		"free":        d.FreeBytes(),
		"huggingFace": map[string]bool{"token": models.HFToken() != ""},
		"models":      d.Models(r.Context(), s.memory(), r.URL.Query().Get("access") != ""),
		"downloads":   d.List(),
	})
}

func (s *Server) startDownload(w http.ResponseWriter, r *http.Request) {
	if s.downloadsOff(w) {
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil || body.Model == "" {
		http.Error(w, "{\"model\"} expected", http.StatusBadRequest)
		return
	}
	st, err := s.Downloads.Start(body.Model, s.memory())
	switch {
	case errors.Is(err, engine.ErrNoModel):
		http.Error(w, err.Error(), http.StatusNotFound)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		writeJSON(w, st)
	}
}

func (s *Server) listDownloads(w http.ResponseWriter, r *http.Request) {
	if s.downloadsOff(w) {
		return
	}
	if id := r.URL.Query().Get("model"); id != "" {
		st, ok := s.Downloads.Status(id)
		if !ok {
			http.Error(w, "no download of "+id, http.StatusNotFound)
			return
		}
		writeJSON(w, st)
		return
	}
	writeJSON(w, map[string]any{"downloads": s.Downloads.List()})
}

func (s *Server) cancelDownload(w http.ResponseWriter, r *http.Request) {
	if s.downloadsOff(w) {
		return
	}
	s.Downloads.Cancel(r.URL.Query().Get("model"))
	w.WriteHeader(http.StatusNoContent)
}
