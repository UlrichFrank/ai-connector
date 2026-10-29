package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/tunnel"
)

// Routing the API among the connected Macs (docs/architecture.md, "Several Macs (workers) at once"). Job ids are
// "<route>.<id>" and file urls "/files/<route>/…" (the workers write them so, internal/web/route.go); the hub
// reads the route to find the Mac.

// ---------- does a model fit a Mac ----------

// headroomGB is what macOS and the rest of the Mac keep besides a model.
const headroomGB = 6

// peaks are the memory a runtime takes at its peak where that is known better than from the model's size (the
// weights × 1.3 rule is far off for these: they stream, offload or quantise on load). Measured on the Mac Studio
// while the runtimes were set up (the numbers in the multi-Mac task, docs/architecture.md; LTX q8 from its
// catalogue note "wants 64 GB"). Matched by model id prefix, the longest first. The models' manifests carry these
// now (catalogModel.Memory, which wins); the list stays for workers from before that and the chat models in Go.
var peaks = []struct {
	prefix string
	gb     float64
}{
	{"ltx-2.5-q4", 16}, // LTX-2.5 int4, distilled two-stage
	{"ltx-2.5-avatar", 16},
	{"ltx-2.5-q8", 45},
	// LTX-2.5 HQ (the dev transformer, res_2s + CFG, then the distilled LoRA fused in): q8 measured 30 GB footprint
	// at 512×320, 49 frames (64 GB M-series; the LoRA fusion at the stage change is the peak), more at bigger sizes;
	// q4 estimated from its smaller transformer and text encoder (−9 GB of weights).
	{"ltx-2.5-hq-q8", 34},
	{"ltx-2.5-hq-q4", 26},
	{"longcat-avatar-1.5-q8", 37},
	{"ace-step-", 10},
	{"flux2-klein-4b", 15},
	{"flux2-klein-base-4b", 15},
	// Z-Image Turbo, mflux's 4-bit pack: estimated from its 5.5 GB of weights at 1024² (not measured yet)
	{"z-image-turbo", 9},
	// Qwen-Image q8 (the bf16 text encoder stays loaded): 2512 measured 40.1 GB at 512², 8 steps (M-series, 64 GB);
	// 1024² and the edit model's reference tokens add a little. 2.1: estimated from mflux's notes (46 GB bf16).
	{"qwen-image-2512", 42},
	{"qwen-image-edit-2511", 44},
	{"qwen-image-2.1", 30},
	{"qwen3.6-35b-a3b", 19},
	// MiniMax Music 3: 20.4 GB measured on the M1 Max (8-bit, 30 s; the flow windows are fixed-size, so a longer song
	// grows only the LM's KV cache); 4-bit estimated from the 5 GB smaller weights
	{"minimax-music3-8bit", 21},
	{"minimax-music3-4bit", 16},
}

// needGB is the memory a model wants: its peak as the worker says or as known here, else its size × 1.3, plus the
// headroom.
func needGB(m catalogModel) float64 {
	if m.Memory > 0 {
		return m.Memory + headroomGB
	}
	best, peak := 0, 0.0
	for _, p := range peaks {
		if strings.HasPrefix(m.ID, p.prefix) && len(p.prefix) > best {
			best, peak = len(p.prefix), p.gb
		}
	}
	if best == 0 {
		peak = float64(m.Size) / (1 << 30) * 1.3
	}
	return peak + headroomGB
}

// fits: the Mac has the memory for the model (a Mac that does not say how much it has: yes).
func fits(n *node, m catalogModel) bool {
	ram := n.ram()
	return ram == 0 || float64(ram) >= needGB(m)
}

// ---------- forwarding ----------

type targetKey struct{}

func target(r *http.Request) *node {
	n, _ := r.Context().Value(targetKey{}).(*node)
	return n
}

// forward sends the request to one Mac as it is (the worker maps ids and paths itself).
func (s *Server) forward(w http.ResponseWriter, r *http.Request, n *node) {
	s.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, n)))
}

