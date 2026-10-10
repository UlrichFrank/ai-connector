package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Connectors (docs/api.md "Connectors"): outbound connections the server makes to applications that expect a local
// AI to dial in. Kind vboard: the server dials vboard's classifier bridge with a vboard token and answers its
// classify, embed and chat requests through the workers (vboard.go). Kept in <data>/connectors.json (0600: the token
// is in it, in clear — the server needs it to dial); one goroutine per enabled connector.

// ConnectorModels are the models a connector answers with ("" = the server's default).
type ConnectorModels struct {
	Classify string `json:"classify"`
	Embed    string `json:"embed"`
	Chat     string `json:"chat"`
}

// ConnectorAgent is one of the AIs a connector offers vboard to chat with (docs/api.md "Connectors", agents): a
// chat model answered with vboard's tools (kind model) or an agent that works on the board itself through vboard's
// MCP server (kind agent: Claude Code). The first is the default (what a request that names none gets).
type ConnectorAgent struct {
	ID    string `json:"id"`    // stable: vboard's panel remembers the pick by it
	Model string `json:"model"` // a catalogue id or name as in /v1/* ("" only in the default list: the default chat model)
	Name  string `json:"name"`  // shown in vboard ("" = the model's name)
	Kind  string `json:"kind"`  // model | agent (claude-code/* is an agent; set by the server)
}

// AdvertisedAgent is an agent as the connector offers it to vboard now (its model offered by a connected worker).
type AdvertisedAgent struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Worker string `json:"worker,omitempty"` // where it would run (a guess, for vboard's panel)
	Model  string `json:"model"`            // the catalogue id it runs on
}

// Connector is a connector as the API shows it: its settings (never the token) and its live state.
type Connector struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Name    string          `json:"name"`
	URL     string          `json:"url"`
	Board   string          `json:"board"`
	Enabled bool            `json:"enabled"`
	Models  ConnectorModels `json:"models"`
	// Agents: the chosen list in its order ([] = the default: the chat model alone)
	Agents  []ConnectorAgent `json:"agents"`
	Owner   string           `json:"owner"` // the user of the device that made it (Device.User)
	Created time.Time        `json:"created"`
	// state: connecting | connected | error | disabled; error: why (also while it retries); since: connected since;
	// user: who the token belongs to, as vboard says; advertised: the agents vboard was told of (while connected)
	State      string            `json:"state"`
	Error      string            `json:"error"`
	Since      string            `json:"since,omitempty"`
	User       string            `json:"user"`
	Advertised []AdvertisedAgent `json:"advertised"`
}

// connectorDefaultName is the name (and what vboard shows as the local AI's name) when none is given.
const connectorDefaultName = "ai-connector"

// storedConnector is a connector as it sits in connectors.json.
type storedConnector struct {
	ID      string           `json:"id"`
	Kind    string           `json:"kind"`
	Name    string           `json:"name"`
	URL     string           `json:"url"`
	Token   string           `json:"token"`
	Board   string           `json:"board,omitempty"`
	Enabled bool             `json:"enabled"`
	Models  ConnectorModels  `json:"models"`
	Agents  []ConnectorAgent `json:"agents,omitempty"`
	Owner   string           `json:"owner"`
	Created time.Time        `json:"created"`
}

// connectorStatus is what a connector's runner reports.
type connectorStatus struct {
	State      string
	Error      string
	User       string
	Since      time.Time
	Advertised []AdvertisedAgent
}

type connectorEntry struct {
	conf   storedConnector
	status connectorStatus
	gen    int // the runner's generation: reports of a runner that was stopped are dropped
	cancel context.CancelFunc
}

// connectors keeps the connectors and runs them.
type connectors struct {
	s    *Server
	path string
	// sleep waits between attempts (tests replace it to see the backoff without waiting)
	sleep func(ctx context.Context, d time.Duration) error
	wg    sync.WaitGroup // the runners, and the old connections they still close after a handover

	mu      sync.Mutex
	list    []*connectorEntry
	changed chan struct{}
	closed  bool
}

func openConnectors(s *Server, dir string) (*connectors, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &connectors{s: s, path: filepath.Join(dir, "connectors.json"), sleep: sleepCtx, changed: make(chan struct{})}
	b, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var list []storedConnector
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	for _, c := range list {
		m.list = append(m.list, &connectorEntry{conf: c})
	}
	return m, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startAll starts the runners of the enabled connectors (on server start).
func (m *connectors) startAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.list {
		m.startLocked(e)
	}
}

