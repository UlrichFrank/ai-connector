// Package hub is the public side of mlxlab (Raspberry Pi, behind traefik): it serves the web UI, signs devices
// in, and forwards every other /api/* and /files/* request to the worker on the Mac through the tunnel. It does
// not touch models or media itself. See docs/architecture.md for the API.
package hub

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/consoleui"
	"github.com/philipparndt/ai-connector/internal/tunnel"
	"github.com/philipparndt/ai-connector/internal/webui"
)

// CookieName is the session cookie the web UI signs in with.
const CookieName = "mlxlab_session"

// Config is what the hub needs to run (from the environment, see FromEnv).
type Config struct {
	Password    string // the sign-in password
	WorkerToken string // the legacy shared worker token (optional: each Mac gets its own token in the web UI)
	Data        string // directory for devices.json and workers.json
	Addr        string // listen address
	Tools       string // directory with mlxlab/<GOOS-GOARCH>, the worker programs served at /tools/mlxlab/
	// the client APIs' default models (docs/api.md): MLXLAB_EMBED_MODEL, MLXLAB_CLASSIFY_MODEL
	EmbedModel    string
	ClassifyModel string
	// ConsoleHosts: host names (lower case, no port) on which / redirects to the console (/console/)
	ConsoleHosts []string
}

// FromEnv reads MLXLAB_PASSWORD, MLXLAB_WORKER_TOKEN, MLXLAB_DATA, MLXLAB_ADDR, MLXLAB_TOOLS, MLXLAB_EMBED_MODEL,
// MLXLAB_CLASSIFY_MODEL and MLXLAB_CONSOLE_HOSTS (a comma list of host names).
func FromEnv() Config {
	c := Config{
		Password:      os.Getenv("MLXLAB_PASSWORD"),
		WorkerToken:   os.Getenv("MLXLAB_WORKER_TOKEN"),
		Data:          os.Getenv("MLXLAB_DATA"),
		Addr:          os.Getenv("MLXLAB_ADDR"),
		Tools:         os.Getenv("MLXLAB_TOOLS"),
		EmbedModel:    os.Getenv("MLXLAB_EMBED_MODEL"),
		ClassifyModel: os.Getenv("MLXLAB_CLASSIFY_MODEL"),
	}
	c.ConsoleHosts = parseHosts(os.Getenv("MLXLAB_CONSOLE_HOSTS"))
	if c.Data == "" {
		c.Data = "/data"
	}
	if c.Addr == "" {
		c.Addr = ":8080"
	}
	if c.Tools == "" {
		c.Tools = "/tools"
	}
	return c
}

// Server is the hub.
type Server struct {
	cfg     Config
	tunnel  *tunnel.Hub
	devices *devices
	workers *workers
	clients *clients
	invites *invites
	logins  *limiter
	fleet   *fleet
	proxy   http.Handler
	rp      *httputil.ReverseProxy
	ui      http.Handler
	console http.Handler

	// failDelay slows down wrong passwords (shorter in tests).
	failDelay time.Duration
}

// New checks the configuration and opens devices.json and workers.json. Without a password the hub would be
// open to anyone, so it refuses to start. The worker token is optional: Macs are enrolled in the web UI.
func New(cfg Config) (*Server, error) {
	if cfg.Password == "" {
		return nil, errors.New("MLXLAB_PASSWORD is not set")
	}
	if cfg.Data == "" {
		cfg.Data = "/data"
	}
	if cfg.Tools == "" {
		cfg.Tools = "/tools"
	}
	if cfg.EmbedModel == "" {
		cfg.EmbedModel = defaultEmbedModel
	}
	if cfg.ClassifyModel == "" {
		cfg.ClassifyModel = defaultClassifyModel
	}
	d, err := openDevices(cfg.Data)
	if err != nil {
		return nil, fmt.Errorf("devices: %w", err)
	}
	ws, err := openWorkers(cfg.Data)
	if err != nil {
		return nil, fmt.Errorf("workers: %w", err)
	}
	cs, err := openClients(cfg.Data)
	if err != nil {
		return nil, fmt.Errorf("clients: %w", err)
	}
	check := func(token string) (string, string, bool) {
		wk, ok := ws.lookup(token)
		return wk.ID, wk.Name, ok
	}
	s := &Server{
		cfg:       cfg,
		tunnel:    &tunnel.Hub{Check: check, Token: cfg.WorkerToken},
		devices:   d,
		workers:   ws,
		clients:   cs,
		invites:   newInvites(),
		logins:    newLimiter(10, time.Minute),
		ui:        webui.Handler(),
		console:   consoleui.Handler(),
		failDelay: time.Second,
	}
	s.fleet = newFleet(s.tunnel, ws, cfg.Data)
	s.proxy = s.newProxy()
	return s, nil
}

