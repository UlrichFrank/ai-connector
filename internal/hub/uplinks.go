package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/tunnel"
)

// Uplinks (docs/api.md "Uplinks"): connections the hub makes to an application that cannot reach it (the hub is at
// home, behind a router). The hub dials the application's URL with the application's token, as a worker dials the
// hub (internal/tunnel: a WebSocket carrying yamux streams), and the application then makes plain HTTP requests to
// the hub through it — the client API (/v1/*, /embed, the native jobs API, capabilities, the person's setup), as the
// uplink's client: a client made with the uplink, owned by the person who made it. Nothing else is reachable through
// an uplink, and the hub knows nothing of the application beyond its URL and token.

// Uplink is an uplink as the console shows it (never its token).
type Uplink struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	URL     string    `json:"url"`
	Enabled bool      `json:"enabled"`
	Owner   string    `json:"owner"`
	Client  string    `json:"client"` // the id of the client it acts as
	Created time.Time `json:"created"`
	// State: connecting (Error: why it waits), connected (Since), error (Error; stopped until it is edited), disabled
	State string     `json:"state"`
	Error string     `json:"error,omitempty"`
	Since *time.Time `json:"since,omitempty"`
}

type storedUplink struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	URL     string    `json:"url"`
	Token   string    `json:"token"`
	Enabled bool      `json:"enabled"`
	Owner   string    `json:"owner"`
	Client  string    `json:"client"`
	Created time.Time `json:"created"`
}

type uplinkEntry struct {
	conf   storedUplink
	state  string
	err    string
	since  time.Time
	gen    int
	cancel context.CancelFunc
}

type uplinks struct {
	s    *Server
	path string
	wg   sync.WaitGroup

	once    sync.Once
	handler http.Handler // the hub's, built once

	mu      sync.Mutex
	list    []*uplinkEntry
	changed chan struct{}
	closed  bool
}

// uplinkKey marks a request that came through an uplink: its value is the uplink's client.
type uplinkKey struct{}

// uplinkClient is the client a request through an uplink acts as.
func uplinkClient(r *http.Request) (Client, bool) {
	c, ok := r.Context().Value(uplinkKey{}).(Client)
	return c, ok
}

func openUplinks(s *Server, dir string) (*uplinks, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	u := &uplinks{s: s, path: filepath.Join(dir, "uplinks.json"), changed: make(chan struct{})}
	b, err := os.ReadFile(u.path)
	if errors.Is(err, os.ErrNotExist) {
		return u, nil
	}
	if err != nil {
		return nil, err
	}
	var list []storedUplink
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	for _, c := range list {
		u.list = append(u.list, &uplinkEntry{conf: c})
	}
	return u, nil
}

func (u *uplinks) startAll() {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, e := range u.list {
		u.startLocked(e)
	}
}

// startLocked (re)starts an uplink: the old connection ends, a new one is dialed when it is enabled.
func (u *uplinks) startLocked(e *uplinkEntry) {
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	e.gen++
	e.err, e.since = "", time.Time{}
	if !e.conf.Enabled || u.closed {
		e.state = "disabled"
		return
	}
	e.state = "connecting"
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	conf, gen := e.conf, e.gen
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		err := tunnel.ServeAt(ctx, conf.URL, conf.Token, conf.Name, u.serve(conf.Client), func(connected bool, err error) {
			u.report(conf.ID, gen, func(e *uplinkEntry) {
				if connected {
					e.state, e.err, e.since = "connected", "", time.Now().UTC().Truncate(time.Second)
					log.Printf("uplink %q: connected to %s", conf.Name, conf.URL)
					return
				}
				e.state, e.since = "connecting", time.Time{}
				if err != nil {
					e.err = err.Error()
					log.Printf("uplink %q: %v", conf.Name, err)
				}
			})
		})
		if ctx.Err() == nil && err != nil { // refused: retrying will not help
			u.report(conf.ID, gen, func(e *uplinkEntry) {
				e.state, e.err = "error", fmt.Sprintf("%s refused the token: edit the uplink", conf.URL)
			})
		}
	}()
}

// serve answers the requests that come through an uplink: the client API, as the uplink's client.
func (u *uplinks) serve(clientID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, "/v1/") && p != "/embed" && !clientMay(r) {
			notFound(w)
			return
		}
		c, ok := u.s.clients.byID(clientID)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "signin", "message": "the uplink's client was revoked"})
			return
		}
		r.Header.Del("Authorization") // nothing the application sends is a credential here
		r.Header.Del("Cookie")
		if q := r.URL.Query(); q.Has("token") {
			q.Del("token")
			r.URL.RawQuery = q.Encode()
		}
		u.once.Do(func() { u.handler = u.s.Handler() })
		u.handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), uplinkKey{}, c)))
	})
}

