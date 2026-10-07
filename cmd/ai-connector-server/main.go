// mlxlab-hub is the public side of mlxlab: the web UI, sign-in, and the proxy to the worker on the Mac.
// See docs/architecture.md. Configured from the environment (MLXLAB_PASSWORD, MLXLAB_WORKER_TOKEN (optional,
// the legacy shared worker token), MLXLAB_DATA, MLXLAB_ADDR, MLXLAB_TOOLS).
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

	"github.com/philipparndt/ai-connector/internal/hub"
	"github.com/philipparndt/ai-connector/internal/webui"
)

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	cfg := hub.FromEnv()
	s, err := hub.New(cfg)
	if err != nil {
		log.Fatalf("mlxlab-hub: %v", err)
	}
	log.Printf("mlxlab-hub %s", version)
	if cfg.WorkerToken == "" {
		log.Printf("no MLXLAB_WORKER_TOKEN: only Macs connected in the web UI (Devices → Connect a Mac) get in")
	}
	if _, err := os.Stat(filepath.Join(cfg.Tools, "mlxlab", "Darwin-arm64")); err != nil {
		log.Printf("no Mac program in %s (MLXLAB_TOOLS): /tools/mlxlab/Darwin-arm64 answers 404", cfg.Tools)
	}
	if !webui.Built() {
		log.Printf("the web app is not built into this binary (make web); only the API is served")
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
