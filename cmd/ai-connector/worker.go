package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// AllowLocalRuntimes: the worker reads <config dir>/runtimes.d, Python runtimes of this machine's own
	// (docs/models.md) — code nobody reviewed, so off unless set here
	AllowLocalRuntimes bool `json:"allowLocalRuntimes,omitempty"`
	// IdleUnload: a model with no job for this long leaves memory ("10m", "1h"; "0" or "" never, the default);
	// Pin: models that stay loaded (MLXLAB_IDLE_UNLOAD and MLXLAB_PIN, a comma list, override them)
	IdleUnload string   `json:"idleUnload,omitempty"`
	Pin        []string `json:"pin,omitempty"`
}

// allowLocalRuntimes, idleUnload and pinned are the worker's settings from its file (set before setup).
var (
	allowLocalRuntimes bool
	idleUnload         string
	pinned             []string
)

// defaultIdleUnload is how long a model without a job stays in memory: 0, never — unloading changes what users of an
// existing worker get (a cold start after a pause), so a worker turns it on (idleUnload, MLXLAB_IDLE_UNLOAD).
const defaultIdleUnload time.Duration = 0

// idleSettings are the idle time and the pinned models: the environment's, else the worker file's, else the default.
func idleSettings(getenv func(string) string) (time.Duration, []string) {
	idle, pins := defaultIdleUnload, pinned
	s := idleUnload
	if v := getenv("MLXLAB_IDLE_UNLOAD"); v != "" {
		s = v
	}
	switch s {
	case "":
	case "0", "off", "never":
		idle = 0
	default:
		if d, err := time.ParseDuration(s); err == nil {
			idle = d
		} else {
			fmt.Fprintf(os.Stderr, "mlxlab: idle unload %q: %v (models stay loaded)\n", s, err)
		}
	}
	if v := getenv("MLXLAB_PIN"); v != "" {
		pins = nil
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				pins = append(pins, p)
			}
		}
	}
	return idle, pins
}

// workerFile is the worker's config file (hub and token): --config, else $MLXLAB_WORKER_CONFIG, else
// <config dir>/worker.json. A second worker on the same Mac (another hub) keeps its own file this way.
func workerFile(flagFile string, getenv func(string) string, root string) string {
	if flagFile != "" {
		return flagFile
	}
	if v := getenv("MLXLAB_WORKER_CONFIG"); v != "" {
		return v
	}
	return filepath.Join(root, "worker.json")
}

// stateDir is where a worker keeps what is its own — the jobs (history.json, outputs/), the prompts, the tools
// allowed always (approvals.json), the Claude Code sessions (agents/, agents.json): --state, else $MLXLAB_STATE,
// else the config dir itself. What is shared stays in the config dir: the models, the Python envs, lib/, tools/,
// mcp.json, the voices and the trained styles. So a second worker on the same Mac (e.g. against a local server
// while the first serves the Pi's hub) shares the downloads but not the history of the other.
func stateDir(flagDir string, getenv func(string) string, root string) string {
	if flagDir != "" {
		return flagDir
	}
	if v := getenv("MLXLAB_STATE"); v != "" {
		return v
	}
	return root
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
		// set in the file only (saving keeps it)
		AllowLocalRuntimes: saved.AllowLocalRuntimes, IdleUnload: saved.IdleUnload, Pin: saved.Pin,
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

// workerLock is the instance lock of the worker with the config file file, beside it (worker.json → worker.lock):
// one `ai-connector worker` per file, so a terminal copy and the one started at login do not both dial the hub with
// the same token (instance.go).
func workerLock(file string) string {
	return strings.TrimSuffix(file, filepath.Ext(file)) + ".lock"
}

// takeWorkerLock waits until no other worker with file runs and takes its lock (nil, nil: stopped while waiting).
func takeWorkerLock(file string) (*os.File, error) {
	path := workerLock(file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	waited := false
	lock, err := waitInstance(ctx, path, 5*time.Second, func(pid int) {
		who := "another mlxlab worker runs"
		if pid > 0 {
			who += fmt.Sprintf(" (pid %d)", pid)
		}
		fmt.Printf("%s with %s: waiting until it ends (`ai-connector worker --install` replaces it)\n", who, file)
		waited = true
	})
	if errors.Is(err, context.Canceled) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if waited {
		fmt.Println("the other mlxlab worker ended: this one takes over")
	}
	return lock, nil
}

// workerKind is the start at login of `ai-connector worker` with the config file file (def: the default one,
// <config dir>/worker.json) and the --state dir ("": not given). Another file than the default one is a second
// worker on this computer: its service has names of its own (workerSuffix) and runs with --config FILE; --state DIR
// goes on its command line too (both as absolute paths). Hub and token never: they are in the file (0600).
func workerKind(file, def, state string) serviceKind {
	k := serviceKind{label: "de.rnd7.ai-connector.worker", unit: "ai-connector-worker.service", task: "ai-connector worker",
		log: "ai-connector-worker.log", what: "ai-connector worker", desc: "ai-connector worker (this computer's models for its server)",
		args: []string{"worker"}}
	if absPath(file) != absPath(def) {
		sfx := workerSuffix(file)
		k.label += "." + sfx
		k.unit = "ai-connector-worker-" + sfx + ".service"
		k.task += " " + sfx
		k.log = "ai-connector-worker-" + sfx + ".log"
		k.what += " --config " + file
		k.args = append(k.args, "--config", absPath(file))
	}
	if state != "" {
		k.args = append(k.args, "--state", absPath(state))
	}
	return k
}

// workerSuffix names a second worker's service after its config file: worker-local.json → "local", Pi.json → "pi"
// (lower case, a-z 0-9 -); a name that says nothing (worker.json in another folder) → the first 8 hex digits of the
// SHA-256 of the file's absolute path.
func workerSuffix(file string) string {
	name := strings.ToLower(strings.TrimSuffix(filepath.Base(file), filepath.Ext(file)))
	for _, p := range []string{"worker-", "worker_", "worker."} {
		name = strings.TrimPrefix(name, p)
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	if s := strings.TrimRight(b.String(), "-"); s != "" && s != "worker" {
		return s
	}
	sum := sha256.Sum256([]byte(absPath(file)))
	return hex.EncodeToString(sum[:4])
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return filepath.Clean(p)
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