// Tunnel is the worker connection (for logging who is connected).
func (s *Server) Tunnel() *tunnel.Hub { return s.tunnel }

// Handler routes: the hub's own endpoints, the worker tunnel, the proxy to the worker and the static UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("POST /api/login", s.login)
	mux.Handle("GET "+tunnel.Path, s.tunnel)               // checks the worker token itself
	mux.HandleFunc("GET /tools/mlxlab/{platform}", s.tool) // a worker token or a device token

	// everything below needs a device token
	auth := http.NewServeMux()
	auth.HandleFunc("POST /api/logout", s.logout)
	auth.HandleFunc("GET /api/devices", s.listDevices)
	auth.HandleFunc("PATCH /api/devices/{id}", s.patchDevice)
	auth.HandleFunc("DELETE /api/devices/{id}", s.revokeDevice)
	auth.HandleFunc("POST /api/invites", s.createInvite)
	auth.HandleFunc("GET /api/workers", s.listWorkers)
	auth.HandleFunc("POST /api/workers", s.addWorker)
	auth.HandleFunc("DELETE /api/workers/{id}", s.revokeWorker)
	auth.HandleFunc("PUT /api/workers/{id}/primary", s.setPrimary)
	auth.HandleFunc("DELETE /api/workers/{id}/primary", s.setPrimary)
	auth.HandleFunc("GET /api/worker/events", s.workerEvents)
	auth.HandleFunc("GET /api/clients", s.listClients)
	auth.HandleFunc("POST /api/clients", s.addClient)
	auth.HandleFunc("DELETE /api/clients/{id}", s.revokeClient)
	auth.Handle("/api/", s.proxy)
	auth.Handle("/files/", s.proxy)
	mux.Handle("/api/", s.requireDevice(auth))
	mux.Handle("/files/", s.requireDevice(auth))
	s.clientAPI(mux) // /v1/*, /embed: a client token or a device token (api.go)

	mux.Handle(consoleui.Prefix, s.console) // the console; GET / on its host names leads there
	mux.Handle("/", s.withConsoleHosts(s.ui))
	return mux
}

// Run serves until ctx ends, then shuts down gracefully: streams (SSE) are cancelled, running requests get a
// few seconds to finish.
func (s *Server) Run(ctx context.Context) error {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second, // no Read/WriteTimeout: uploads and SSE take as long as they take
		BaseContext:       func(net.Listener) context.Context { return base },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	cancel() // ends the event streams, which would otherwise hold Shutdown up
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	return srv.Shutdown(sctx)
}

// ---------- auth ----------

type ctxKey struct{}

// tokenOf finds the device token: the Authorization header, the session cookie, or ?token= (players and
// EventSource cannot set headers).
func tokenOf(r *http.Request) string {
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
	}
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return r.URL.Query().Get("token")
}

func (s *Server) requireDevice(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := tokenOf(r)
		dev, ok := s.devices.lookup(token)
		if !ok && clientMay(r) { // an application's token reaches the native jobs API, nothing else here
			if c, ok := s.clients.lookup(bearer(r)); ok {
				next.ServeHTTP(w, withClient(r, c))
				return
			}
		}
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "signin"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, dev)))
	})
}

func deviceOf(r *http.Request) Device {
	d, _ := r.Context().Value(ctxKey{}).(Device)
	return d
}

