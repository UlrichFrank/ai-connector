package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Several Macs behind one hub (docs/architecture.md, "Several Macs (workers) at once"): the hub sends every request
// with X-Mlxlab-Route: <rid>, and this worker then answers with job ids "<rid>.<id>" and file urls
// "/files/<rid>/…", and takes its own prefix off what comes in. It is done here, once, for every handler: the
// request's path and JSON body on the way in, the JSON answer and the event stream on the way out. Without the
// header (mlxlab serve) nothing changes.

const (
	// RouteHeader carries the route id the hub gave this worker.
	RouteHeader = "X-Mlxlab-Route"
	// WorkerIDHeader and WorkerNameHeader (URL-encoded) name this worker as the hub knows it; every job then
	// carries "worker": {"id", "name"}.
	WorkerIDHeader   = "X-Mlxlab-Worker-Id"
	WorkerNameHeader = "X-Mlxlab-Worker-Name"
)

// route is how the hub addresses this worker in one request.
type route struct {
	rid      string
	id, name string
}

type routeKey struct{}

// routeOf is the request's route (nil: no hub prefix).
func routeOf(r *http.Request) *route {
	rt, _ := r.Context().Value(routeKey{}).(*route)
	return rt
}

// ValidRoute: a route id is 1–16 lower-case letters and digits (it goes into ids and paths).
func ValidRoute(rid string) bool {
	if rid == "" || len(rid) > 16 {
		return false
	}
	for _, c := range rid {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// outID is a job id as the hub's clients see it.
func (rt *route) outID(id string) string {
	if rt == nil || id == "" || strings.HasPrefix(id, rt.rid+".") {
		return id
	}
	return rt.rid + "." + id
}

// inID takes the own prefix off a job id.
func (rt *route) inID(id string) string {
	if rt == nil {
		return id
	}
	return strings.TrimPrefix(id, rt.rid+".")
}

// Which strings are file urls. Only these get the route: a field that holds file urls (fileKeys: the string, or
// each string of its list), and any other string that is a /files/ url as a whole — never free text (textKeys and
// everything inside them: a chat's answer, its messages, logs, a tool's arguments), even when it mentions a
// /files/ path: what a model wrote reaches the caller as it was written. Keys are compared in lower case (an
// llm.Message is "Content").
var (
	fileKeys = map[string]bool{"files": true, "file": true, "preview": true, "cover": true, "images": true,
		"image": true, "audio": true, "source": true, "url": true, "sample": true, "video": true, "poster": true}
	textKeys = map[string]bool{"text": true, "content": true, "prompt": true, "negativeprompt": true, "lyrics": true,
		"system": true, "logs": true, "log": true, "status": true, "error": true, "message": true, "summary": true,
		"arguments": true, "reasoning": true, "thinking": true, "title": true, "description": true, "caption": true,
		"captions": true, "instructions": true, "problem": true}
)

// isURL: a string that is a /files/ url as a whole (no spaces: not a sentence that mentions one).
func isURL(s string) bool {
	return strings.HasPrefix(s, "/files/") && !strings.ContainsAny(s, " \t\r\n")
}

// outURL: /files/… becomes /files/<rid>/….
func (rt *route) outURL(s string) string {
	if !strings.HasPrefix(s, "/files/") || strings.HasPrefix(s, "/files/"+rt.rid+"/") {
		return s
	}
	return "/files/" + rt.rid + "/" + strings.TrimPrefix(s, "/files/")
}

// inURL: /files/<rid>/… becomes /files/… again.
func (rt *route) inURL(s string) string {
	if rest, ok := strings.CutPrefix(s, "/files/"+rt.rid+"/"); ok {
		return "/files/" + rest
	}
	return s
}

// mapURLs applies f to the file urls in v (key: the field v is in, "" at the top).
func mapURLs(key string, v any, f func(string) string) any {
	k := strings.ToLower(key)
	if textKeys[k] {
		return v
	}
	switch x := v.(type) {
	case string:
		if fileKeys[k] && strings.HasPrefix(x, "/files/") || isURL(x) {
			return f(x)
		}
	case []any:
		for i := range x {
			x[i] = mapURLs(key, x[i], f) // a list's strings are what its field holds
		}
	case map[string]any:
		for kk, e := range x {
			x[kk] = mapURLs(kk, e, f)
		}
	}
	return v
}

// inPath maps a request path: /api/jobs/<rid>.<id>/… and /files/<rid>/….
func (rt *route) inPath(p string) string {
	if rest, ok := strings.CutPrefix(p, "/api/jobs/"); ok {
		return "/api/jobs/" + rt.inID(rest)
	}
	if rest, ok := strings.CutPrefix(p, "/files/"+rt.rid+"/"); ok {
		return "/files/" + rest
	}
	if rest, ok := strings.CutPrefix(p, "/api/agents/sessions/"); ok { // a Claude Code session, like a job
		return "/api/agents/sessions/" + rt.inID(rest)
	}
	return p
}

// isSession: a Claude Code session (agents.Info as JSON).
func isSession(m map[string]any) bool {
	_, id := m["id"].(string)
	_, driver := m["driver"].(string)
	_, used := m["lastUsed"]
	return id && driver && used
}

// isJob: a job object (jobs.Job as JSON).
func isJob(m map[string]any) bool {
	_, id := m["id"].(string)
	_, state := m["state"]
	_, kind := m["kind"]
	_, req := m["request"].(map[string]any)
	return id && state && kind && req
}

// out maps a decoded JSON answer: job ids (a job's id and its request's parent and coverFor, an update's jobId),
// the file urls (mapURLs; not free text), and the worker on every job.
func (rt *route) out(v any) any {
	return rt.outIDs(mapURLs("", v, rt.outURL))
}

// outIDs maps the job and session ids, and adds the worker.
func (rt *route) outIDs(v any) any {
	switch x := v.(type) {
	case []any:
		for i := range x {
			x[i] = rt.outIDs(x[i])
		}
	case map[string]any:
		job, session := isJob(x), isSession(x)
		for k, e := range x {
			if !textKeys[strings.ToLower(k)] {
				x[k] = rt.outIDs(e)
			}
		}
		if id, ok := x["jobId"].(string); ok {
			x["jobId"] = rt.outID(id)
		}
		if job {
			x["id"] = rt.outID(x["id"].(string))
			req := x["request"].(map[string]any)
			for _, k := range []string{"parent", "coverFor"} {
				if s, ok := req[k].(string); ok {
					req[k] = rt.outID(s)
				}
			}
			if rt.id != "" {
				x["worker"] = map[string]string{"id": rt.id, "name": rt.name}
			}
		}
		if session {
			x["id"] = rt.outID(x["id"].(string))
			if rt.id != "" {
				x["worker"] = map[string]string{"id": rt.id, "name": rt.name}
			}
		}
	}
	return v
}

// in maps a decoded JSON request body: /files/<rid>/ urls (mapURLs; not free text) and the job ids in parent and
// coverFor.
func (rt *route) in(v any) any {
	return rt.inIDs(mapURLs("", v, rt.inURL))
}

func (rt *route) inIDs(v any) any {
	switch x := v.(type) {
	case []any:
		for i := range x {
			x[i] = rt.inIDs(x[i])
		}
	case map[string]any:
		for k, e := range x {
			if !textKeys[strings.ToLower(k)] {
				x[k] = rt.inIDs(e)
			}
		}
		for _, k := range []string{"parent", "coverFor"} {
			if s, ok := x[k].(string); ok {
				x[k] = rt.inID(s)
			}
		}
	}
	return v
}

// mapJSON decodes b (numbers kept as they are), maps it and encodes it again; ok=false: not JSON.
func mapJSON(b []byte, f func(any) any) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF { // more than one value: leave it alone
		return nil, false
	}
	out, err := json.Marshal(f(v))
	if err != nil {
		return nil, false
	}
	return append(out, '\n'), true
}

// routed wraps the API: with the hub's route header, requests and answers are mapped (see the top of this file).
func (s *Server) routed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get(RouteHeader)
		// the direct path has no job ids or files to map, and its texts and vectors are the caller's: as they are
		if !ValidRoute(rid) || strings.HasPrefix(r.URL.Path, "/api/direct/") {
			next.ServeHTTP(w, r)
			return
		}
		rt := &route{rid: rid, id: r.Header.Get(WorkerIDHeader)}
		rt.name, _ = url.QueryUnescape(r.Header.Get(WorkerNameHeader))
		r = r.Clone(context.WithValue(r.Context(), routeKey{}, rt))
		r.URL.Path, r.URL.RawPath = rt.inPath(r.URL.Path), ""
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead &&
			!strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			b, err := io.ReadAll(io.LimitReader(r.Body, 64<<20)) // a chat's images come with it
			r.Body.Close()
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if m, ok := mapJSON(b, rt.in); ok {
				b = m
			}
			r.Body = io.NopCloser(bytes.NewReader(b))
			r.ContentLength = int64(len(b))
			r.Header.Set("Content-Length", strconv.Itoa(len(b)))
		}
		rw := &routeWriter{ResponseWriter: w, rt: rt}
		next.ServeHTTP(rw, r)
		rw.finish()
	})
}

