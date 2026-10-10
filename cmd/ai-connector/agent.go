package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/agent"
	"github.com/philipparndt/ai-connector/internal/approvals"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/guard"
	"github.com/philipparndt/ai-connector/internal/mcp"
	"github.com/philipparndt/ai-connector/internal/sandbox/files"
	"github.com/philipparndt/ai-connector/internal/sandbox/web"
	webapi "github.com/philipparndt/ai-connector/internal/web"
)

// agentConfig is what chats with tools work with (internal/agent). Each zone's toolboxes come from a hook; a hook
// left nil means that zone answers "not available on this Mac yet":
//
//	Home   internal/mcp: one agent.Toolbox per server of ~/.config/mlxlab/mcp.json
//	Web    internal/sandbox/web: web_search, web_fetch in the sandbox VM
//	Files  internal/sandbox/files: make_file in the offline sandbox VM
//	Guard  internal/guard: the Laya classifier (screens web results, grades answers)
//	Always internal/approvals: the home tools allowed always (no approval card)
func agentConfig(root string, text *engine.TextBackend, always *approvals.Store) *agent.Config {
	out := filepath.Join(root, "outputs")
	var g agent.Guard // nil: the chat model judges (internal/engine/chat.go)
	if os.Getenv("MLXLAB_GUARD") == "laya" {
		g = guard.New(text, guard.Config{}) // faster, but missed most new injections in the eval
	}
	return &agent.Config{
		Guard:  g,
		Home:   mcp.Toolboxes,
		Always: always.Has, // home tools the user allowed always run without asking
		Files: func(context.Context) (agent.Toolbox, error) {
			return files.New(files.Options{OutDir: out})
		},
		Web: webSandbox,
	}
}

// The web sandbox's VMs are slow to start, so one sandbox serves every chat (it stops itself when idle); the loop
// closes its toolboxes after each turn, which must not stop the VMs.
var (
	webOnce sync.Once
	webBox  *web.Sandbox
	webErr  error
)

func webSandbox(context.Context) (agent.Toolbox, error) {
	webOnce.Do(func() {
		key, err := web.LoadBraveKey()
		if err != nil {
			webErr = fmt.Errorf("the Brave key: %w", err)
			return
		}
		deny, err := web.LoadDeny()
		if err != nil {
			webErr = fmt.Errorf("the web deny list: %w", err)
			return
		}
		webBox = web.New(web.Config{BraveKey: key, Deny: deny})
	})
	if webErr != nil {
		return nil, webErr
	}
	return shared{webBox}, nil
}

// shared is a toolbox whose Close leaves the sandbox running.
type shared struct{ agent.Toolbox }

func (shared) Close() error { return nil }

// closeSandboxes stops the web sandbox's VMs when mlxlab quits (they would keep running otherwise).
func closeSandboxes() {
	if webBox != nil {
		_ = webBox.Close()
	}
}

// homeTools lists the home zone's tools for the web API (Settings → Tools, checking a tool before it is allowed
// always): every enabled MCP server of mcp.json, in parallel; a server that does not answer is listed with its error.
func homeTools(ctx context.Context) ([]webapi.ToolServer, error) {
	cfg, err := mcp.LoadConfig(mcp.ConfigPath())
	if err != nil {
		return nil, err
	}
	servers := cfg.Enabled()
	out := make([]webapi.ToolServer, len(servers))
	var wg sync.WaitGroup
	for i, s := range servers {
		out[i].Name = s.Name
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			b, err := mcp.Open(cctx, s, mcp.Options{})
			if err != nil {
				out[i].Error = err.Error()
				return
			}
			defer b.Close()
			tools, err := b.Tools(cctx)
			if err != nil {
				out[i].Error = err.Error()
				return
			}
			out[i].Tools = tools
		}()
	}
	wg.Wait()
	return out, nil
}
