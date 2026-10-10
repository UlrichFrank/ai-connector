// Package agents runs Claude Code on a worker as long-running agents (docs/api.md, "Agents (Claude Code)"): one
// session per conversation, kept warm between turns and closed after an idle timeout. A session is driven in one
// of two ways:
//
//   - json (the default, for the APIs): `claude -p --input-format stream-json --output-format stream-json …` stays
//     running; each turn is a line on its stdin, its answer streams back as events (text deltas, tool calls, the
//     result with usage and cost).
//   - tty: an interactive `claude` in a pseudo terminal that a person can attach to from the browser (a WebSocket
//     carrying the terminal's bytes). Turns from the APIs are typed into it; their answers come from Claude Code's
//     hooks (a Stop hook in the session's own --settings file), so there is no token streaming.
//
// Claude Code runs with the subscription of whoever is logged in on the worker; the hub sends a person's agent
// jobs only to workers they own. Each session works in its own folder under <config>/agents/<session id>/, with
// only the tools of the worker's allow-list (agents.json) and only the MCP servers its job asks for.
package agents

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Config is the worker's agents.json (<config dir>/agents.json, e.g. ~/.config/mlxlab/agents.json).
type Config struct {
	// AllowedTools are the Claude Code tools a session may use without asking (--allowedTools). Empty: the
	// read-mostly DefaultAllowedTools. The tools of the session's own MCP servers (mcp__<name>) are always added.
	AllowedTools []string `json:"allowedTools,omitempty"`
	// Model is the model of a session whose job names none ("sonnet").
	Model string `json:"model,omitempty"`
	// IdleMinutes: a session nobody used for this long is closed (default 30).
	IdleMinutes int `json:"idleMinutes,omitempty"`
	// MaxSessions: at most this many sessions run at once (default 4); a new one closes the one idle longest.
	MaxSessions int `json:"maxSessions,omitempty"`
	// AutoTrust: a tty session answers Claude Code's "do you trust this folder?" question for its own new folder
	// with yes (default true; the json driver never asks).
	AutoTrust *bool `json:"autoTrust,omitempty"`
	// Folders are the absolute folders (and everything inside them) a person's own tty session may work in
	// (POST /api/agents/sessions "cwd"), besides the agents folder.
	Folders []string `json:"folders,omitempty"`
}

// DefaultAllowedTools: reading and searching the session's own folder, and the web; nothing that changes files or
// runs commands. Rules with a path ("./**") are relative to the session's folder.
var DefaultAllowedTools = []string{"Read(./**)", "Glob(./**)", "Grep(./**)", "WebSearch", "WebFetch", "TodoWrite"}

// Defaults.
const (
	DefaultModel       = "sonnet"
	DefaultIdleMinutes = 30
	DefaultMaxSessions = 4
)

// LoadConfig reads <dir>/agents.json; a missing file gives the defaults.
func LoadConfig(dir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, "agents.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c.withDefaults(), err
	}
	if err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			return Config{}.withDefaults(), err
		}
	}
	return c.withDefaults(), nil
}

func (c Config) withDefaults() Config {
	if len(c.AllowedTools) == 0 {
		c.AllowedTools = append([]string(nil), DefaultAllowedTools...)
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.IdleMinutes <= 0 {
		c.IdleMinutes = DefaultIdleMinutes
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = DefaultMaxSessions
	}
	if c.AutoTrust == nil {
		yes := true
		c.AutoTrust = &yes
	}
	return c
}

// Idle is the idle timeout.
func (c Config) Idle() time.Duration { return time.Duration(c.IdleMinutes) * time.Minute }