// secure: the request reached traefik (or us) over TLS, so the cookie may be marked Secure.
func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSession(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// clientIP is who is signing in. Behind traefik the peer is traefik itself, so its X-Real-Ip (or the last
// X-Forwarded-For entry, the one traefik added) names the client.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-Ip"); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- endpoints ----------

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	dev, signedIn := s.devices.lookup(tokenOf(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"app":       "mlxlab",
		"role":      "hub",
		"name":      "mlxlab",
		"signedIn":  signedIn,
		"user":      dev.User,
		"owner":     dev.Owner,
		"canInvite": dev.mayInvite(),
		"worker":    s.workerState(),
		"workers":   s.connectedWorkers(),
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.logins.allow(clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate", "message": "Too many attempts, try again in a minute"})
		return
	}
	var body struct {
		Password string `json:"password"`
		Invite   string `json:"invite"`
		Device   string `json:"device"`
		User     string `json:"user"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	// checked before the invite, so a missing name does not use the code up
	user := cleanUser(body.User)
	if user == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user", "message": "Who uses this device? A user name is needed"})
		return
	}
	invitedBy := "" // an owner: signed in with the password
	if body.Invite != "" {
		by, ok := s.invites.use(body.Invite)
		if ok { // the device that made it must still be there and still allowed to invite
			inviter, found := s.devices.get(by)
			ok = found && inviter.mayInvite()
		}
		if !ok {
			time.Sleep(s.failDelay)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invite", "message": "The code is wrong, used or expired"})
			return
		}
		invitedBy = by
	} else if subtle.ConstantTimeCompare([]byte(body.Password), []byte(s.cfg.Password)) != 1 {
		time.Sleep(s.failDelay) // makes guessing slow, on top of the rate limit
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "password", "message": "Wrong password"})
		return
	}
	name := strings.TrimSpace(body.Device)
	if name == "" {
		name = "Device"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	token, dev, err := s.devices.add(name, user, invitedBy)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	how := "password"
	if invitedBy != "" {
		how = "invite of " + invitedBy
	}
	log.Printf("signed in: %q of %q (%s) with the %s from %s", dev.Name, dev.User, dev.ID, how, clientIP(r))
	setSession(w, r, token, 365*24*60*60)
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "device": dev})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	dev := deviceOf(r)
	if _, err := s.devices.revoke(dev.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	setSession(w, r, "", -1)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Device
		Current bool `json:"current"`
	}
	me := deviceOf(r).ID
	out := []item{}
	for _, d := range s.devices.list() {
		out = append(out, item{d, d.ID == me})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ok, err := s.devices.revoke(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if id == deviceOf(r).ID {
		setSession(w, r, "", -1)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// workerState is the primary Mac (the one field of old clients, from when one Mac was connected at a time).
type workerState struct {
	Connected bool   `json:"connected"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Since     string `json:"since,omitempty"`
}

func (s *Server) workerState() workerState {
	n := s.fleet.node(s.fleet.primaryID())
	if n == nil {
		return workerState{}
	}
	return workerState{Connected: true, ID: n.id, Name: n.name, Since: n.since.UTC().Format(time.RFC3339)}
}

// connectedWorkers are the Macs on the tunnel now.
func (s *Server) connectedWorkers() []workerItem {
	out := []workerItem{}
	for _, it := range s.workerList() {
		if it.Connected {
			out = append(out, it)
		}
	}
	return out
}

// workerEvents streams the Macs: the primary in the old fields, and every connected one; once at once, then on
// every change (one comes, goes, is known, the primary changes). A comment line every 25 s keeps proxies from
// closing a quiet stream.
func (s *Server) workerEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	changed := s.fleet.Changed() // taken before the state, so no change slips between the two
	var last []byte
	send := func() {
		b, _ := json.Marshal(struct {
			workerState
			Workers []workerItem `json:"workers"`
		}{s.workerState(), s.connectedWorkers()})
		if bytes.Equal(b, last) {
			return
		}
		last = b
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
	send()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changed:
			changed = s.fleet.Changed()
			send()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
