package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/philipparndt/ai-connector/internal/pyjson"
)

// The client APIs (docs/api.md): OpenAI-compatible endpoints, TEI's /embed and Laya's /v1/systemone, for other
// applications. Embedding and classifying go to a worker's direct path (beside its queue); chats become jobs
// (openai.go).

const (
	defaultEmbedModel    = "qwen3-embedding-0.6b"
	defaultClassifyModel = "laya-typed-decisions"
	maxEmbedInputs       = 2048
	maxAPIBody           = 32 << 20
)

// clientAPI registers the client APIs on the server's mux, each behind requireAPI.
func (s *Server) clientAPI(mux *http.ServeMux) {
	api := http.NewServeMux()
	api.HandleFunc("GET /v1/models", s.v1Models)
	api.HandleFunc("POST /v1/embeddings", s.v1Embeddings)
	api.HandleFunc("POST /v1/chat/completions", s.v1Chat)
	api.HandleFunc("POST /v1/systemone", s.layaClassify)
	api.HandleFunc("POST /embed", s.teiEmbed)
	api.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		(&apiError{http.StatusNotFound, "unknown_url", r.Method + " " + r.URL.Path + " is not an endpoint here"}).openAI(w)
	})
	h := s.requireAPI(api)
	mux.Handle("/v1/", h)
	mux.Handle("POST /embed", h)
}

// ---------- errors ----------

// apiError is a failure of a client API call, written in the shape of the API that was called.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func badRequest(format string, a ...any) *apiError {
	return &apiError{http.StatusBadRequest, "", fmt.Sprintf(format, a...)}
}

// openAI: {"error":{"message","type","code"}}.
func (e *apiError) openAI(w http.ResponseWriter) { writeJSON(w, e.status, e.body()) }

// body is the error object (also sent inside a stream that already started).
func (e *apiError) body() map[string]any {
	typ := "invalid_request_error"
	if e.status >= 500 || e.status == http.StatusConflict {
		typ = "server_error"
	}
	var code any
	if e.code != "" {
		code = e.code
	}
	return map[string]any{"error": map[string]any{"message": e.message, "type": typ, "code": code}}
}

// tei: {"error","error_type"} as text-embeddings-inference answers.
func (e *apiError) tei(w http.ResponseWriter) {
	typ := "Validation"
	switch {
	case e.status == http.StatusConflict || e.status == http.StatusServiceUnavailable:
		typ = "Unhealthy"
	case e.status >= 500:
		typ = "Backend"
	}
	writeJSON(w, e.status, map[string]string{"error": e.message, "error_type": typ})
}

// readBody decodes a JSON request body into v.
func readBody(w http.ResponseWriter, r *http.Request, v any) *apiError {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAPIBody)).Decode(v); err != nil {
		return badRequest("the body is not the JSON this endpoint takes: %v", err)
	}
	return nil
}

// ---------- models and placement ----------

// fleetModel is a model of the connected workers, with the workers that list it.
type fleetModel struct {
	catalogModel
	workers []*node
}

// fleetModels is every model the workers list, in the order they first appear.
func fleetModels(nodes []*node) []fleetModel {
	var out []fleetModel
	at := map[string]int{}
	for _, n := range nodes {
		_, _, _, models := n.snapshot()
		for _, m := range models {
			i, seen := at[m.ID]
			if !seen {
				i = len(out)
				at[m.ID] = i
				out = append(out, fleetModel{catalogModel: m})
			}
			out[i].workers = append(out[i].workers, n)
		}
	}
	return out
}