// forwardBody forwards with another body.
func (s *Server) forwardBody(w http.ResponseWriter, r *http.Request, n *node, body []byte) {
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))
	r2.Header.Del("Content-Length")
	s.forward(w, r2, n)
}

func offlineMac(w http.ResponseWriter, name string) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "offline", "message": name + " is offline"})
}

// byRoute finds the Mac of a route: known (enrolled, or the legacy one), and connected (nil when not).
func (s *Server) byRoute(route string) (n *node, name string, known bool) {
	id, name := s.workers.byRoute(route)
	if id == "" {
		return nil, "", false
	}
	if n := s.fleet.node(id); n != nil && n.isRouted() {
		return n, n.name, true
	}
	return nil, name, true
}

// fileRoute is the route of a /files/<route>/… url ("" if it has none).
func fileRoute(u string) string {
	rest, ok := strings.CutPrefix(u, "/files/")
	if !ok {
		return ""
	}
	route, _, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return route
}

// jobRoute is the route of a job id "<route>.<id>" ("" if it has none).
func jobRoute(id string) string {
	route, _, ok := strings.Cut(id, ".")
	if !ok {
		return ""
	}
	return route
}

// dispatch routes everything of the API that is not the hub's own.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	routed, single := s.fleet.pick(r.Context())
	if single != nil { // an older mlxlab alone: everything to it, as before
		s.forward(w, r, single)
		return
	}
	if len(routed) == 0 {
		if r.Method == "POST" && r.URL.Path == "/api/jobs" {
			s.submitOffline(w, r) // a claude-code job says whose worker it waits for (agents.go)
			return
		}
		offline(w)
		return
	}
	prim := s.fleet.primary(routed)
	p, m := r.URL.Path, r.Method
	switch {
	case m == "GET" && p == "/api/jobs":
		s.mergedJobs(w, r, routed)
	case m == "GET" && p == "/api/events":
		s.mergedEvents(w, r)
	case m == "GET" && p == "/api/models":
		s.mergedModels(w, r, routed, prim)
	case m == "POST" && p == "/api/jobs":
		s.submit(w, r, routed, prim)
	case strings.HasPrefix(p, "/api/jobs/"):
		id, _, _ := strings.Cut(strings.TrimPrefix(p, "/api/jobs/"), "/")
		s.toRoute(w, r, jobRoute(id), prim)
	case strings.HasPrefix(p, "/files/"):
		s.toRoute(w, r, fileRoute(p), prim)
	case strings.HasPrefix(p, "/api/agents/"): // Claude Code sessions: the person's own workers (agents.go)
		s.agentsAPI(w, r, routed)
	case m == "GET" && p == "/api/voices":
		s.mergedVoices(w, r, routed, prim)
	case strings.HasPrefix(p, "/api/voices/"):
		id, _, _ := strings.Cut(strings.TrimPrefix(p, "/api/voices/"), "/")
		holder := s.voiceHolder(r.Context(), routed, id)
		if holder == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "message": "no connected Mac has the voice " + id})
			return
		}
		s.forward(w, r, holder)
	case m == "GET" && p == "/api/styles":
		s.mergedStyles(w, r, routed, prim)
	case strings.HasPrefix(p, "/api/styles/"):
		id, _, _ := strings.Cut(strings.TrimPrefix(p, "/api/styles/"), "/")
		holder := s.styleHolder(r.Context(), routed, id)
		if holder == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "message": "no connected Mac has the style " + id})
			return
		}
		s.forward(w, r, holder)
	case m == "POST" && (p == "/api/voices" || p == "/api/upload" || p == "/api/styles"),
		m == "GET" && p == "/api/community-styles": // (a LoRA is looked up where it will be added)
		s.forward(w, r, s.chosen(r, prim)) // ?worker=<id>, else the primary
	case m == "POST" && p == "/api/prompts", m == "DELETE" && strings.HasPrefix(p, "/api/prompts/"),
		(m == "PUT" || m == "DELETE") && strings.HasPrefix(p, "/api/approvals/"):
		s.fanOut(w, r, routed, prim)
	case m == "POST" && (p == "/api/folders/rename" || p == "/api/unload"):
		s.toAll(w, r, routed)
	default: // the libraries (prompts, approvals, tools …) and anything new: the primary
		s.forward(w, r, prim)
	}
}

