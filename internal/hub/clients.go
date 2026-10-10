package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Client is an application that uses the client APIs (docs/api.md): docscanner, a script. Made in the console,
// it sends its own token (aic_…) and may use the OpenAI, TEI and Laya endpoints and the native jobs API — not
// devices, workers, clients or connectors.
type Client struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"lastSeen,omitzero"`
	// Owner is the person (the user of the device that made it) the application acts for: its claude-code jobs run
	// on their workers with their subscription (agents.go). "" for tokens from before owners.
	Owner string `json:"owner,omitempty"`
}

// clientPrefix starts every client token, so one is told from a device token at a glance (and in logs).
const clientPrefix = "aic_"

// storedClient is a client as it sits in clients.json: with the hash of its token, never the token itself.
type storedClient struct {
	Client
	Hash string `json:"hash"`
}

// clients keeps the client tokens in <data>/clients.json (like devices and workers).
type clients struct {
	path string

	mu     sync.Mutex
	byHash map[string]*storedClient
}

func openClients(dir string) (*clients, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	cs := &clients{path: filepath.Join(dir, "clients.json"), byHash: map[string]*storedClient{}}
	b, err := os.ReadFile(cs.path)
	if errors.Is(err, os.ErrNotExist) {
		return cs, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*storedClient
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	for _, s := range list {
		cs.byHash[s.Hash] = s
	}
	return cs, nil
}

// add makes a client and returns its token (only the caller ever sees it).
func (cs *clients) add(name, owner string) (string, Client, error) {
	token := clientPrefix + randomString(32)
	s := &storedClient{Client: Client{ID: randomString(9), Name: name, Created: time.Now().UTC().Truncate(time.Second), Owner: owner},
		Hash: hashToken(token)}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.byHash[s.Hash] = s
	if err := cs.saveLocked(); err != nil {
		delete(cs.byHash, s.Hash)
		return "", Client{}, err
	}
	return token, s.Client, nil
}

// lookup finds the client of a token and notes that it was used (written at most once a minute: an application
// may call many times a second).
func (cs *clients) lookup(token string) (Client, bool) {
	if !strings.HasPrefix(token, clientPrefix) {
		return Client{}, false
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	s, ok := cs.byHash[hashToken(token)]
	if !ok {
		return Client{}, false
	}
	if now := time.Now().UTC(); now.Sub(s.LastSeen) >= seenEvery {
		s.LastSeen = now.Truncate(time.Second)
		if err := cs.saveLocked(); err != nil { // best effort: a missed lastSeen is not worth failing the request
			log.Printf("clients.json: %v", err)
		}
	}
	return s.Client, true
}

func (cs *clients) list() []Client {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]Client, 0, len(cs.byHash))
	for _, s := range cs.byHash {
		out = append(out, s.Client)
	}
	sort.Slice(out, func(i, j int) bool { // made in the same second: by id, so the order stays
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// revoke removes a client by id; false when there is none.
func (cs *clients) revoke(id string) (bool, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for h, s := range cs.byHash {
		if s.ID == id {
			delete(cs.byHash, h)
			return true, cs.saveLocked()
		}
	}
	return false, nil
}

func (cs *clients) saveLocked() error {
	list := make([]*storedClient, 0, len(cs.byHash))
	for _, s := range cs.byHash {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].Created.Equal(list[j].Created) {
			return list[i].Created.Before(list[j].Created)
		}
		return list[i].ID < list[j].ID
	})
	return writeFileAtomic(cs.path, list)
}

// ---------- auth ----------

type clientKey struct{}

// clientOf is the client a request came with (ok=false: a device).
func clientOf(r *http.Request) (Client, bool) {
	c, ok := r.Context().Value(clientKey{}).(Client)
	return c, ok
}

func withClient(r *http.Request, c Client) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), clientKey{}, c))
}

// bearer is the Authorization header's token ("" none): the one way a client sends its token.
func bearer(r *http.Request) string {
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
	}
	return ""
}

// clientMay: the native jobs API, the one part of /api/* and /files/* a client token reaches (docs/api.md).
func clientMay(r *http.Request) bool {
	p, m := r.URL.Path, r.Method
	switch {
	case m == http.MethodGet && (p == "/api/models" || p == "/api/jobs" || p == "/api/events"):
		return true
	case m == http.MethodPost && p == "/api/jobs":
		return true
	case strings.HasPrefix(p, "/api/jobs/"):
		id, rest, sub := strings.Cut(strings.TrimPrefix(p, "/api/jobs/"), "/")
		return id != "" && (m == http.MethodGet && !sub || m == http.MethodPost && rest == "cancel")
	case m == http.MethodGet && strings.HasPrefix(p, "/files/"):
		return true
	}
	return false
}

// requireAPI guards the client APIs (/v1/*, /embed): a client token or a device token, else an OpenAI-shaped 401.
func (s *Server) requireAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := s.clients.lookup(bearer(r)); ok {
			next.ServeHTTP(w, withClient(r, c))
			return
		}
		if dev, ok := s.devices.lookup(tokenOf(r)); ok {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, dev)))
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
			"message": "a client token (Authorization: Bearer aic_…) is needed", "type": "invalid_request_error", "code": "invalid_api_key"}})
	})
}

// ---------- endpoints ----------

func (s *Server) listClients(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.clients.list())
}

func (s *Server) addClient(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name", "message": "The application needs a name"})
		return
	}
	if len(name) > 100 {
		name = name[:100]
	}
	token, c, err := s.clients.add(name, deviceOf(r).User)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	log.Printf("client made: %q (%s) by %q, owner %q", c.Name, c.ID, deviceOf(r).Name, c.Owner)
	writeJSON(w, http.StatusOK, map[string]any{"client": c, "token": token})
}

func (s *Server) revokeClient(w http.ResponseWriter, r *http.Request) {
	ok, err := s.clients.revoke(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