// findModel finds a model by the name an application gives (docs/api.md): its id or a unique prefix of it, then the
// last path segment of a Hugging Face name (Qwen/Qwen3-Embedding-0.6B), then laya-<name> (typed-decisions); all
// case-insensitive.
func findModel(models []fleetModel, name string) (fleetModel, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return fleetModel{}, false
	}
	match := func(q string) (fleetModel, bool) {
		var hit fleetModel
		n := 0
		for _, m := range models {
			id := strings.ToLower(m.ID)
			if id == q {
				return m, true
			}
			if strings.HasPrefix(id, q) {
				hit, n = m, n+1
			}
		}
		return hit, n == 1
	}
	base := name[strings.LastIndex(name, "/")+1:]
	for _, q := range []string{name, base, "laya-" + base} {
		if q == "" {
			continue
		}
		if m, ok := match(q); ok {
			return m, true
		}
	}
	return fleetModel{}, false
}

// place picks the worker for a model of a kind: one that lists it (preferring one that has it loaded, see choose).
func (s *Server) place(ctx context.Context, name, kind string) (*node, catalogModel, *apiError) {
	routed, _ := s.fleet.pick(ctx)
	if len(routed) == 0 {
		return nil, catalogModel{}, &apiError{http.StatusServiceUnavailable, "no_worker", "no worker is connected"}
	}
	fm, ok := findModel(fleetModels(routed), name)
	if !ok && s.fleet.refreshModels(ctx, routed) { // pulled a moment ago? the workers' lists are up to 30 s old
		fm, ok = findModel(fleetModels(routed), name)
	}
	if !ok {
		return nil, catalogModel{}, &apiError{http.StatusNotFound, "model_not_found", fmt.Sprintf("no connected worker has the model %q", name)}
	}
	m := fm.catalogModel
	if m.Kind != kind {
		return nil, m, badRequest("%s is a %s model, this endpoint takes a %s model", m.ID, m.Kind, kind)
	}
	n, ok := choose(routed, s.fleet.primary(routed), m, true, nil)
	if !ok {
		return nil, m, &apiError{http.StatusConflict, "no_worker",
			fmt.Sprintf("no connected worker has enough memory for %s (needs ~%d GB)", m.ID, int(math.Ceil(needGB(m))))}
	}
	return n, m, nil
}

// direct calls a worker's direct path (POST path with the JSON of in) and decodes its answer into out.
func (s *Server) direct(ctx context.Context, n *node, path string, in, out any) *apiError {
	b, err := json.Marshal(in)
	if err != nil {
		return badRequest("%v", err)
	}
	resp, err := s.fleet.request(ctx, n, http.MethodPost, path, bytes.NewReader(b), "application/json")
	if err != nil {
		return &apiError{http.StatusBadGateway, "worker_error", n.name + ": " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct{ Error, Message string }
		json.Unmarshal(raw, &e)
		switch {
		case e.Error == "": // not the direct path's answer: an older worker
			return &apiError{http.StatusBadGateway, "worker_error", fmt.Sprintf("%s cannot do this (%s): update it", n.name, resp.Status)}
		case resp.StatusCode == http.StatusNotFound:
			return &apiError{http.StatusNotFound, "model_not_found", e.Message}
		case resp.StatusCode == http.StatusBadRequest:
			return badRequest("%s", e.Message)
		}
		return &apiError{http.StatusBadGateway, "worker_error", n.name + ": " + e.Message}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &apiError{http.StatusBadGateway, "worker_error", n.name + ": " + err.Error()}
	}
	return nil
}

// embed embeds texts with the model on a worker that has it.
func (s *Server) embed(ctx context.Context, model string, texts []string) ([][]float32, catalogModel, *apiError) {
	n, m, e := s.place(ctx, model, "embed")
	if e != nil {
		return nil, m, e
	}
	var out struct {
		Vectors [][]float32 `json:"vectors"`
	}
	if e := s.direct(ctx, n, "/api/direct/embed", map[string]any{"model": m.ID, "texts": texts}, &out); e != nil {
		return nil, m, e
	}
	if len(out.Vectors) != len(texts) {
		return nil, m, &apiError{http.StatusBadGateway, "worker_error", fmt.Sprintf("%s: %d vectors for %d texts", n.name, len(out.Vectors), len(texts))}
	}
	return out.Vectors, m, nil
}

// inputTexts reads an input that is a text or a list of texts (token arrays are refused: the workers tokenise).
func inputTexts(raw json.RawMessage) ([]string, *apiError) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, badRequest("the input is missing")
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, nil
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return nil, badRequest("the input is a text or a list of texts")
	}
	if len(list) == 0 {
		return nil, badRequest("the input is empty")
	}
	if len(list) > maxEmbedInputs {
		return nil, badRequest("at most %d inputs at once (got %d)", maxEmbedInputs, len(list))
	}
	texts := make([]string, len(list))
	for i, x := range list {
		if json.Unmarshal(x, &texts[i]) != nil {
			return nil, badRequest("input %d is not a text: token arrays are not taken, send the text", i)
		}
	}
	return texts, nil
}