// toRoute forwards to the Mac of a route; without one (an id from before, a file of the primary), the primary.
func (s *Server) toRoute(w http.ResponseWriter, r *http.Request, route string, prim *node) {
	if route == "" {
		s.forward(w, r, prim)
		return
	}
	n, name, known := s.byRoute(route)
	switch {
	case !known:
		s.forward(w, r, prim) // not a route after all (a folder name): the primary's
	case n == nil:
		offlineMac(w, name)
	default:
		s.forward(w, r, n)
	}
}

// chosen is the Mac a request names with ?worker=<id>, else the primary.
func (s *Server) chosen(r *http.Request, prim *node) *node {
	if id := r.URL.Query().Get("worker"); id != "" {
		if n := s.fleet.node(id); n != nil && n.isRouted() {
			return n
		}
	}
	return prim
}

// ---------- merged lists ----------

// each asks every Mac at once; f gets the answer of each (nil body on an error).
func (s *Server) each(ctx context.Context, nodes []*node, path string) [][]byte {
	out := make([][]byte, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			r, err := s.fleet.request(ctx, n, "GET", path, nil, "")
			if err != nil {
				log.Printf("%s from %q: %v", path, n.name, err)
				return
			}
			defer r.Body.Close()
			b, err := io.ReadAll(r.Body)
			if err != nil || r.StatusCode != http.StatusOK {
				log.Printf("%s from %q: %d %v", path, n.name, r.StatusCode, err)
				return
			}
			out[i] = b
		}()
	}
	wg.Wait()
	return out
}

// mergedJobs: GET /api/jobs — every Mac's jobs, oldest first (as one Mac lists them).
func (s *Server) mergedJobs(w http.ResponseWriter, r *http.Request, nodes []*node) {
	type item struct {
		raw     json.RawMessage
		created time.Time
	}
	var all []item
	got := false
	for _, b := range s.each(r.Context(), nodes, "/api/jobs") {
		if b == nil {
			continue
		}
		var list []json.RawMessage
		if json.Unmarshal(b, &list) != nil {
			continue
		}
		got = true
		for _, raw := range list {
			var c struct {
				Created time.Time `json:"created"`
			}
			json.Unmarshal(raw, &c)
			all = append(all, item{raw, c.Created})
		}
	}
	if !got {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "worker", "message": "no Mac answered"})
		return
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].created.Before(all[j].created) })
	out := make([]json.RawMessage, len(all))
	for i, it := range all {
		out[i] = it.raw
	}
	writeJSON(w, http.StatusOK, out)
}

// mergedEvents: GET /api/events — every Mac's events in one stream. When a Mac comes or goes the stream ends, so
// the client connects again and loads the lists anew (with the new Mac's jobs, without the gone one's).
func (s *Server) mergedEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, stop := s.fleet.subscribe()
	defer stop()
	set := s.fleet.routedSet()
	changed := s.fleet.Changed()
	fmt.Fprint(w, ": hello\n\n")
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case data := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
		case <-changed:
			changed = s.fleet.Changed()
			if s.fleet.routedSet() != set {
				return
			}
		}
	}
}

// modelWorker is a Mac in a model's "workers".
type modelWorker struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Here   bool   `json:"here"`
	Loaded bool   `json:"loaded"`
	Fits   bool   `json:"fits"`
}