// startLocked (re)starts a connector's runner after its settings changed: the old runner is stopped (it ends in the
// background: its requests are canceled), a new one starts when the connector is enabled.
func (m *connectors) startLocked(e *connectorEntry) {
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	e.gen++
	if !e.conf.Enabled || m.closed {
		e.status = connectorStatus{State: "disabled"}
		return
	}
	e.status = connectorStatus{State: "connecting"}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	r := &vboardRun{m: m, s: m.s, conf: e.conf, gen: e.gen, instance: "aic_" + randomString(12)}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		r.run(ctx)
	}()
}

// Close stops every runner and waits until they (and the connections they still close) are gone (server shutdown).
func (m *connectors) Close() {
	m.mu.Lock()
	m.closed = true
	for _, e := range m.list {
		if e.cancel != nil {
			e.cancel()
			e.cancel = nil
		}
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// report is a runner's new state (dropped when the runner was replaced or stopped meanwhile).
func (m *connectors) report(id string, gen int, update func(st *connectorStatus)) {
	m.mu.Lock()
	var changed bool
	for _, e := range m.list {
		if e.conf.ID == id && e.gen == gen && e.cancel != nil {
			before := e.status
			update(&e.status)
			changed = !reflect.DeepEqual(e.status, before)
		}
	}
	m.mu.Unlock()
	if changed {
		m.notify()
	}
}

// Changed is closed at the next change (settings or state).
func (m *connectors) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

func (m *connectors) notify() {
	m.mu.Lock()
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
}

func (e *connectorEntry) view() Connector {
	c := Connector{ID: e.conf.ID, Kind: e.conf.Kind, Name: e.conf.Name, URL: e.conf.URL, Board: e.conf.Board,
		Enabled: e.conf.Enabled, Models: e.conf.Models, Agents: append([]ConnectorAgent{}, e.conf.Agents...), Owner: e.conf.Owner,
		Created: e.conf.Created, State: e.status.State, Error: e.status.Error, User: e.status.User,
		Advertised: append([]AdvertisedAgent{}, e.status.Advertised...)}
	if c.State == "" {
		c.State = "disabled"
	}
	if !e.status.Since.IsZero() {
		c.Since = e.status.Since.UTC().Format(time.RFC3339)
	}
	return c
}

func (m *connectors) views() []Connector {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Connector, 0, len(m.list))
	for _, e := range m.list {
		out = append(out, e.view())
	}
	return out
}

func (m *connectors) saveLocked() error {
	list := make([]storedConnector, len(m.list))
	for i, e := range m.list {
		list[i] = e.conf
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil { // an older file of another mode
		return err
	}
	return os.Rename(tmp, m.path)
}

// ---------- validation ----------

type connectorError struct{ field, message string }

func (e *connectorError) Error() string { return e.message }

func cleanConnectorName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return connectorDefaultName
	}
	return trimRunes(s, 80) // vboard keeps 80 characters of hello.name
}

// cleanConnectorURL checks a vboard address: http or https with a host; a trailing slash is dropped.
func cleanConnectorURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if s == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", &connectorError{"url", "The vboard address is an http(s) address, like https://vboard.example.com"}
	}
	return strings.TrimRight(s, "/"), nil
}

// cleanConnectorToken checks a token loosely: vboard's personal tokens start with vbp_ (the prefix is configurable),
// so any printable text without spaces is taken.
func cleanConnectorToken(s string) (string, error) {
	s = strings.TrimSpace(s)
	ok := s != "" && len(s) <= 4096
	for _, r := range s {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			ok = false
		}
	}
	if !ok {
		return "", &connectorError{"token", "The vboard token is missing or not a token (vbp_…)"}
	}
	return s, nil
}

func cleanBoard(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) > 200 || strings.ContainsAny(s, "/?#& ") {
		return "", &connectorError{"board", "Not a board id"}
	}
	return s, nil
}

func cleanModels(m ConnectorModels) ConnectorModels {
	return ConnectorModels{Classify: strings.TrimSpace(m.Classify), Embed: strings.TrimSpace(m.Embed), Chat: strings.TrimSpace(m.Chat)}
}

// Agents: at most 20 (vboard keeps 20), ids and names of at most 80 characters (vboard's limits).
const (
	maxAgents    = 20
	agentChars   = 80
	agentDefault = "default" // the id of the default list's one entry
	kindModel    = "model"
	kindAgent    = "agent"
)

// agentKind: a claude-code model is an agent (it works on the board itself, through MCP), anything else a model.
func agentKind(model string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), claudePrefix) {
		return kindAgent
	}
	return kindModel
}

