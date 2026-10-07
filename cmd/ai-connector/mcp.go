package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/philipparndt/ai-connector/internal/mcp"
	"github.com/philipparndt/ai-connector/internal/transit/mcpserver"
)

const mcpUsage = `usage:
  mlxlab mcp list                                  the MCP servers of mcp.json and their tools
  mlxlab mcp call [--yes] <server__tool> ['<json>'] call a tool (one that changes something needs --yes)
  mlxlab mcp serve transit [--http ADDR] [--print-config]
                                                   run the built-in timetable MCP server (stdio by default)

config: ` + "%s" + ` (mode 0600; docs/chat-tools.md, docs/mcp.example.json)
`

// mcpCommand is "mlxlab mcp": try the MCP servers a home chat may use.
func mcpCommand(args []string) error {
	usage := func() { fmt.Fprintf(os.Stderr, mcpUsage, mcp.ConfigPath()) }
	if len(args) == 0 {
		usage()
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch args[0] {
	case "list":
		return mcpList(ctx)
	case "call":
		fs := flag.NewFlagSet("mlxlab mcp call", flag.ContinueOnError)
		yes := fs.Bool("yes", false, "allow a tool that changes something")
		fs.Usage = usage
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		rest := fs.Args()
		if len(rest) < 1 || len(rest) > 2 {
			usage()
			return errors.New("mcp call: <server__tool> ['<json args>']")
		}
		argJSON := "{}"
		if len(rest) == 2 {
			argJSON = rest[1]
		}
		return mcpCall(ctx, rest[0], argJSON, *yes)
	case "serve":
		return mcpServe(ctx, args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	}
	usage()
	return fmt.Errorf("mcp: unknown command %q", args[0])
}

// mcpServe is "mlxlab mcp serve <server>": an MCP server built into mlxlab, for mcp.json to start as a command.
// Only stdio JSON-RPC goes to stdout; logs go to stderr.
func mcpServe(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "transit" {
		return errors.New("mcp serve: the built-in server is \"transit\" (mlxlab mcp serve transit [--http ADDR] [--print-config])")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if abs, err := filepath.EvalSymlinks(exe); err == nil {
		exe = abs
	}
	return mcpserver.Main(ctx, args[1:], []string{exe, "mcp", "serve", "transit"}, os.Stdin, os.Stdout, os.Stderr)
}

func loadMCP() (*mcp.Config, error) {
	cfg, err := mcp.LoadConfig(mcp.ConfigPath())
	if err != nil {
		return nil, err
	}
	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("no MCP servers: %s is missing or empty (see docs/mcp.example.json)", mcp.ConfigPath())
	}
	return cfg, nil
}

func mcpList(ctx context.Context) error {
	cfg, err := loadMCP()
	if err != nil {
		return err
	}
	for name, s := range cfg.Servers {
		if s.Disabled {
			fmt.Printf("%s: disabled\n\n", name)
		}
	}
	boxes, errs := mcp.OpenAll(ctx, cfg)
	for _, b := range boxes {
		c := b.Client()
		name, version, proto, kind := c.Info()
		fmt.Printf("%s: %s %s (MCP %s over %s)\n", c.Server().Name, name, version, proto, kind)
		tools, err := b.Tools(ctx)
		if err != nil {
			fmt.Println("  ", err)
			continue
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, t := range tools {
			kind := "read-only"
			if t.Changes {
				kind = "CHANGES (approval)"
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\n", t.Name, kind, firstLine(t.Description, 70))
		}
		w.Flush()
		fmt.Println()
		b.Close()
	}
	for _, err := range errs {
		fmt.Fprintln(os.Stderr, "mlxlab mcp:", err)
	}
	if len(boxes) == 0 && len(errs) > 0 {
		return errors.New("no server answered")
	}
	return nil
}

func mcpCall(ctx context.Context, name, argJSON string, yes bool) error {
	server, _, ok := strings.Cut(name, mcp.Sep)
	if !ok {
		return fmt.Errorf("%q: the name is <server>%s<tool> (see mlxlab mcp list)", name, mcp.Sep)
	}
	if !json.Valid([]byte(argJSON)) {
		return fmt.Errorf("the arguments are not JSON: %s", argJSON)
	}
	cfg, err := loadMCP()
	if err != nil {
		return err
	}
	s := cfg.Servers[server]
	if s == nil {
		return fmt.Errorf("no server %q in %s", server, mcp.ConfigPath())
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	b, err := mcp.Open(cctx, s, mcp.Options{Stderr: os.Stderr})
	if err != nil {
		return err
	}
	defer b.Close()
	tools, err := b.Tools(cctx)
	if err != nil {
		return err
	}
	found, changes := false, false
	for _, t := range tools {
		if t.Name == name {
			found, changes = true, t.Changes
		}
	}
	if !found {
		return fmt.Errorf("%s has no tool %q (see mlxlab mcp list)", server, strings.TrimPrefix(name, server+mcp.Sep))
	}
	if changes && !yes {
		return fmt.Errorf("%s may change something (not marked read-only): add --yes to call it", name)
	}
	res, err := b.Call(cctx, name, json.RawMessage(argJSON))
	if err != nil {
		return err
	}
	fmt.Println(res.Text)
	if res.IsError {
		return errors.New("the tool reported an error")
	}
	return nil
}

func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