// routeWriter maps a JSON answer (buffered, then mapped whole) and an event stream (event by event); anything
// else (files, text) passes as it is.
type routeWriter struct {
	http.ResponseWriter
	rt     *route
	mode   int // 0 undecided, 1 pass, 2 json, 3 sse
	status int
	buf    bytes.Buffer
}

const (
	modePass = iota + 1
	modeJSON
	modeSSE
)

func (w *routeWriter) decide() {
	if w.mode != 0 {
		return
	}
	ct := w.Header().Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		w.mode = modeJSON
	case strings.HasPrefix(ct, "text/event-stream"):
		w.mode = modeSSE
	default:
		w.mode = modePass
	}
}

func (w *routeWriter) WriteHeader(status int) {
	w.decide()
	if w.mode == modeJSON {
		w.status = status // the length changes: the header goes with the body
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *routeWriter) Write(b []byte) (int, error) {
	w.decide()
	switch w.mode {
	case modeJSON:
		return w.buf.Write(b)
	case modeSSE:
		w.buf.Write(b)
		for {
			i := bytes.Index(w.buf.Bytes(), []byte("\n\n"))
			if i < 0 {
				return len(b), nil
			}
			event := string(w.buf.Next(i + 2))
			if _, err := io.WriteString(w.ResponseWriter, w.sse(event)); err != nil {
				return 0, err
			}
		}
	}
	return w.ResponseWriter.Write(b)
}

// sse maps the data lines of one event.
func (w *routeWriter) sse(event string) string {
	var out strings.Builder
	sc := bufio.NewScanner(strings.NewReader(event))
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			if m, ok := mapJSON([]byte(data), w.rt.out); ok {
				line = "data: " + strings.TrimSuffix(string(m), "\n")
			}
		}
		out.WriteString(line + "\n")
	}
	out.WriteString("\n")
	return out.String()
}

func (w *routeWriter) Flush() {
	if w.mode == modeJSON {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *routeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish writes a buffered JSON answer, mapped.
func (w *routeWriter) finish() {
	switch w.mode {
	case modeJSON:
		b := w.buf.Bytes()
		if m, ok := mapJSON(b, w.rt.out); ok {
			b = m
		}
		w.Header().Del("Content-Length")
		if w.status != 0 {
			w.ResponseWriter.WriteHeader(w.status)
		}
		w.ResponseWriter.Write(b)
	case modeSSE:
		if w.buf.Len() > 0 {
			io.WriteString(w.ResponseWriter, w.buf.String())
		}
	}
}
