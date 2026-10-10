package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/philipparndt/ai-connector/internal/agents"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/hub"
	"github.com/philipparndt/ai-connector/internal/models"
)

// The first start of `ai-connector vboard` (docs/api.md, "vboard: setting up models from vboard"): the small models
// vboard needs come by themselves, Claude Code is offered when it is installed and logged in here, and everything
// else is downloaded when the person clicks in vboard.

// downloads are this worker's downloads on request (the store and the sources as setup's).
func downloads(root string, cat *engine.Catalog) *engine.Downloads {
	srcs, _ := models.Sources(os.Getenv("MLXLAB_MODELS_FROM"), nil)
	return &engine.Downloads{Catalog: cat, Store: &models.Store{Dir: filepath.Join(root, "models"), Sources: srcs}, Root: root}
}

// firstModels are downloaded without asking when they are missing: the embedder and the classifier (about 1 GB
// together; vboard's "Group by theme" and "Sort into frames").
var firstModels = []string{"qwen3-embedding-0.6b", "laya-typed-decisions"}

// startFirstModels downloads the first models in the background; they are offered once they are here.
func startFirstModels(dl *engine.Downloads, mem int64) {
	for _, id := range firstModels {
		m := dl.Catalog.Find(id)
		if m == nil || m.Here {
			continue
		}
		if _, err := dl.Start(id, mem); err == nil {
			fmt.Printf("downloading %s (%s) in the background\n", m.Name, models.Human(m.Size))
		}
	}
}

// claudeAgentsDefault are offered when --agents was never given and Claude Code is ready here.
var claudeAgentsDefault = []string{"claude-code/sonnet", "claude-code/opus"}

// defaultAgents is the connector's agent list: --agents when given (vboardAgents), else — when Claude Code is
// installed and logged in here — the chat model followed by Claude Code (Sonnet, Opus); else the chat model alone.
func defaultAgents(list, chat string, claudeReady func() bool) ([]hub.ConnectorAgent, error) {
	if list != "" {
		return vboardAgents(list, chat)
	}
	if !claudeReady() {
		return nil, nil
	}
	out := []hub.ConnectorAgent{{ID: "default", Model: chat}} // "": the first chat model this computer has
	for _, m := range claudeAgentsDefault {
		out = append(out, hub.ConnectorAgent{Model: m})
	}
	return out, nil
}

// claudeReady: Claude Code is installed and logged in for the agents of this state dir.
func claudeReady(own string) bool {
	m, _ := agents.New(own)
	return m.Ready()
}
