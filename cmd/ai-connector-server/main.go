// mlxlab-hub is the public side of mlxlab: the web UI, sign-in, and the proxy to the worker on the Mac.
// See docs/architecture.md. Configured from the environment (MLXLAB_PASSWORD, MLXLAB_WORKER_TOKEN (optional,
// the legacy shared worker token), MLXLAB_DATA, MLXLAB_ADDR, MLXLAB_TOOLS,
// MLXLAB_CONSOLE_HOSTS, MLXLAB_PUBLIC_URL, MLXLAB_MDNS, MLXLAB_MDNS_NAME).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/philipparndt/ai-connector/internal/consoleui"
	"github.com/philipparndt/ai-connector/internal/hub"
	"github.com/philipparndt/ai-connector/internal/webui"
)

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	cfg := hub.FromEnv()
	if len(os.Args) > 1 && os.Args[1] == "avahi-service" {
		writeAvahiService(cfg, os.Args[2:])
		return
	}
	s, err := hub.New(cfg)
	if err != nil {
		log.Fatalf("mlxlab-hub: %v", err)
	}
	log.Printf("mlxlab-hub %s", version)
	if cfg.WorkerToken == "" {
		log.Printf("no MLXLAB_WORKER_TOKEN: only Macs connected in the web UI (Devices → Connect a Mac) get in")
	}
	for _, p := range hub.Platforms {
		if _, err := os.Stat(filepath.Join(cfg.Tools, "mlxlab", p)); err != nil {
			log.Printf("no worker program for %s in %s (MLXLAB_TOOLS): /tools/mlxlab/%s answers 404", p, cfg.Tools, p)
		}
	}
	if !webui.Built() {
		log.Printf("the web app is not built into this binary (make web); only the API is served")
	}
	if !consoleui.Built() {
		log.Printf("the console is not built into this binary (make console): /console/ answers 404")
	}
	if len(cfg.ConsoleHosts) > 0 {
		log.Printf("console hosts (/ → /console/): %s", strings.Join(cfg.ConsoleHosts, ", "))
	}
	if cfg.PublicURL != "" {
		log.Printf("public URL (link PINs, Bonjour): %s", cfg.PublicURL)
	}
	if !cfg.MDNS {
		log.Printf("bonjour off (MLXLAB_MDNS)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go logWorker(ctx, s)
	log.Printf("mlxlab-hub listening on %s, data in %s", cfg.Addr, cfg.Data)
	if err := s.Run(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("mlxlab-hub: %v", err)
	}
	log.Printf("mlxlab-hub stopped")
}

// writeAvahiService is `ai-connector-server avahi-service <file> [port]`: it writes the Bonjour announcement as an
// avahi static service file (MLXLAB_MDNS_NAME, MLXLAB_PUBLIC_URL; port default 443) and exits. The chart's init
// container runs it into the node's /etc/avahi/services (mdns.avahi).
func writeAvahiService(cfg hub.Config, args []string) {
	if len(args) < 1 || len(args) > 2 {
		log.Fatalf("usage: ai-connector-server avahi-service <file> [port]")
	}
	port := 443
	if len(args) == 2 {
		if _, err := fmt.Sscanf(args[1], "%d", &port); err != nil || port <= 0 || port > 65535 {
			log.Fatalf("avahi-service: bad port %q", args[1])
		}
	}
	if cfg.PublicURL == "" {
		log.Printf("avahi-service: no MLXLAB_PUBLIC_URL, the record carries no url (apps then use the node's address)")
	}
	// write next to the file and rename: avahi watches the directory and must never read half a file
	tmp := args[0] + ".tmp"
	if err := os.WriteFile(tmp, hub.AvahiService(cfg.MDNSName, cfg.PublicURL, port), 0o644); err != nil {
		log.Fatalf("avahi-service: %v", err)
	}
	if err := os.Rename(tmp, args[0]); err != nil {
		os.Remove(tmp)
		log.Fatalf("avahi-service: %v", err)
	}
	log.Printf("avahi-service: wrote %s", args[0])
}

// logWorker prints which Macs are connected, at start and on every change.
func logWorker(ctx context.Context, s *hub.Server) {
	t := s.Tunnel()
	for {
		changed := t.Changed()
		if conns := t.Connected(); len(conns) > 0 {
			names := make([]string, len(conns))
			for i, c := range conns {
				names[i] = fmt.Sprintf("%s (%s, since %s)", c.Name, c.ID, c.Since.Format("15:04:05"))
			}
			log.Printf("workers: %s", strings.Join(names, ", "))
		} else {
			log.Printf("workers: none connected (the API answers 503 offline)")
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}
