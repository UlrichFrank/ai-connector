package hub

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/tunnel"
)

// Worker is an enrolled Mac: made in the web UI ("Connect a Mac"), it connects with its own token.
type Worker struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"lastSeen"`
	// Route is the short id that prefixes this Mac's job ids and file urls ("m1": "m1.<job id>",
	// "/files/m1/…"); given once at enrollment, so links stay valid across restarts.
	Route string `json:"route"`
	// Owner is the person (a device's user) who enrolled it: Claude Code on it runs with their subscription, so
	// only their claude-code jobs go there (agents.go). "" for the legacy worker and workers from before owners.
	Owner string `json:"owner,omitempty"`
}

// legacyRoute is the route of the worker with the legacy shared token.
const legacyRoute = "m0"

// storedWorker is a worker as it sits in workers.json: with the hash of its token, never the token itself.
type storedWorker struct {
	Worker
	Hash string `json:"hash"`
}

// workers keeps the enrolled workers' tokens in <data>/workers.json (like devices).
type workers struct {
	path string

	mu     sync.Mutex
	byHash map[string]*storedWorker
}

func openWorkers(dir string) (*workers, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ws := &workers{path: filepath.Join(dir, "workers.json"), byHash: map[string]*storedWorker{}}
	b, err := os.ReadFile(ws.path)
	if errors.Is(err, os.ErrNotExist) {
		return ws, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*storedWorker
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	missing := false
	for _, s := range list {
		ws.byHash[s.Hash] = s
		missing = missing || s.Route == ""
	}
	if missing { // workers.json from before routes: each Mac gets one now, in the order they were made
		sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
		for _, s := range list {
			if s.Route == "" {
				s.Route = ws.nextRouteLocked()
			}
		}
		if err := ws.saveLocked(); err != nil {
			return nil, err
		}
	}
	return ws, nil
}

// nextRouteLocked is the first free route m1, m2, … (m0 is the legacy worker's).
func (ws *workers) nextRouteLocked() string {
	taken := map[string]bool{}
	for _, s := range ws.byHash {
		taken[s.Route] = true
	}
	for i := 1; ; i++ {
		if r := "m" + strconv.Itoa(i); !taken[r] {
			return r
		}
	}
}

// route is the route of a worker id (the legacy one's too); "" when unknown.
func (ws *workers) route(id string) string {
	if id == tunnel.Legacy {
		return legacyRoute
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, s := range ws.byHash {
		if s.ID == id {
			return s.Route
		}
	}
	return ""
}

// byRoute is the worker id of a route ("" when unknown) and its enrolled name.
func (ws *workers) byRoute(route string) (id, name string) {
	if route == legacyRoute {
		return tunnel.Legacy, "Mac"
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, s := range ws.byHash {
		if s.Route == route {
			return s.ID, s.Name
		}
	}
	return "", ""
}

// add enrolls a worker and returns its token (only the caller ever sees it).
func (ws *workers) add(name, owner string) (string, Worker, error) {
	token := randomString(32)
	now := time.Now().UTC().Truncate(time.Second)
	s := &storedWorker{Worker: Worker{ID: randomString(9), Name: name, Created: now, Owner: owner}, Hash: hashToken(token)}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	s.Route = ws.nextRouteLocked()
	ws.byHash[s.Hash] = s
	if err := ws.saveLocked(); err != nil {
		delete(ws.byHash, s.Hash)
		return "", Worker{}, err
	}
	return token, s.Worker, nil
}

// lookup finds the worker of a token and notes that it was seen (a worker connects or downloads rarely, so
// every time is written).
func (ws *workers) lookup(token string) (Worker, bool) {
	if token == "" {
		return Worker{}, false
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	s, ok := ws.byHash[hashToken(token)]
	if !ok {
		return Worker{}, false
	}
	s.LastSeen = time.Now().UTC().Truncate(time.Second)
	if err := ws.saveLocked(); err != nil { // best effort: a missed lastSeen is not worth refusing the Mac
		log.Printf("workers.json: %v", err)
	}
	return s.Worker, true
}

func (ws *workers) list() []Worker {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	out := make([]Worker, 0, len(ws.byHash))
	for _, s := range ws.byHash {
		out = append(out, s.Worker)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// revoke removes a worker by id; false when there is none.
func (ws *workers) revoke(id string) (bool, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for h, s := range ws.byHash {
		if s.ID == id {
			delete(ws.byHash, h)
			return true, ws.saveLocked()
		}
	}
	return false, nil
}

func (ws *workers) saveLocked() error {
	list := make([]*storedWorker, 0, len(ws.byHash))
	for _, s := range ws.byHash {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
	return writeFileAtomic(ws.path, list)
}

// ---------- endpoints ----------

func (s *Server) listWorkers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.workerList())
}

// workerItem is a Mac in GET /api/workers: enrolled (or the legacy one while it is connected), what it is and
// what it does while connected.
type workerItem struct {
	Worker
	Connected bool         `json:"connected"`
	Legacy    bool         `json:"legacy,omitempty"`
	Since     string       `json:"since,omitempty"`
	Primary   bool         `json:"primary"`
	Pinned    bool         `json:"pinned,omitempty"` // the primary because someone chose it
	Routes    bool         `json:"routes"`           // it understands route ids (an older mlxlab does not)
	Specs     *Specs       `json:"specs,omitempty"`
	State     *WorkerState `json:"state,omitempty"`
}

func (s *Server) workerList() []workerItem {
	out := []workerItem{}
	prim := s.fleet.primaryID()
	pinned := s.fleet.pinnedID()
	item := func(wk Worker) workerItem {
		it := workerItem{Worker: wk, Primary: wk.ID == prim, Pinned: wk.ID == pinned}
		if n := s.fleet.node(wk.ID); n != nil {
			it.Connected, it.Since = true, n.since.UTC().Format(time.RFC3339)
			n.mu.Lock()
			it.Routes = n.routes
			if n.known && n.routes {
				sp, st := n.specs, n.state
				it.Specs, it.State = &sp, &st
			}
			n.mu.Unlock()
		}
		return it
	}
	if n := s.fleet.node(tunnel.Legacy); n != nil {
		it := item(Worker{ID: tunnel.Legacy, Name: n.name, Route: legacyRoute})
		it.Legacy = true
		out = append(out, it)
	}
	for _, wk := range s.workers.list() {
		out = append(out, item(wk))
	}
	return out
}

// setPrimary: PUT /api/workers/{id}/primary pins the Mac whose libraries (prompts, tools …) are served;
// DELETE goes back to the automatic choice (the most memory).
func (s *Server) setPrimary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == http.MethodDelete {
		if err := s.fleet.pin(""); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.workerList())
		return
	}
	if s.workers.route(id) == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err := s.fleet.pin(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.workerList())
}

func (s *Server) addWorker(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "Mac"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	token, wk, err := s.workers.add(name, deviceOf(r).User)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	log.Printf("worker enrolled: %q (%s) by %q", wk.Name, wk.ID, deviceOf(r).Name)
	writeJSON(w, http.StatusOK, map[string]any{"worker": wk, "token": token})
}

func (s *Server) revokeWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ok, err := s.workers.revoke(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if s.fleet.pinnedID() == id {
		s.fleet.pin("")
	}
	if s.tunnel.Disconnect(id) {
		log.Printf("worker %s revoked: its tunnel is closed", id)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