// agentID derives a stable id from a model id: lower-case letters, digits, ".", "-" and "_" (claude-code/haiku →
// claude-code-haiku).
func agentID(model string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(model)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return trimRunes(strings.Trim(b.String(), "-"), agentChars)
}

// cleanAgents checks a chosen list: a model each (the id derived from it when none is given), ids unique, at most 20,
// and the first a chat model (vboards before agents, search, and requests that name none use the first, with vboard's
// tools). An empty list is the default.
func cleanAgents(in []ConnectorAgent) ([]ConnectorAgent, error) {
	if len(in) > maxAgents {
		return nil, &connectorError{"agents", fmt.Sprintf("At most %d agents", maxAgents)}
	}
	out := make([]ConnectorAgent, 0, len(in))
	seen := map[string]bool{}
	for i, a := range in {
		a.Model = strings.TrimSpace(a.Model)
		if a.Model == "" || len(a.Model) > 200 || strings.ContainsAny(a.Model, " \t\r\n") {
			return nil, &connectorError{"agents", fmt.Sprintf("Agent %d: choose a model", i+1)}
		}
		if a.ID = strings.TrimSpace(a.ID); a.ID == "" {
			a.ID = agentID(a.Model)
		}
		ok := a.ID != "" && len([]rune(a.ID)) <= agentChars
		for _, r := range a.ID {
			ok = ok && !unicode.IsSpace(r) && unicode.IsPrint(r)
		}
		if !ok {
			return nil, &connectorError{"agents", fmt.Sprintf("Agent %d: an id of at most %d characters without spaces", i+1, agentChars)}
		}
		if seen[a.ID] {
			return nil, &connectorError{"agents", fmt.Sprintf("The id %q is there twice", a.ID)}
		}
		seen[a.ID] = true
		a.Name = trimRunes(strings.TrimSpace(a.Name), agentChars)
		a.Kind = agentKind(a.Model)
		if i == 0 && a.Kind != kindModel {
			return nil, &connectorError{"agents", "The first agent is the default, which older vboards and search ask with " +
				"vboard's tools: it must be a chat model, not " + a.Model}
		}
		out = append(out, a)
	}
	return out, nil
}

// agentList is what a connector offers: the chosen agents, else the chat model alone (models.chat or the server's
// default; kind model whatever it is: that is how it answered before there were agents).
func (c storedConnector) agentList() []ConnectorAgent {
	if len(c.Agents) > 0 {
		return c.Agents
	}
	return []ConnectorAgent{{ID: agentDefault, Model: c.Models.Chat, Kind: kindModel}}
}

func trimRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---------- endpoints ----------

func (s *Server) connectorAPI(auth *http.ServeMux) {
	auth.HandleFunc("GET /api/connectors", s.listConnectors)
	auth.HandleFunc("POST /api/connectors", s.addConnector)
	auth.HandleFunc("PATCH /api/connectors/{id}", s.patchConnector)
	auth.HandleFunc("DELETE /api/connectors/{id}", s.deleteConnector)
	auth.HandleFunc("GET /api/connectors/events", s.connectorEvents)
}

func writeConnectorError(w http.ResponseWriter, err error) {
	var ce *connectorError
	if errors.As(err, &ce) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": ce.field, "message": ce.message})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": err.Error()})
}

func (s *Server) listConnectors(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.connectors.views())
}

