package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/philipparndt/ai-connector/internal/agents"
	"github.com/philipparndt/ai-connector/internal/engine"
)

// claudeAgents are this worker's Claude Code sessions (internal/agents), served by the web API (nil before setup).
var claudeAgents *agents.Manager

// claudeBackend sets up the Claude Code agents: the claude-code/* models while claude is on the PATH (asked again
// every minute), and the idle sessions closed in the background (agents.json: idleMinutes).
func claudeBackend(root string, cat *engine.Catalog) engine.Backend {
	m, err := agents.New(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mlxlab: agents.json:", err)
	}
	claudeAgents = m
	b := &engine.ClaudeCodeBackend{Agents: m, Catalog: cat}
	b.Discover()
	go b.Watch(context.Background(), time.Minute)
	go m.Run(context.Background())
	return b
}