func (u *uplinks) report(id string, gen int, update func(e *uplinkEntry)) {
	u.mu.Lock()
	for _, e := range u.list {
		if e.conf.ID == id && e.gen == gen && e.cancel != nil {
			update(e)
		}
	}
	u.mu.Unlock()
	u.notify()
}

func (u *uplinks) Changed() <-chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.changed
}

func (u *uplinks) notify() {
	u.mu.Lock()
	close(u.changed)
	u.changed = make(chan struct{})
	u.mu.Unlock()
}

// Close ends every uplink (server shutdown).
func (u *uplinks) Close() {
	u.mu.Lock()
	u.closed = true
	for _, e := range u.list {
		if e.cancel != nil {
			e.cancel()
			e.cancel = nil
		}
	}
	u.mu.Unlock()
	u.wg.Wait()
}

func (e *uplinkEntry) view() Uplink {
	v := Uplink{ID: e.conf.ID, Name: e.conf.Name, URL: e.conf.URL, Enabled: e.conf.Enabled, Owner: e.conf.Owner,
		Client: e.conf.Client, Created: e.conf.Created, State: e.state, Error: e.err}
	if !e.since.IsZero() {
		t := e.since
		v.Since = &t
	}
	return v
}

func (u *uplinks) views() []Uplink {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]Uplink, len(u.list))
	for i, e := range u.list {
		out[i] = e.view()
	}
	return out
}

func (u *uplinks) saveLocked() error {
	list := make([]storedUplink, len(u.list))
	for i, e := range u.list {
		list[i] = e.conf
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := u.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, u.path)
}

// ---------- checks ----------

func cleanUplinkURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	p, err := url.Parse(s)
	if err != nil || p.Host == "" || (p.Scheme != "http" && p.Scheme != "https" && p.Scheme != "ws" && p.Scheme != "wss") {
		return "", errors.New("the uplink's URL: http(s)://host[:port]/path (or ws(s)://) of the application's endpoint")
	}
	return s, nil
}

func cleanUplinkToken(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return "", errors.New("the application's token: a text without spaces")
	}
	return s, nil
}

// ---------- endpoints (a device) ----------

func (s *Server) uplinkAPI(auth *http.ServeMux) {
	auth.HandleFunc("GET /api/uplinks", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.uplinks.views()) })
	auth.HandleFunc("POST /api/uplinks", s.addUplink)
	auth.HandleFunc("PATCH /api/uplinks/{id}", s.patchUplink)
	auth.HandleFunc("DELETE /api/uplinks/{id}", s.deleteUplink)
	auth.HandleFunc("GET /api/uplinks/events", s.uplinkEvents)
}

type uplinkBody struct {
	Name    *string `json:"name"`
	URL     *string `json:"url"`
	Token   *string `json:"token"`
	Enabled *bool   `json:"enabled"`
}

func uplinkError(w http.ResponseWriter, field string, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": field, "message": err.Error()})
}