func (s *Server) addConnector(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind    string           `json:"kind"`
		Name    string           `json:"name"`
		URL     string           `json:"url"`
		Token   string           `json:"token"`
		Board   string           `json:"board"`
		Enabled *bool            `json:"enabled"`
		Models  *ConnectorModels `json:"models"`
		Agents  []ConnectorAgent `json:"agents"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	if body.Kind != "" && body.Kind != "vboard" {
		writeConnectorError(w, &connectorError{"kind", fmt.Sprintf("Unknown kind %q (vboard is the one there is)", body.Kind)})
		return
	}
	c := storedConnector{ID: randomString(9), Kind: "vboard", Name: cleanConnectorName(body.Name), Enabled: body.Enabled == nil || *body.Enabled,
		Owner: deviceOf(r).User, Created: time.Now().UTC().Truncate(time.Second)}
	var err error
	if c.URL, err = cleanConnectorURL(body.URL); err != nil {
		writeConnectorError(w, err)
		return
	}
	if c.Token, err = cleanConnectorToken(body.Token); err != nil {
		writeConnectorError(w, err)
		return
	}
	if c.Board, err = cleanBoard(body.Board); err != nil {
		writeConnectorError(w, err)
		return
	}
	if body.Models != nil {
		c.Models = cleanModels(*body.Models)
	}
	if c.Agents, err = cleanAgents(body.Agents); err != nil {
		writeConnectorError(w, err)
		return
	}
	m := s.connectors
	m.mu.Lock()
	e := &connectorEntry{conf: c}
	m.list = append(m.list, e)
	if err := m.saveLocked(); err != nil {
		m.list = m.list[:len(m.list)-1]
		m.mu.Unlock()
		writeConnectorError(w, err)
		return
	}
	m.startLocked(e)
	v := e.view()
	m.mu.Unlock()
	m.notify()
	log.Printf("connector %q (%s) made by %q of %q: %s", c.Name, c.ID, deviceOf(r).Name, c.Owner, c.URL)
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) patchConnector(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    *string `json:"name"`
		URL     *string `json:"url"`
		Token   *string `json:"token"`
		Board   *string `json:"board"`
		Enabled *bool   `json:"enabled"`
		Models  *struct {
			Classify *string `json:"classify"`
			Embed    *string `json:"embed"`
			Chat     *string `json:"chat"`
		} `json:"models"`
		Agents *[]ConnectorAgent `json:"agents"` // replaces the list ([] = the default)
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "message": err.Error()})
		return
	}
	m := s.connectors
	m.mu.Lock()
	var e *connectorEntry
	for _, x := range m.list {
		if x.conf.ID == r.PathValue("id") {
			e = x
		}
	}
	if e == nil {
		m.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	c := e.conf
	var err error
	if body.Name != nil {
		c.Name = cleanConnectorName(*body.Name)
	}
	if body.URL != nil {
		c.URL, err = cleanConnectorURL(*body.URL)
	}
	if err == nil && body.Token != nil && strings.TrimSpace(*body.Token) != "" { // blank: keep the stored one
		c.Token, err = cleanConnectorToken(*body.Token)
	}
	if err == nil && body.Board != nil {
		c.Board, err = cleanBoard(*body.Board)
	}
	if body.Enabled != nil {
		c.Enabled = *body.Enabled
	}
	if bm := body.Models; bm != nil {
		set := func(dst *string, v *string) {
			if v != nil {
				*dst = strings.TrimSpace(*v)
			}
		}
		set(&c.Models.Classify, bm.Classify)
		set(&c.Models.Embed, bm.Embed)
		set(&c.Models.Chat, bm.Chat)
	}
	if err == nil && body.Agents != nil {
		c.Agents, err = cleanAgents(*body.Agents)
	}
	if err != nil {
		m.mu.Unlock()
		writeConnectorError(w, err)
		return
	}
	before := e.conf
	e.conf = c
	if err := m.saveLocked(); err != nil {
		e.conf = before
		m.mu.Unlock()
		writeConnectorError(w, err)
		return
	}
	// changed settings: dial again with them (a connector that stopped on an error tries again on any edit)
	if !reflect.DeepEqual(c, before) || e.status.State == "error" {
		m.startLocked(e)
	}
	v := e.view()
	m.mu.Unlock()
	m.notify()
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) deleteConnector(w http.ResponseWriter, r *http.Request) {
	m := s.connectors
	m.mu.Lock()
	for i, e := range m.list {
		if e.conf.ID != r.PathValue("id") {
			continue
		}
		m.list = append(m.list[:i:i], m.list[i+1:]...)
		if err := m.saveLocked(); err != nil {
			m.list = append(m.list[:i:i], append([]*connectorEntry{e}, m.list[i:]...)...)
			m.mu.Unlock()
			writeConnectorError(w, err)
			return
		}
		if e.cancel != nil {
			e.cancel()
			e.cancel = nil
		}
		m.mu.Unlock()
		m.notify()
		log.Printf("connector %q (%s) deleted by %q", e.conf.Name, e.conf.ID, deviceOf(r).Name)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	m.mu.Unlock()
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

// connectorEvents streams the connectors: the whole list at once and on every change (a comment every 25 s keeps
// proxies from closing a quiet stream).
func (s *Server) connectorEvents(w http.ResponseWriter, r *http.Request) {
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
	var last []byte
	for {
		changed := s.connectors.Changed() // taken before the list, so no change slips between the two
		b, _ := json.Marshal(s.connectors.views())
		if !bytes.Equal(b, last) {
			last = b
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// Close stops the connectors (their connections to vboard and the requests running on them).
func (s *Server) Close() { s.connectors.Close() }