// mergedModels: GET /api/models — every model any Mac lists, each with the Macs: has it, has it loaded, fits.
func (s *Server) mergedModels(w http.ResponseWriter, r *http.Request, nodes []*node, prim *node) {
	order := append([]*node{prim}, without(nodes, prim)...)
	type answer struct {
		top    map[string]json.RawMessage
		models []map[string]any
		cat    []catalogModel
		loaded map[string]string
	}
	answers := make([]*answer, len(order))
	for i, b := range s.each(r.Context(), order, "/api/models") {
		if b == nil {
			continue
		}
		a := &answer{}
		var cat struct {
			Models []catalogModel    `json:"models"`
			Loaded map[string]string `json:"loaded"`
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if dec.Decode(&a.top) != nil || json.Unmarshal(b, &cat) != nil {
			continue
		}
		dec = json.NewDecoder(bytes.NewReader(a.top["models"]))
		dec.UseNumber()
		dec.Decode(&a.models)
		a.cat, a.loaded = cat.Models, cat.Loaded
		answers[i] = a
		n := order[i]
		n.mu.Lock()
		n.models = cat.Models // fresher than the last probe
		n.mu.Unlock()
	}
	var top map[string]json.RawMessage
	for _, a := range answers {
		if a != nil {
			top = a.top
			break
		}
	}
	if top == nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "worker", "message": "no Mac answered"})
		return
	}
	var ids []string
	entry := map[string]map[string]any{}
	cat := map[string]catalogModel{}
	for _, a := range answers {
		if a == nil {
			continue
		}
		for i, m := range a.models {
			id, _ := m["id"].(string)
			if id == "" || i >= len(a.cat) {
				continue
			}
			if _, seen := entry[id]; !seen {
				ids = append(ids, id)
			}
			if prev, seen := entry[id]; !seen || prev["here"] != true && m["here"] == true {
				entry[id], cat[id] = m, a.cat[i] // the copy of a Mac that has it (its size on disk, where)
			}
		}
	}
	loaded := map[string]string{}
	for _, a := range answers {
		if a == nil {
			continue
		}
		for b, m := range a.loaded {
			if m != "" && loaded[b] == "" {
				loaded[b] = m
			}
		}
	}
	models := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		m := entry[id]
		var ws []modelWorker
		for i, a := range answers {
			if a == nil {
				continue
			}
			mw := modelWorker{ID: order[i].id, Name: order[i].name, Fits: fits(order[i], cat[id])}
			lists := false
			for _, c := range a.cat {
				if c.ID == id {
					mw.Here, lists = c.Here, true
				}
			}
			if !lists { // this worker cannot run it (another platform, another runtime)
				continue
			}
			for _, l := range a.loaded {
				mw.Loaded = mw.Loaded || l == id
			}
			ws = append(ws, mw)
		}
		m["workers"] = ws
		models = append(models, m)
	}
	out := map[string]any{}
	for k, v := range top {
		out[k] = v
	}
	out["models"], out["loaded"] = models, loaded
	writeJSON(w, http.StatusOK, out)
}

func without(nodes []*node, n *node) []*node {
	var out []*node
	for _, x := range nodes {
		if x != n {
			out = append(out, x)
		}
	}
	return out
}

// mergedVoices: GET /api/voices — every Mac's voices (each with its Mac; they stay where they were made), the
// presets of the primary.
func (s *Server) mergedVoices(w http.ResponseWriter, r *http.Request, nodes []*node, prim *node) {
	order := append([]*node{prim}, without(nodes, prim)...)
	voices := []map[string]any{}
	var presets json.RawMessage
	got := false
	for i, b := range s.each(r.Context(), order, "/api/voices") {
		if b == nil {
			continue
		}
		var v struct {
			Voices  []map[string]any `json:"voices"`
			Presets json.RawMessage  `json:"presets"`
		}
		if json.Unmarshal(b, &v) != nil {
			continue
		}
		got = true
		if presets == nil {
			presets = v.Presets
		}
		for _, x := range v.Voices {
			x["worker"] = map[string]string{"id": order[i].id, "name": order[i].name}
			voices = append(voices, x)
		}
	}
	if !got {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "worker", "message": "no Mac answered"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"voices": voices, "presets": presets})
}

// voiceHolder is the Mac whose voice library has this id (nil: none; a preset voice is no library voice).
func (s *Server) voiceHolder(ctx context.Context, nodes []*node, id string) *node {
	for i, b := range s.each(ctx, nodes, "/api/voices") {
		var v struct {
			Voices []struct {
				ID string `json:"id"`
			} `json:"voices"`
		}
		if b == nil || json.Unmarshal(b, &v) != nil {
			continue
		}
		for _, x := range v.Voices {
			if x.ID == id {
				return nodes[i]
			}
		}
	}
	return nil
}

