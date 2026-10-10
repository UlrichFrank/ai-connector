package web

import (
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"

	"github.com/philipparndt/ai-connector/internal/engine"
)

// The direct path (docs/api.md, "Worker: direct path"): the server's embeddings, TEI and Laya APIs call these. They
// run beside the job queue — a search must not wait behind a video — and leave no job in the history.

// maxDirectTexts bounds one call (the OpenAI API allows 2048 inputs).
const maxDirectTexts = 4096

func directError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg})
}

// directModel finds the model and the backend of a direct call; nil when it answered with an error.
func (s *Server) directModel(w http.ResponseWriter, id string, kind engine.Kind) (*engine.Model, engine.Backend) {
	m := s.Jobs.Catalog.Find(id)
	if m == nil {
		directError(w, http.StatusNotFound, "model", "no model "+id)
		return nil, nil
	}
	if m.Kind != kind {
		directError(w, http.StatusBadRequest, "kind", m.ID+" is a "+string(m.Kind)+" model, not "+string(kind))
		return nil, nil
	}
	b, ok := s.Jobs.Backends[m.Backend]
	if !ok {
		directError(w, http.StatusNotFound, "model", m.ID+": backend "+m.Backend+" is not available")
		return nil, nil
	}
	return m, b
}

func (s *Server) directEmbed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string   `json:"model"`
		Texts []string `json:"texts"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&body); err != nil {
		directError(w, http.StatusBadRequest, "request", err.Error())
		return
	}
	if len(body.Texts) == 0 || len(body.Texts) > maxDirectTexts {
		directError(w, http.StatusBadRequest, "request", "1 to 4096 texts")
		return
	}
	m, b := s.directModel(w, body.Model, engine.Embed)
	if m == nil {
		return
	}
	e, ok := b.(engine.Embedder)
	if !ok {
		directError(w, http.StatusBadRequest, "kind", m.ID+" cannot embed directly")
		return
	}
	vecs, err := e.Embed(r.Context(), m, body.Texts)
	if err != nil {
		log.Printf("direct embed with %s: %v", m.ID, err)
		directError(w, http.StatusInternalServerError, "run", err.Error())
		return
	}
	dim := 0
	for _, v := range vecs {
		normalize(v) // the built-in embedder's are already; a runtime's need not be
		dim = len(v)
	}
	writeJSON(w, map[string]any{"model": m.ID, "dim": dim, "vectors": vecs})
}

func (s *Server) directClassify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model        string          `json:"model"`
		States       []string        `json:"states"`
		Instructions string          `json:"instructions"`
		Options      []engine.Option `json:"options"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&body); err != nil {
		directError(w, http.StatusBadRequest, "request", err.Error())
		return
	}
	if len(body.States) == 0 || len(body.States) > maxDirectTexts || len(body.Options) == 0 {
		directError(w, http.StatusBadRequest, "request", "a question needs 1 to 4096 states and options")
		return
	}
	m, b := s.directModel(w, body.Model, engine.Classify)
	if m == nil {
		return
	}
	c, ok := b.(engine.Classifier)
	if !ok {
		directError(w, http.StatusBadRequest, "kind", m.ID+" cannot classify directly")
		return
	}
	ans, err := c.Classify(r.Context(), m, body.States, body.Instructions, body.Options)
	if err != nil {
		log.Printf("direct classify with %s: %v", m.ID, err)
		directError(w, http.StatusInternalServerError, "run", err.Error())
		return
	}
	writeJSON(w, map[string]any{"model": m.ID, "answers": ans})
}

// normalize scales v to length 1 (a zero vector stays).
func normalize(v []float32) {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 || math.Abs(n-1) < 1e-6 {
		return
	}
	f := float32(1 / math.Sqrt(n))
	for i := range v {
		v[i] *= f
	}
}
