package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/agents"
	"github.com/philipparndt/ai-connector/internal/llm"
)

// ClaudeCodeBackend chats with Claude Code running on this worker (internal/agents): a chat job is one turn in a
// long-running session — the job's Session (the conversation key) picks it, and it stays warm between turns. It
// uses the subscription of whoever is logged in to Claude Code here, so the hub sends a person's jobs only to
// workers they own. Its jobs run beside the queue (Parallel): they use no memory of this machine worth queueing
// for, and a turn can take minutes of tool use.
type ClaudeCodeBackend struct {
	Agents  *agents.Manager
	Catalog *Catalog
}

// BackendClaudeCode is the backend name of the claude-code/* models.
const BackendClaudeCode = "claude-code"

// MCPServer is an MCP server a Claude Code session may use (Request.MCP).
type MCPServer = agents.MCPServer

// Parallel is a backend whose jobs start at once, beside the queue (jobs.Manager runs every other job one at a
// time).
type Parallel interface {
	Parallel() bool
}

// claudeModels are the models offered while Claude Code is installed (the aliases claude --model takes).
var claudeModels = []struct{ alias, name, about string }{
	{"sonnet", "Claude Code · Sonnet", "the everyday model"},
	{"opus", "Claude Code · Opus", "the most capable model"},
	{"haiku", "Claude Code · Haiku", "the fastest model"},
}

func (b *ClaudeCodeBackend) Name() string   { return BackendClaudeCode }
func (b *ClaudeCodeBackend) Heavy() bool    { return false }
func (b *ClaudeCodeBackend) Loaded() string { return "" }
func (b *ClaudeCodeBackend) Unload()        {}
func (b *ClaudeCodeBackend) Parallel() bool { return true }

// Discover lists the claude-code models when claude is on the PATH (none otherwise).
func (b *ClaudeCodeBackend) Discover() []*Model {
	var ms []*Model
	if v, ok := b.Agents.Available(); ok {
		for _, c := range claudeModels {
			ms = append(ms, &Model{ID: BackendClaudeCode + "/" + c.alias, Name: c.name, Kind: Chat, Backend: BackendClaudeCode,
				About: "Claude Code (" + v + ") on this worker, " + c.about + ": an agent with its own tools and folder, " +
					"on the subscription logged in here. A conversation (session) stays running between turns.",
				Here: true, Where: "claude " + v})
		}
	}
	b.Catalog.SetRuntime(BackendClaudeCode, ms)
	return ms
}

// Watch discovers again every interval (Claude Code installed while the worker runs) until ctx ends.
func (b *ClaudeCodeBackend) Watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.Discover()
		}
	}
}

func (b *ClaudeCodeBackend) Run(ctx context.Context, m *Model, r Request, _ string, emit Emit) error {
	alias, ok := strings.CutPrefix(m.ID, BackendClaudeCode+"/")
	if !ok || m.Kind != Chat {
		return fmt.Errorf("%s: not a Claude Code model", m.ID)
	}
	if r.Zone != "" {
		return fmt.Errorf("Claude Code brings its own tools: no zone (%s)", r.Zone)
	}
	prompt, system := claudePrompt(r)
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("no message")
	}
	emit.Status("starting Claude Code …")
	tools := map[string]map[string]any{}
	res, err := b.Agents.Turn(ctx, agents.Options{Key: r.Session, Owner: r.Owner, Model: alias, Driver: r.Driver, MCP: r.MCP,
		System: system}, prompt, func(e agents.Event) {
		switch e.Type {
		case "text":
			emit(Event{Type: "text", Text: e.Text})
		case "status":
			emit.Status(e.Text)
		case "tool":
			// a job's tools are upserted by id: the end of a call keeps what its start said
			t := tools[e.Tool.ID]
			if t == nil {
				t = map[string]any{"id": e.Tool.ID, "zone": ""}
				tools[e.Tool.ID] = t
			}
			t["state"] = e.Tool.State
			if e.Tool.Name != "" {
				t["name"] = e.Tool.Name
			}
			if len(e.Tool.Arguments) > 0 {
				t["arguments"] = e.Tool.Arguments
			}
			if e.Tool.Summary != "" {
				t["summary"] = e.Tool.Summary
			}
			if e.Tool.Result != "" {
				t["result"] = e.Tool.Result
			}
			c := make(map[string]any, len(t))
			for k, v := range t {
				c[k] = v
			}
			emit(Event{Type: "tool", Data: c})
		}
	})
	st := map[string]any{"session": res.Session, "driver": res.Driver, "claudeSession": res.ClaudeSession,
		"seconds": res.Duration.Seconds(), "turns": res.Turns}
	if res.Driver == agents.DriverJSON {
		st["promptTokens"] = res.InputTokens + res.CacheReadTokens + res.CacheWriteTokens
		st["tokens"] = res.OutputTokens
		st["cacheReadTokens"] = res.CacheReadTokens
		st["costUSD"] = res.CostUSD
		if res.FirstText > 0 {
			st["firstTokenSeconds"] = res.FirstText.Seconds()
		}
	}
	if res.Session != "" {
		emit(Event{Type: "stats", Data: st})
		usage := "" // the tty driver does not see the usage
		if res.Driver == agents.DriverJSON {
			usage = fmt.Sprintf("%d+%d tokens, $%.4f, ", res.InputTokens+res.CacheReadTokens+res.CacheWriteTokens, res.OutputTokens, res.CostUSD)
		}
		emit.Log(fmt.Sprintf("claude %s (%s, session %s): %s%.1f s", alias, res.Driver, res.Session, usage, res.Duration.Seconds()))
	}
	return err
}

// claudePrompt is what a turn sends: the last user message. A session keeps its own history, so the earlier
// messages are not sent again; a one-off turn (no session) gets them as a transcript in front. System messages
// become the session's appended system prompt (read when it starts).
func claudePrompt(r Request) (prompt, system string) {
	var sys []string
	var turns []llm.Message
	for _, m := range r.Messages {
		if m.Role == "system" {
			sys = append(sys, m.Content)
		} else if m.Role == "user" || m.Role == "assistant" {
			turns = append(turns, m)
		}
	}
	system = strings.Join(sys, "\n\n")
	if len(turns) == 0 {
		return r.Prompt, system
	}
	last := len(turns) - 1
	for last >= 0 && turns[last].Role != "user" {
		last--
	}
	if last < 0 {
		return "", system
	}
	prompt = turns[last].Content
	if r.Session == "" && last > 0 {
		var b strings.Builder
		b.WriteString("The conversation so far:\n\n")
		for _, m := range turns[:last] {
			fmt.Fprintf(&b, "%s: %s\n\n", strings.ToUpper(m.Role[:1])+m.Role[1:], m.Content)
		}
		b.WriteString("Now answer this message:\n\n")
		b.WriteString(prompt)
		prompt = b.String()
	}
	return prompt, system
}
