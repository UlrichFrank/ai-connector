package tunnel

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// ---------- hub side ----------

// Legacy is the worker id of a worker that came with the legacy shared token.
const Legacy = "legacy"

// Hub accepts workers — every one at once, one session per worker id (a newer connection of the same worker
// replaces its older one) — and hands out an http.RoundTripper to them: the URL host (Host(id)) names the worker.
// A worker is let in when Check knows its token (an enrolled worker) or when it brings Token (the legacy shared
// token, id Legacy); with neither set, every worker is refused.
type Hub struct {
	// Check looks up an enrolled worker's token: its id and name, ok=false when unknown. Optional.
	Check func(token string) (id, name string, ok bool)
	// Token is the legacy shared worker token. Optional.
	Token string

	mu   sync.Mutex
	conn map[string]*session
	// changed is closed (and replaced) whenever a worker connects or leaves.
	changed chan struct{}
}

type session struct {
	sess  *yamux.Session
	name  string
	since time.Time
}

// Conn is a connected worker.
type Conn struct {
	ID    string // the enrolled worker's id, Legacy for the shared token
	Name  string
	Since time.Time
}

// ErrOffline: the worker is not connected.
var ErrOffline = fmt.Errorf("the Mac is offline")

// Connected lists the connected workers, the longest connected first.
func (h *Hub) Connected() []Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []Conn{}
	for id, s := range h.conn {
		if !s.sess.IsClosed() {
			out = append(out, Conn{ID: id, Name: s.name, Since: s.since})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Lookup is the connected worker with this id.
func (h *Hub) Lookup(id string) (Conn, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.conn[id]
	if !ok || s.sess.IsClosed() {
		return Conn{}, false
	}
	return Conn{ID: id, Name: s.name, Since: s.since}, true
}

// Disconnect closes the tunnel of the worker with this id (its token was revoked); false when it is not connected.
func (h *Hub) Disconnect(id string) bool {
	if id == "" {
		return false
	}
	h.mu.Lock()
	s := h.conn[id]
	h.mu.Unlock()
	if s == nil {
		return false
	}
	s.sess.Close() // ServeHTTP sees it end and tells Changed
	return true
}

// Changed returns a channel that is closed at the next connect or disconnect.
func (h *Hub) Changed() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.changed == nil {
		h.changed = make(chan struct{})
	}
	return h.changed
}

func (h *Hub) notify() {
	if h.changed != nil {
		close(h.changed)
	}
	h.changed = make(chan struct{})
}

// authorize finds the worker of a token: an enrolled one through Check, else the legacy shared Token (Legacy).
func (h *Hub) authorize(token string) (id, name string, ok bool) {
	if token == "" {
		return "", "", false
	}
	if h.Check != nil {
		if id, name, ok := h.Check(token); ok {
			return id, name, true
		}
	}
	if h.Token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(h.Token)) == 1 {
		return Legacy, "", true
	}
	return "", "", false
}

// ServeHTTP is the handler for Path: it checks the worker token and keeps the connection until it drops.
// An enrolled worker is shown with the name it was enrolled with; a legacy one with the name it sends (its
// host name).
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	token = strings.TrimSpace(token)
	id, name, ok := h.authorize(token)
	if !ok {
		http.Error(w, "unknown worker token", http.StatusUnauthorized)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(-1)
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	sess, err := yamux.Client(conn, config())
	if err != nil {
		conn.Close()
		return
	}
	if name == "" {
		name = r.Header.Get(HeaderName)
	}
	if name == "" {
		name = "Mac"
	}
	me := &session{sess: sess, name: name, since: time.Now()}
	h.mu.Lock()
	if h.conn == nil {
		h.conn = map[string]*session{}
	}
	old := h.conn[id]
	h.conn[id] = me
	h.notify()
	h.mu.Unlock()
	if old != nil {
		old.sess.Close() // the same Mac again: its older connection is dead or about to be
	}
	// revoked between the check and now? Disconnect could not see this session yet, so look once more.
	if id != Legacy {
		if again, _, ok := h.authorize(token); !ok || again != id {
			sess.Close()
		}
	}
	log.Printf("worker %q (%s) connected from %s", name, id, r.RemoteAddr)
	<-sess.CloseChan()
	h.mu.Lock()
	if h.conn[id] == me {
		delete(h.conn, id)
		h.notify()
	}
	h.mu.Unlock()
	log.Printf("worker %q (%s) left", name, id)
}

// hostSuffix ends every worker host name (Host).
const hostSuffix = ".worker"

// Host is the URL host that reaches the worker with this id through Transport.
func Host(id string) string { return "w" + hex.EncodeToString([]byte(id)) + hostSuffix }

// idOf is the worker id of a Host (with or without a port).
func idOf(addr string) (string, bool) {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	enc, ok := strings.CutSuffix(host, hostSuffix)
	if !ok || !strings.HasPrefix(enc, "w") {
		return "", false
	}
	b, err := hex.DecodeString(enc[1:])
	if err != nil {
		return "", false
	}
	return string(b), true
}

// DialWorker opens a stream to the worker with this id.
func (h *Hub) DialWorker(id string) (net.Conn, error) {
	h.mu.Lock()
	s := h.conn[id]
	h.mu.Unlock()
	if s == nil || s.sess.IsClosed() {
		return nil, ErrOffline
	}
	c, err := s.sess.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	return c, nil
}

// Dial opens a stream to the worker that addr's host names (Host), for an http.Transport.
func (h *Hub) Dial(_ context.Context, _, addr string) (net.Conn, error) {
	id, ok := idOf(addr)
	if !ok {
		return nil, fmt.Errorf("%w: %s names no worker", ErrOffline, addr)
	}
	return h.DialWorker(id)
}

// Transport makes HTTP requests to the workers: http://<Host(id)>/… reaches the worker with that id.
func (h *Hub) Transport() *http.Transport {
	return &http.Transport{
		DialContext:         h.Dial,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // already compressed media; SSE must not be buffered
	}
}