// mergedStyles: GET /api/styles — every Mac's trained styles (each with its Mac; a style stays where it was
// trained), the trainable models and defaults of the primary.
func (s *Server) mergedStyles(w http.ResponseWriter, r *http.Request, nodes []*node, prim *node) {
	order := append([]*node{prim}, without(nodes, prim)...)
	list := []map[string]any{}
	var models, defaults, community json.RawMessage
	got := false
	for i, b := range s.each(r.Context(), order, "/api/styles") {
		if b == nil {
			continue
		}
		var v struct {
			Styles    []map[string]any `json:"styles"`
			Models    json.RawMessage  `json:"models"`
			Defaults  json.RawMessage  `json:"defaults"`
			Community json.RawMessage  `json:"community"`
		}
		if json.Unmarshal(b, &v) != nil {
			continue
		}
		got = true
		if models == nil {
			models, defaults = v.Models, v.Defaults
		}
		if community == nil && len(v.Community) > 0 {
			community = v.Community
		}
		for _, x := range v.Styles {
			x["worker"] = map[string]string{"id": order[i].id, "name": order[i].name}
			list = append(list, x)
		}
	}
	if !got {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "worker", "message": "no Mac answered"})
		return
	}
	out := map[string]any{"styles": list, "models": models, "defaults": defaults}
	if community != nil {
		out["community"] = community // the curated community LoRAs (docs/music.md)
	}
	writeJSON(w, http.StatusOK, out)
}

// styleHolder is the Mac whose styles library has this id (nil: none).
func (s *Server) styleHolder(ctx context.Context, nodes []*node, id string) *node {
	for i, b := range s.each(ctx, nodes, "/api/styles") {
		var v struct {
			Styles []struct {
				ID string `json:"id"`
			} `json:"styles"`
		}
		if b == nil || json.Unmarshal(b, &v) != nil {
			continue
		}
		for _, x := range v.Styles {
			if x.ID == id {
				return nodes[i]
			}
		}
	}
	return nil
}

// ---------- writes to every Mac ----------

// answer is a Mac's response, read whole.
type answer struct {
	status int
	ctype  string
	body   []byte
	err    error
}

func (s *Server) send(ctx context.Context, n *node, method, path string, body []byte, ctype string) answer {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r, err := s.fleet.request(ctx, n, method, path, rd, ctype)
	if err != nil {
		return answer{err: err}
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	return answer{status: r.StatusCode, ctype: r.Header.Get("Content-Type"), body: b, err: err}
}

func (a answer) ok() bool { return a.err == nil && a.status >= 200 && a.status < 300 }

func (a answer) write(w http.ResponseWriter) {
	if a.err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "worker", "message": a.err.Error()})
		return
	}
	if a.ctype != "" {
		w.Header().Set("Content-Type", a.ctype)
	}
	w.WriteHeader(a.status)
	w.Write(a.body)
}

// fanOut: prompts and approvals stay alike on every Mac. The primary answers; the others get the same change
// (a new prompt with the id the primary gave it).
func (s *Server) fanOut(w http.ResponseWriter, r *http.Request, nodes []*node, prim *node) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	uri, ctype := r.URL.RequestURI(), r.Header.Get("Content-Type")
	a := s.send(r.Context(), prim, r.Method, uri, body, ctype)
	if a.ok() {
		if r.Method == http.MethodPost && r.URL.Path == "/api/prompts" {
			body = a.body // the saved prompt, with its id
		}
		var wg sync.WaitGroup
		for _, n := range without(nodes, prim) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if o := s.send(context.WithoutCancel(r.Context()), n, r.Method, uri, body, ctype); !o.ok() {
					log.Printf("%s %s on %q: %d %v %s", r.Method, uri, n.name, o.status, o.err, bytes.TrimSpace(o.body))
				}
			}()
		}
		wg.Wait()
	}
	a.write(w)
}

