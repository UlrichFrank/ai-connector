package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/philipparndt/ai-connector/internal/agent"
	"github.com/philipparndt/ai-connector/internal/engine"
)

// Toolbox offers the tools of one MCP server to the agent loop. Every MCP server is in the home zone; its tools
// are named <server>__<tool> and change something (need approval) unless the server marks them readOnlyHint or the
// config lists them in readOnly.
type Toolbox struct {
	c   *Client
	log *os.File // a stdio server's stderr

	mu    sync.Mutex
	tools []agent.Tool
}

var _ agent.Toolbox = (*Toolbox)(nil)

// Open connects to a server.
func Open(ctx context.Context, s *Server, o Options) (*Toolbox, error) {
	c, err := Connect(ctx, s, o)
	if err != nil {
		return nil, err
	}
	return &Toolbox{c: c}, nil
}

// Client is the connection to the server.
func (b *Toolbox) Client() *Client { return b.c }

// Tools lists the server's tools (once; later calls give the same list).
func (b *Toolbox) Tools(ctx context.Context) ([]agent.Tool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tools != nil {
		return b.tools, nil
	}
	infos, err := b.c.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", b.c.srv.Name, err)
	}
	tools := make([]agent.Tool, 0, len(infos))
	for _, t := range infos {
		tools = append(tools, b.toTool(t))
	}
	b.tools = tools
	return tools, nil
}

// toTool converts a listed tool.
func (b *Toolbox) toTool(t ToolInfo) agent.Tool {
	s := b.c.srv
	params := t.InputSchema
	if len(params) == 0 || string(params) == "null" {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	desc := t.Description
	if desc == "" {
		desc = t.Title
	}
	return agent.Tool{
		Name:        s.Name + Sep + t.Name,
		Description: desc,
		Parameters:  params,
		Zone:        agent.ZoneHome,
		Changes:     !(t.ReadOnly() || s.IsReadOnly(t.Name)),
		Untrusted:   false,
	}
}

// Call runs a tool. name is the prefixed name (vboard__search_cards). An error the server reports (isError, or a
// JSON-RPC error such as unknown tool or bad arguments) is a Result with IsError for the model; the error return
// is for a connection that failed.
func (b *Toolbox) Call(ctx context.Context, name string, args json.RawMessage) (agent.Result, error) {
	tool, ok := strings.CutPrefix(name, b.c.srv.Name+Sep)
	if !ok {
		return agent.Result{}, fmt.Errorf("tool %q is not one of %s", name, b.c.srv.Name)
	}
	res, err := b.c.CallTool(ctx, tool, args)
	var rpc *RPCError
	if errors.As(err, &rpc) {
		return agent.Result{Text: rpc.Error(), IsError: true}, nil
	}
	if err != nil {
		return agent.Result{}, fmt.Errorf("%s: %w", name, err)
	}
	return agent.Result{Text: res.Text(), IsError: res.IsError}, nil
}

// Close ends the connection.
func (b *Toolbox) Close() error {
	err := b.c.Close()
	if b.log != nil {
		_ = b.log.Close()
	}
	return err
}

// LogPath is where a stdio server's stderr goes.
func LogPath(server string) string {
	return filepath.Join(engine.ConfigDir(), "logs", "mcp-"+server+".log")
}

// Toolboxes connects to every enabled server of mcp.json (in parallel) and lists their tools. A server that fails
// is left out and its error is in the returned error (joined); the others are usable. No mcp.json: no toolboxes.
func Toolboxes(ctx context.Context) ([]agent.Toolbox, error) {
	cfg, err := LoadConfig(ConfigPath())
	if err != nil {
		return nil, err
	}
	boxes, errs := OpenAll(ctx, cfg)
	out := make([]agent.Toolbox, 0, len(boxes))
	for _, b := range boxes {
		out = append(out, b)
	}
	return out, errors.Join(errs...)
}

// OpenAll opens the enabled servers of cfg and lists their tools; the result keeps the order of cfg.Enabled.
func OpenAll(ctx context.Context, cfg *Config) ([]*Toolbox, []error) {
	servers := cfg.Enabled()
	boxes := make([]*Toolbox, len(servers))
	errs := make([]error, len(servers))
	var wg sync.WaitGroup
	for i, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := Options{}
			var log *os.File
			if s.transport() == "stdio" {
				if log = openLog(s.Name); log != nil {
					o.Stderr = log
				}
			}
			b, err := Open(ctx, s, o)
			if err != nil {
				if log != nil {
					log.Close()
				}
				errs[i] = err
				return
			}
			b.log = log
			if _, err := b.Tools(ctx); err != nil {
				_ = b.Close()
				errs[i] = err
				return
			}
			boxes[i] = b
		}()
	}
	wg.Wait()
	var ok []*Toolbox
	var bad []error
	for i := range servers {
		if boxes[i] != nil {
			ok = append(ok, boxes[i])
		}
		if errs[i] != nil {
			bad = append(bad, errs[i])
		}
	}
	return ok, bad
}

// openLog opens the stderr log of a stdio server (appending; nil if it cannot be opened).
func openLog(server string) *os.File {
	p := LogPath(server)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}
