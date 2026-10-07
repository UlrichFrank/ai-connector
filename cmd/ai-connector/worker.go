package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/tunnel"
)

// workerConfig is where the worker connects to, kept in <config>/worker.json.
type workerConfig struct {
	Hub   string `json:"hub"`
	Token string `json:"token"`
}

// resolveWorker picks the hub URL and the token, each on its own: the flag, else the environment, else the file.
// save: the hub came as a flag and the token as a flag or from MLXLAB_WORKER_TOKEN (the command the web UI
// shows), so the file should remember both for next time.
func resolveWorker(flagHub, flagToken string, getenv func(string) string, file string) (cfg workerConfig, save bool, err error) {
	var saved workerConfig
	if b, err := os.ReadFile(file); err == nil {
		if err := json.Unmarshal(b, &saved); err != nil {
			return cfg, false, fmt.Errorf("%s: %w", file, err)
		}
	}
	pick := func(flag, env, saved string) string {
		if flag != "" {
			return flag
		}
		if v := getenv(env); v != "" {
			return v
		}
		return saved
	}
	cfg = workerConfig{
		Hub:   strings.TrimRight(pick(flagHub, "MLXLAB_HUB", saved.Hub), "/"),
		Token: pick(flagToken, "MLXLAB_WORKER_TOKEN", saved.Token),
	}
	if cfg.Hub == "" {
		return cfg, false, errors.New("no hub: mlxlab worker --hub https://… --token … (or MLXLAB_HUB)")
	}
	if !strings.HasPrefix(cfg.Hub, "http://") && !strings.HasPrefix(cfg.Hub, "https://") {
		return cfg, false, fmt.Errorf("the hub %q needs http:// or https://", cfg.Hub)
	}
	if cfg.Token == "" {
		return cfg, false, errors.New(tokenHint)
	}
	return cfg, flagHub != "" && (flagToken != "" || getenv("MLXLAB_WORKER_TOKEN") != ""), nil
}

const tokenHint = "no worker token: MLXLAB_WORKER_TOKEN=T mlxlab worker --hub URL, or mlxlab worker --hub URL --token T " +
	"(kept in worker.json) — the token comes from the web UI (Devices → Connect a Mac)"

func saveWorker(file string, cfg workerConfig) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	// the token lets anyone pose as this Mac to the hub
	if err := os.WriteFile(file, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(file, 0o600) // an older file keeps its mode with WriteFile
}

// worker serves handler to the hub until Ctrl-C (or SIGTERM), printing every change of the connection.
func worker(mgr *jobs.Manager, cfg workerConfig, name string, handler http.Handler) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ch, unsub := mgr.Subscribe()
	defer unsub()
	go logUpdates(ch)

	fmt.Printf("mlxlab worker %q: connecting to %s …\n", name, cfg.Hub)
	up := false
	err := tunnel.Serve(ctx, cfg.Hub, cfg.Token, name, handler, func(connected bool, err error) {
		now := time.Now().Format("15:04:05")
		switch {
		case connected:
			up = true
			fmt.Printf("%s connected to %s\n", now, cfg.Hub)
		case errors.Is(err, tunnel.ErrUnauthorized) || ctx.Err() != nil:
			// said below, or we are stopping
		case up:
			up = false
			fmt.Printf("%s lost connection: %v — retrying\n", now, err)
		default:
			fmt.Printf("%s cannot reach the hub: %v — retrying\n", now, err)
		}
	})
	// like mlxlab serve: the models (and the Python workers) go before we exit
	fmt.Println("mlxlab worker: stopping …")
	mgr.Unload()
	closeSandboxes()
	if errors.Is(err, tunnel.ErrUnauthorized) {
		return fmt.Errorf("%s refused the worker token (revoked or mistyped?); connect the Mac again from the web UI "+
			"(Devices → Connect a Mac), or set it with MLXLAB_WORKER_TOKEN=T mlxlab worker --hub %s (or --token T)",
			cfg.Hub, cfg.Hub)
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