// toAll: renaming a folder and unloading go to every Mac (the library is merged; so is "free the memory").
func (s *Server) toAll(w http.ResponseWriter, r *http.Request, nodes []*node) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	answers := make([]answer, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = s.send(r.Context(), n, r.Method, r.URL.RequestURI(), body, r.Header.Get("Content-Type"))
		}()
	}
	wg.Wait()
	moved, good := 0, -1
	for i, a := range answers {
		if !a.ok() {
			continue
		}
		if good < 0 {
			good = i
		}
		var m struct{ Moved int }
		json.Unmarshal(a.body, &m)
		moved += m.Moved
	}
	switch {
	case good < 0:
		answers[0].write(w)
	case r.URL.Path == "/api/folders/rename":
		writeJSON(w, http.StatusOK, map[string]int{"moved": moved})
	default:
		answers[good].write(w)
	}
}

// ---------- a new job ----------

// resolve finds a model among the Macs' catalogues: its id, or a unique prefix of it (as a worker does).
func resolve(nodes []*node, id string) (catalogModel, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	var hit *catalogModel
	unique := true
	for _, n := range nodes {
		_, _, _, models := n.snapshot()
		for _, m := range models {
			if m.ID == id {
				return m, true
			}
			if id != "" && strings.HasPrefix(m.ID, id) {
				if hit != nil && hit.ID != m.ID {
					unique = false
				}
				hit = &m
			}
		}
	}
	if hit != nil && unique {
		return *hit, true
	}
	return catalogModel{}, false
}

// choose picks the worker for a model among those that list it and it fits: the model loaded > on disk > to
// download; then one that holds the request's files; then idle > fewer jobs waiting; then more memory; then the
// primary.
// ok=false: it fits none.
func choose(nodes []*node, prim *node, m catalogModel, known bool, holds map[*node]bool) (*node, bool) {
	type cand struct {
		n                            *node
		tier, holds, busy, load, ram int
	}
	var cs []cand
	for _, n := range nodes {
		if known && !fits(n, m) {
			continue
		}
		_, sp, st, models := n.snapshot()
		c := cand{n: n, tier: 2, busy: 1, load: st.load(), ram: sp.RAMGB}
		lists := false
		for _, x := range models {
			if x.ID == m.ID {
				lists = true
				if x.Here {
					c.tier = 1
				}
			}
		}
		// workers differ in what they can run (a Linux PC has no MLX, a runtime's models are its own): a known
		// model goes only to a worker that lists it
		if known && !lists {
			continue
		}
		for _, l := range st.Loaded {
			if known && l == m.ID {
				c.tier = 0
			}
		}
		if !holds[n] {
			c.holds = 1
		}
		if st.idle() {
			c.busy = 0
		}
		cs = append(cs, c)
	}
	if len(cs) == 0 {
		return nil, false
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		switch {
		case a.tier != b.tier:
			return a.tier < b.tier
		case a.holds != b.holds:
			return a.holds < b.holds
		case a.busy != b.busy:
			return a.busy < b.busy
		case a.load != b.load:
			return a.load < b.load
		case a.ram != b.ram:
			return a.ram > b.ram
		}
		return a.n == prim && b.n != prim
	})
	return cs[0].n, true
}

