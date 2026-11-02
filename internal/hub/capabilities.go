package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// What a token can run now (docs/api.md "Capabilities"): the models a connected worker has on disk — nothing is
// downloaded for them — on the workers the token may use: any for most models, for Claude Code only the worker of
// the person it acts for (the device's user, the client token's owner; agents.go). An application learns from it what
// to offer, and from the event stream when that changes.

// capsEvery: the capabilities event stream checks this often besides on every change of the fleet.
var capsEvery = 30 * time.Second

type capabilities struct {
	// User is the person the token acts for ("" none: no Claude Code)
	User   string     `json:"user"`
	Models []capModel `json:"models"`
}

type capModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Backend       string   `json:"backend"`
	Images        bool     `json:"images,omitempty"` // a chat model that takes images
	Tasks         []string `json:"tasks,omitempty"`
	NonCommercial bool     `json:"nonCommercial,omitempty"`
	// Workers: where it would run (has it on disk), the one a job would go to now first
	Workers []capWorker `json:"workers"`
	// Generator: what an image, video, upscale, cutout, music, sound or speech model takes
	Generator map[string]any `json:"generator,omitempty"`
}

type capWorker struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// capabilitiesOf is what the request's token can run on the routed workers now.
func (s *Server) capabilitiesOf(r *http.Request, routed []*node) capabilities {
	out := capabilities{User: userOf(r), Models: []capModel{}}
	var claude *node // the person's worker for Claude Code (nil: none connected, or no person)
	if out.User != "" && len(routed) > 0 {
		claude, _ = s.claudeNode(out.User, routed, "", viaOf(r))
	}
	for _, fm := range fleetModels(routed) {
		var here []*node
		for _, n := range fm.workers {
			if !hasOnDisk(n, fm.ID) {
				continue
			}
			if isClaudeModel(fm.catalogModel) && n != claude {
				continue
			}
			here = append(here, n)
		}
		if len(here) == 0 {
			continue
		}
		if n, ok := choose(here, s.fleet.primary(routed), fm.catalogModel, true, nil); ok {
			here = append([]*node{n}, without(here, n)...)
		}
		m := capModel{ID: fm.ID, Name: fm.Name, Kind: fm.Kind, Backend: fm.Backend, Images: fm.Images, Tasks: fm.Tasks,
			NonCommercial: fm.NonCommercial}
		if m.Name == "" {
			m.Name = m.ID
		}
		for _, n := range here {
			m.Workers = append(m.Workers, capWorker{n.id, n.name})
		}
		if g, ok := generatorOf(fm.catalogModel); ok {
			g.ID, g.Name, g.Worker, g.Model = fm.ID, m.Name, here[0].name, fm.ID
			m.Generator = generatorsJSON([]generatorInfo{g})[0]
			delete(m.Generator, "id") // said by the model
			delete(m.Generator, "name")
			delete(m.Generator, "worker")
			delete(m.Generator, "nonCommercial")
		}
		out.Models = append(out.Models, m)
	}
	return out
}

func isClaudeModel(m catalogModel) bool {
	return m.Backend == "claude-code" || strings.HasPrefix(m.ID, claudePrefix)
}

// hasOnDisk: the worker lists the model as on its disk.
func hasOnDisk(n *node, id string) bool {
	_, _, _, models := n.snapshot()
	for _, m := range models {
		if m.ID == id {
			return m.Here
		}
	}
	return false
}

// getCapabilities: GET /api/capabilities (client or device token).
func (s *Server) getCapabilities(w http.ResponseWriter, r *http.Request) {
	routed, _ := s.fleet.pick(r.Context())
	writeJSON(w, http.StatusOK, s.capabilitiesOf(r, routed))
}

// capabilityEvents: GET /api/capabilities/events — the capabilities at once and whenever they change (SSE), a
// ping every 15 s.
func (s *Server) capabilityEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	var last []byte
	send := func() {
		routed, _ := s.fleet.pick(r.Context())
		b, _ := json.Marshal(s.capabilitiesOf(r, routed))
		if bytes.Equal(b, last) {
			return
		}
		last = b
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
	changed := s.fleet.Changed()
	send()
	check := time.NewTicker(capsEvery)
	defer check.Stop()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changed:
			changed = s.fleet.Changed()
			send()
		case <-check.C:
			send()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}
