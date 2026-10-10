package hub

// The hub inside `ai-connector vboard` (cmd/ai-connector/vboard.go): one process, no server of its own — the hub
// listens on a loopback port only, this machine is its one worker and it has one vboard connector. Nobody signs in
// (the password is random, the console only on request); the worker token is kept in the data directory, since the
// hub itself keeps only hashes.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

// Serve serves on a listener the caller opened (Run, or a free loopback port known before the worker dials it).
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second, // no Read/WriteTimeout: uploads and SSE take as long as they take
		BaseContext:       func(net.Listener) context.Context { return base },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	cancel()  // ends the event streams, which would otherwise hold Shutdown up
	s.Close() // the connectors' connections
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	return srv.Shutdown(sctx)
}

// standaloneFile keeps the local worker's token (the hub stores its hash only).
const standaloneFile = "standalone.json"

type standaloneState struct {
	WorkerToken string `json:"workerToken"`
}

// LocalWorker is the token of this machine as the standalone hub's worker: the one made before while the hub still
// knows it, else a new worker (name, owner) — the owner is the one of the connector, so Claude Code runs here.
func (s *Server) LocalWorker(name, owner string) (string, error) {
	file := filepath.Join(s.cfg.Data, standaloneFile)
	var st standaloneState
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.WorkerToken != "" {
		if w, ok := s.workers.lookup(st.WorkerToken); ok && w.Owner == owner {
			return st.WorkerToken, nil
		}
	}
	token, _, err := s.workers.add(name, owner)
	if err != nil {
		return "", err
	}
	st.WorkerToken = token
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(file, b, 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// ConnectorSettings are what `ai-connector vboard` sets on its one connector.
type ConnectorSettings struct {
	Name   string
	URL    string
	Token  string
	Board  string
	Models ConnectorModels
	Agents []ConnectorAgent
	Owner  string
}

// EnsureConnector makes the standalone hub's one vboard connector, or changes it to these settings (a running one
// takes the change over its open connection, as an edit in the console does). Other connectors are removed: the
// hub serves this one vboard.
func (s *Server) EnsureConnector(set ConnectorSettings) (Connector, error) {
	c := storedConnector{Kind: "vboard", Name: cleanConnectorName(set.Name), Enabled: true, Owner: set.Owner, Models: cleanModels(set.Models)}
	var err error
	if c.URL, err = cleanConnectorURL(set.URL); err != nil {
		return Connector{}, err
	}
	if c.Token, err = cleanConnectorToken(set.Token); err != nil {
		return Connector{}, err
	}
	if c.Board, err = cleanBoard(set.Board); err != nil {
		return Connector{}, err
	}
	if c.Agents, err = cleanAgents(set.Agents); err != nil {
		return Connector{}, err
	}
	m := s.connectors
	m.mu.Lock()
	var e *connectorEntry
	keep := m.list[:0]
	for _, x := range m.list {
		if e == nil && x.conf.Kind == "vboard" {
			e = x
			keep = append(keep, x)
			continue
		}
		if x.cancel != nil {
			x.cancel()
		}
	}
	m.list = keep
	if e == nil {
		c.ID, c.Created = randomString(9), time.Now().UTC().Truncate(time.Second)
		e = &connectorEntry{conf: c}
		m.list = append(m.list, e)
		if err := m.saveLocked(); err != nil {
			m.list = m.list[:len(m.list)-1]
			m.mu.Unlock()
			return Connector{}, err
		}
		m.startLocked(e)
	} else {
		c.ID, c.Created = e.conf.ID, e.conf.Created
		before := e.conf
		e.conf = c
		if err := m.saveLocked(); err != nil {
			e.conf = before
			m.mu.Unlock()
			return Connector{}, err
		}
		switch {
		case e.run == nil || e.status.State == "error" || !before.Enabled:
			m.startLocked(e)
		case !reflect.DeepEqual(c, before):
			e.run.reconfigure(c)
		}
	}
	v := e.view()
	m.mu.Unlock()
	m.notify()
	return v, nil
}

// Connectors is the connectors' state now; ConnectorsChanged fires on the next change (for a log of the state).
func (s *Server) Connectors() []Connector            { return s.connectors.views() }
func (s *Server) ConnectorsChanged() <-chan struct{} { return s.connectors.Changed() }

// ConnectorErrorField is the setting a connector error is about ("url", "token", "board", "agents", …), "" for
// other errors.
func ConnectorErrorField(err error) string {
	var ce *connectorError
	if errors.As(err, &ce) {
		return ce.field
	}
	return ""
}