// float32Base64 is a vector as OpenAI's encoding_format "base64" has it: little-endian float32s.
func float32Base64(v []float32) string {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// ---------- OpenAI: models, embeddings ----------

// v1Models: GET /v1/models — the chat, embed and classify models of the connected workers.
func (s *Server) v1Models(w http.ResponseWriter, r *http.Request) {
	routed, _ := s.fleet.pick(r.Context())
	data := []map[string]any{}
	for _, m := range fleetModels(routed) {
		if m.Kind != "chat" && m.Kind != "embed" && m.Kind != "classify" {
			continue
		}
		names := make([]string, len(m.workers))
		for i, n := range m.workers {
			names[i] = n.name
		}
		data = append(data, map[string]any{"id": m.ID, "object": "model", "created": 0, "owned_by": "ai-connector",
			"kind": m.Kind, "workers": names})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// v1Embeddings: POST /v1/embeddings.
func (s *Server) v1Embeddings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model          string          `json:"model"`
		Input          json.RawMessage `json:"input"`
		EncodingFormat string          `json:"encoding_format"`
	}
	if e := readBody(w, r, &body); e != nil {
		e.openAI(w)
		return
	}
	texts, e := inputTexts(body.Input)
	if e != nil {
		e.openAI(w)
		return
	}
	if body.EncodingFormat != "" && body.EncodingFormat != "float" && body.EncodingFormat != "base64" {
		badRequest("encoding_format is float or base64").openAI(w)
		return
	}
	model := body.Model
	if model == "" {
		model = s.cfg.EmbedModel
	}
	vecs, m, e := s.embed(r.Context(), model, texts)
	if e != nil {
		e.openAI(w)
		return
	}
	data := make([]map[string]any, len(vecs))
	for i, v := range vecs {
		var emb any = v
		if body.EncodingFormat == "base64" { // the OpenAI SDKs ask for this by default
			emb = float32Base64(v)
		}
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": emb}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "model": m.ID, "data": data,
		"usage": map[string]int{"prompt_tokens": 0, "total_tokens": 0}})
}

// ---------- TEI ----------

// teiEmbed: POST /embed as text-embeddings-inference answers it (vboard's EMBED_URL).
func (s *Server) teiEmbed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Inputs json.RawMessage `json:"inputs"`
		// normalize, truncate: the vectors are always normalised; the embedder truncates long texts itself
	}
	if e := readBody(w, r, &body); e != nil {
		e.tei(w)
		return
	}
	texts, e := inputTexts(body.Inputs)
	if e != nil {
		e.tei(w)
		return
	}
	vecs, _, e := s.embed(r.Context(), s.cfg.EmbedModel, texts)
	if e != nil {
		e.tei(w)
		return
	}
	writeJSON(w, http.StatusOK, vecs)
}

// ---------- Laya ----------

// layaQuestion is one choice question of a /v1/systemone request, its options in the order they were sent.
type layaQuestion struct {
	id           string
	instructions string
	options      []map[string]string
}