// addUplink: POST /api/uplinks {name?, url, token, enabled?} — with its client, owned by the device's user.
func (s *Server) addUplink(w http.ResponseWriter, r *http.Request) {
	var b uplinkBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	c := storedUplink{ID: randomString(9), Name: "uplink", Enabled: true, Owner: deviceOf(r).User, Created: time.Now().UTC().Truncate(time.Second)}
	if b.Name != nil && strings.TrimSpace(*b.Name) != "" {
		c.Name = trimRunes(strings.TrimSpace(*b.Name), 80)
	}
	var err error
	if c.URL, err = cleanUplinkURL(deref(b.URL)); err != nil {
		uplinkError(w, "url", err)
		return
	}
	if c.Token, err = cleanUplinkToken(deref(b.Token)); err != nil {
		uplinkError(w, "token", err)
		return
	}
	if b.Enabled != nil {
		c.Enabled = *b.Enabled
	}
	_, cl, err := s.clients.add(c.Name, c.Owner) // its token is never used: the uplink's requests act as it
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	c.Client = cl.ID
	e := &uplinkEntry{conf: c}
	u := s.uplinks
	u.mu.Lock()
	u.list = append(u.list, e)
	err = u.saveLocked()
	if err == nil {
		u.startLocked(e)
	}
	v := e.view()
	u.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	u.notify()
	log.Printf("uplink made: %q to %s by %q, owner %q", c.Name, c.URL, deviceOf(r).Name, c.Owner)
	writeJSON(w, http.StatusCreated, v)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// patchUplink: PATCH /api/uplinks/{id} {name?, url?, token? (blank keeps it), enabled?} — it dials again.
func (s *Server) patchUplink(w http.ResponseWriter, r *http.Request) {
	var b uplinkBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	u := s.uplinks
	u.mu.Lock()
	var e *uplinkEntry
	for _, x := range u.list {
		if x.conf.ID == r.PathValue("id") {
			e = x
		}
	}
	if e == nil {
		u.mu.Unlock()
		notFound(w)
		return
	}
	c := e.conf
	var err error
	if b.Name != nil && strings.TrimSpace(*b.Name) != "" {
		c.Name = trimRunes(strings.TrimSpace(*b.Name), 80)
	}
	if b.URL != nil {
		if c.URL, err = cleanUplinkURL(*b.URL); err != nil {
			u.mu.Unlock()
			uplinkError(w, "url", err)
			return
		}
	}
	if b.Token != nil && strings.TrimSpace(*b.Token) != "" {
		if c.Token, err = cleanUplinkToken(*b.Token); err != nil {
			u.mu.Unlock()
			uplinkError(w, "token", err)
			return
		}
	}
	if b.Enabled != nil {
		c.Enabled = *b.Enabled
	}
	old := e.conf
	e.conf = c
	if err = u.saveLocked(); err != nil {
		e.conf = old
		u.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	u.startLocked(e)
	v := e.view()
	u.mu.Unlock()
	u.notify()
	writeJSON(w, http.StatusOK, v)
}

// deleteUplink: DELETE /api/uplinks/{id} — the connection ends, its client is revoked.
func (s *Server) deleteUplink(w http.ResponseWriter, r *http.Request) {
	u := s.uplinks
	u.mu.Lock()
	var gone *uplinkEntry
	kept := u.list[:0:0]
	for _, e := range u.list {
		if e.conf.ID == r.PathValue("id") {
			gone = e
			continue
		}
		kept = append(kept, e)
	}
	if gone == nil {
		u.mu.Unlock()
		notFound(w)
		return
	}
	u.list = kept
	err := u.saveLocked()
	if gone.cancel != nil {
		gone.cancel()
		gone.cancel = nil
	}
	u.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	if _, err := s.clients.revoke(gone.conf.Client); err != nil {
		log.Printf("uplink %q: revoking its client: %v", gone.conf.Name, err)
	}
	u.notify()
	w.WriteHeader(http.StatusNoContent)
}

// uplinkEvents: GET /api/uplinks/events — the list at once and on every change (SSE).
func (s *Server) uplinkEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		changed := s.uplinks.Changed()
		b, _ := json.Marshal(s.uplinks.views())
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
		for waiting := true; waiting; {
			select {
			case <-r.Context().Done():
				return
			case <-changed:
				waiting = false
			case <-ping.C:
				fmt.Fprint(w, ": ping\n\n")
				fl.Flush()
			}
		}
	}
}

// EnsureUplink makes the uplink to url, or brings the one there is up to date (its name, a new token; "" keeps the
// stored one): the standalone hub's way to an application (`ai-connector local --uplink`). Its client acts for owner.
func (s *Server) EnsureUplink(name, rawURL, token, owner string) (Uplink, error) {
	u := s.uplinks
	addr, err := cleanUplinkURL(rawURL)
	if err != nil {
		return Uplink{}, err
	}
	if strings.TrimSpace(token) != "" {
		if token, err = cleanUplinkToken(token); err != nil {
			return Uplink{}, err
		}
	}
	if name = trimRunes(strings.TrimSpace(name), 80); name == "" {
		name = "uplink"
	}
	u.mu.Lock()
	var e *uplinkEntry
	for _, x := range u.list {
		if x.conf.URL == addr {
			e = x
		}
	}
	if e == nil && token == "" {
		u.mu.Unlock()
		return Uplink{}, fmt.Errorf("the first uplink to %s needs the application's token", addr)
	}
	u.mu.Unlock()
	if e == nil {
		_, cl, err := s.clients.add(name, owner) // its token is never used: the uplink's requests act as it
		if err != nil {
			return Uplink{}, err
		}
		u.mu.Lock()
		e = &uplinkEntry{conf: storedUplink{ID: randomString(9), Name: name, URL: addr, Token: token, Enabled: true, Owner: owner,
			Client: cl.ID, Created: time.Now().UTC().Truncate(time.Second)}}
		u.list = append(u.list, e)
	} else {
		client := ""
		if _, ok := s.clients.byID(e.conf.Client); !ok { // revoked in the console: a new one
			_, cl, err := s.clients.add(name, owner)
			if err != nil {
				return Uplink{}, err
			}
			client = cl.ID
		}
		u.mu.Lock()
		e.conf.Name, e.conf.Enabled = name, true
		if token != "" {
			e.conf.Token = token
		}
		if client != "" {
			e.conf.Client = client
		}
	}
	err = u.saveLocked()
	if err == nil {
		u.startLocked(e)
	}
	v := e.view()
	u.mu.Unlock()
	if err != nil {
		return Uplink{}, err
	}
	u.notify()
	return v, nil
}

// Uplinks is the uplinks' state now; UplinksChanged fires on the next change (for a log of the state).
func (s *Server) Uplinks() []Uplink               { return s.uplinks.views() }
func (s *Server) UplinksChanged() <-chan struct{} { return s.uplinks.Changed() }
