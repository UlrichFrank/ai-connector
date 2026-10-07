// transit-mcp is an MCP server for public transport timetables (Deutsche Bahn and EFA-BW: naldo, VVS): the
// read-only tools find_stop, departures, journeys and trip (internal/transit, docs/chat-tools.md).
//
//	transit-mcp                  MCP over stdio
//	transit-mcp --http :8790     MCP over streamable HTTP (127.0.0.1:8790/mcp)
//	transit-mcp --print-config   an mcp.json snippet that runs this binary
//
// "mlxlab mcp serve transit" runs the same server.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/philipparndt/ai-connector/internal/transit/mcpserver"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	if abs, err := filepath.EvalSymlinks(self); err == nil {
		self = abs
	}
	if err := mcpserver.Main(ctx, os.Args[1:], []string{self}, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "transit-mcp:", err)
		os.Exit(1)
	}
}