// layaClassify: POST /v1/systemone as vboard-ai's classifier answers it (vboard's CLASSIFIER_URL).
func (s *Server) layaClassify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State     json.RawMessage `json:"state"`
		Model     string          `json:"model"`
		Questions json.RawMessage `json:"questions"`
	}
	if e := readBody(w, r, &body); e != nil {
		e.openAI(w)
		return
	}
	if st := bytes.TrimSpace(body.State); len(st) == 0 || string(st) == "null" {
		badRequest("the state is missing").openAI(w)
		return
	}
	// the text Laya reads is the state as vboard-ai passes it on: Python's json.dumps(state, ensure_ascii=False)
	state, err := pyjson.Marshal(body.State)
	if err != nil {
		badRequest("the state: %v", err).openAI(w)
		return
	}
	qs, err := layaQuestions(body.Questions)
	if err != nil {
		badRequest("%v", err).openAI(w)
		return
	}
	model := body.Model
	if model == "" {
		model = s.cfg.ClassifyModel
	}
	n, m, e := s.place(r.Context(), model, "classify")
	if e != nil {
		e.openAI(w)
		return
	}
	answers := map[string]any{}
	for _, q := range qs {
		var out struct {
			Answers []struct {
				Choice        string             `json:"choice"`
				Probabilities map[string]float64 `json:"probabilities"`
				Confidence    float64            `json:"confidence"`
			} `json:"answers"`
		}
		in := map[string]any{"model": m.ID, "states": []string{state}, "instructions": q.instructions, "options": q.options}
		if e := s.direct(r.Context(), n, "/api/direct/classify", in, &out); e != nil {
			e.openAI(w)
			return
		}
		if len(out.Answers) != 1 {
			(&apiError{http.StatusBadGateway, "worker_error", fmt.Sprintf("%s: %d answers for one state", n.name, len(out.Answers))}).openAI(w)
			return
		}
		a := out.Answers[0]
		answers[q.id] = map[string]any{"choice": a.Choice, "probabilities": a.Probabilities, "answer_confidence": a.Confidence}
	}
	writeJSON(w, http.StatusOK, map[string]any{"answers": answers})
}

// layaQuestions reads the questions. The criteria are an object whose order is the options' order (Laya reads them
// in it), so they are read member by member rather than into a map.
func layaQuestions(raw json.RawMessage) ([]layaQuestion, error) {
	ids, vals, err := orderedObject(raw)
	if err != nil {
		return nil, fmt.Errorf("questions: %w", err)
	}
	if len(ids) == 0 {
		return nil, errors.New("no questions")
	}
	out := make([]layaQuestion, len(ids))
	for i, id := range ids {
		var q struct {
			Type         string          `json:"type"`
			Instructions string          `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		}
		if err := json.Unmarshal(vals[i], &q); err != nil {
			return nil, fmt.Errorf("question %q: %w", id, err)
		}
		if q.Type != "" && q.Type != "choice" {
			return nil, fmt.Errorf("question %q: only choice questions are answered (not %q)", id, q.Type)
		}
		labels, descs, err := orderedObject(q.Criteria)
		if err != nil {
			return nil, fmt.Errorf("question %q: criteria: %w", id, err)
		}
		if len(labels) == 0 {
			return nil, fmt.Errorf("question %q: no criteria", id)
		}
		opts := make([]map[string]string, len(labels))
		for j, l := range labels {
			var d string
			if t := bytes.TrimSpace(descs[j]); string(t) != "null" && json.Unmarshal(t, &d) != nil {
				return nil, fmt.Errorf("question %q: criterion %q: the description is a text", id, l)
			}
			opts[j] = map[string]string{"label": l, "description": d}
		}
		out[i] = layaQuestion{id: id, instructions: q.Instructions, options: opts}
	}
	return out, nil
}

// orderedObject reads a JSON object's members in their order (a key given twice keeps its first place and its last
// value, as a Python dict does).
func orderedObject(raw json.RawMessage) (keys []string, vals []json.RawMessage, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, nil, errors.New("not an object")
	}
	at := map[string]int{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		k, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		if i, dup := at[k]; dup {
			vals[i] = v
			continue
		}
		at[k] = len(keys)
		keys, vals = append(keys, k), append(vals, v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, err
	}
	return keys, vals, nil
}