// submit: POST /api/jobs — to the Mac it belongs on (see choose), with the files it names copied there when they
// are on another Mac.
func (s *Server) submit(w http.ResponseWriter, r *http.Request, nodes []*node, prim *node) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAPIBody)) // a chat's images come with it
	var req map[string]json.RawMessage
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": "a job (JSON): " + fmt.Sprint(err)})
		return
	}
	str := func(k string) string {
		var v string
		json.Unmarshal(req[k], &v)
		return v
	}
	pin := str("worker")
	delete(req, "worker")
	// the application that submits it: from its token, never the caller's word
	delete(req, "client")
	if c, ok := clientOf(r); ok {
		req["client"], _ = json.Marshal(c.Name)
	}
	delete(req, "owner") // the server says whose a job is (agents.go)
	if isClaude(nodes, str("model")) {
		s.submitClaude(w, r, nodes, req, pin) // only to the person's own worker
		return
	}

	// what it works on decides: a follow-up, a cover for a song, a studio task on a song, a library voice
	var to *node
	for _, ref := range []string{jobRoute(str("parent")), jobRoute(str("coverFor")), fileRoute(str("source"))} {
		if ref == "" || to != nil {
			continue
		}
		n, name, known := s.byRoute(ref)
		if !known {
			continue
		}
		if n == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "offline",
				"message": name + " is offline (it holds what this job works on)"})
			return
		}
		to = n
	}
	if v := str("voice"); to == nil && v != "" {
		to = s.voiceHolder(r.Context(), nodes, v) // nil: a preset voice, every Mac has it
	}
	if v := str("style"); to == nil && v != "" {
		// a trained style stays on the Mac that trained it: songs with it (and its training) run there
		if to = s.styleHolder(r.Context(), nodes, v); to == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "message": "no connected Mac has the style " + v})
			return
		}
	}
	var images []string
	json.Unmarshal(req["images"], &images)
	audio := str("audio")
	switch {
	case to != nil:
	case pin != "":
		n := s.fleet.node(pin)
		if n == nil || !n.isRouted() {
			if wk, ok := s.findWorker(pin); ok {
				offlineMac(w, wk)
			} else {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "message": "no Mac " + pin})
			}
			return
		}
		to = n
	default:
		holds := map[*node]bool{}
		for _, u := range append(images, audio) {
			if n, _, _ := s.byRoute(fileRoute(u)); n != nil {
				holds[n] = true
			}
		}
		m, known := resolve(nodes, str("model"))
		n, ok := choose(nodes, prim, m, known, holds)
		if !ok {
			name := m.Name
			if name == "" {
				name = m.ID
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": "no-worker",
				"message": fmt.Sprintf("No connected Mac has enough memory for %s (needs ~%d GB)", name, int(math.Ceil(needGB(m))))})
			return
		}
		to = n
	}

	// files on another Mac come along
	copied := false
	for i, u := range images {
		if nu, err := s.bring(r.Context(), u, to); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "copy", "message": err.Error()})
			return
		} else if nu != u {
			images[i], copied = nu, true
		}
	}
	if audio != "" {
		nu, err := s.bring(r.Context(), audio, to)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "copy", "message": err.Error()})
			return
		}
		if nu != audio {
			req["audio"], _ = json.Marshal(nu)
		}
	}
	if copied {
		req["images"], _ = json.Marshal(images)
	}
	out, _ := json.Marshal(req)
	s.forwardBody(w, r, to, out)
}

// findWorker is the name of a Mac by id (enrolled, or the legacy one).
func (s *Server) findWorker(id string) (string, bool) {
	if id == tunnel.Legacy {
		return "The Mac", true
	}
	for _, wk := range s.workers.list() {
		if wk.ID == id {
			return wk.Name, true
		}
	}
	return "", false
}

// bring makes a file url usable on the Mac to: as it is when it is there (or no Mac's), else a copy uploaded
// to it.
func (s *Server) bring(ctx context.Context, u string, to *node) (string, error) {
	route := fileRoute(u)
	if route == "" || route == to.route {
		return u, nil
	}
	from, name, known := s.byRoute(route)
	if !known {
		return u, nil
	}
	if from == nil {
		return "", fmt.Errorf("%s is offline (it holds %s)", name, u)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	src, err := s.fleet.request(ctx, from, "GET", u, nil, "")
	if err != nil {
		return "", err
	}
	defer src.Body.Close()
	if src.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s from %s: %s", u, from.name, src.Status)
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", path.Base(u))
		if err == nil {
			_, err = io.Copy(part, src.Body)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	up, err := s.fleet.request(ctx, to, "POST", "/api/upload", pr, mw.FormDataContentType())
	if err != nil {
		return "", err
	}
	defer up.Body.Close()
	var res struct {
		URL string `json:"url"`
	}
	if up.StatusCode != http.StatusOK || json.NewDecoder(up.Body).Decode(&res) != nil || res.URL == "" {
		return "", fmt.Errorf("copying %s to %s: %s", u, to.name, up.Status)
	}
	return res.URL, nil
}
