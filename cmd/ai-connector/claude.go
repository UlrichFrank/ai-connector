package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/philipparndt/ai-connector/internal/agents"
	"github.com/philipparndt/ai-connector/internal/engine"
)

// claudeAgents are this worker's Claude Code sessions (internal/agents), served by the web API (nil before setup).
var claudeAgents *agents.Manager

// agentsCommand is `mlxlab agents login|status [--state DIR]`: Claude Code's login as the worker's agent sessions
// see it (their own config folder, the user's credentials; internal/agents/home.go). `login` runs
// `claude auth login` that way, `status` `claude auth status --text`.
func agentsCommand(args []string) error {
	const use = "usage: mlxlab agents login|status [--state DIR]   Claude Code's login for this worker's agents\n"
	if len(args) == 0 || (args[0] != "login" && args[0] != "status") {
		fmt.Fprint(os.Stderr, use)
		return nil
	}
	fs := flag.NewFlagSet("mlxlab agents "+args[0], flag.ContinueOnError)
	state := fs.String("state", "", "the worker's state dir (default: the config dir; $MLXLAB_STATE)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	root := engine.ConfigDir()
	m, err := agents.New(stateDir(*state, os.Getenv, root))
	if err != nil {
		fmt.Fprintln(os.Stderr, "mlxlab: agents.json:", err)
	}
	if _, ok := m.Available(); !ok {
		return agents.ErrUnavailable
	}
	cmd := m.AuthCommand("login")
	if args[0] == "status" {
		cmd = m.AuthCommand("status", "--text")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if home := m.ClaudeHome(); home != "" {
		fmt.Fprintln(os.Stderr, "Claude Code for the agents: config", home, "with the login of your own Claude Code")
	}
	return cmd.Run()
}

// claudeBackend sets up the Claude Code agents: the claude-code/* models while claude is on the PATH (asked again
// every minute), and the idle sessions closed in the background (agents.json: idleMinutes).
func claudeBackend(root, own string, cat *engine.Catalog) engine.Backend {
	m, err := agents.New(own)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mlxlab: agents.json:", err)
	}
	if own != root {
		m.Shared = []string{root} // the config dir beside this worker's own: no session reads it either
	}
	claudeAgents = m
	b := &engine.ClaudeCodeBackend{Agents: m, Catalog: cat}
	b.Discover()
	go b.Watch(context.Background(), time.Minute)
	go m.Run(context.Background())
	return b
}
