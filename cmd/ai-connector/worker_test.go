package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/tunnel"
	"github.com/philipparndt/ai-connector/internal/web"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// flags beat the environment, the environment beats the file, each value on its own.
func TestResolveWorker(t *testing.T) {
	file := filepath.Join(t.TempDir(), "worker.json")
	if err := saveWorker(file, workerConfig{Hub: "https://file.example", Token: "file-token"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Fatalf("worker.json mode %v, want 0600", fi.Mode().Perm())
	}
	e := env(map[string]string{"MLXLAB_HUB": "https://env.example/", "MLXLAB_WORKER_TOKEN": "env-token"})
	for _, c := range []struct {
		name             string
		hub, token       string
		getenv           func(string) string
		wantHub, wantTok string
		wantSave         bool
	}{
		{"file", "", "", env(nil), "https://file.example", "file-token", false},
		{"env over file", "", "", e, "https://env.example", "env-token", false},
		{"flags over env", "https://flag.example", "flag-token", e, "https://flag.example", "flag-token", true},
		// the command the web UI shows: --hub, the token in MLXLAB_WORKER_TOKEN; both are kept
		{"hub flag, token from env", "https://flag.example", "", e, "https://flag.example", "env-token", true},
		{"hub flag, token from file", "https://flag.example", "", env(nil), "https://flag.example", "file-token", false},
		{"token flag, hub from file", "", "flag-token", env(nil), "https://file.example", "flag-token", false},
		{"both from env", "", "", e, "https://env.example", "env-token", false},
	} {
		cfg, save, err := resolveWorker(c.hub, c.token, c.getenv, file)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if cfg.Hub != c.wantHub || cfg.Token != c.wantTok || save != c.wantSave {
			t.Errorf("%s: got %+v save=%v, want %s %s save=%v", c.name, cfg, save, c.wantHub, c.wantTok, c.wantSave)
		}
	}

	none := filepath.Join(t.TempDir(), "worker.json")
	if _, _, err := resolveWorker("", "", env(nil), none); err == nil {
		t.Error("no hub anywhere: want an error")
	}
	if _, _, err := resolveWorker("https://hub.example", "", env(nil), none); err == nil || !strings.Contains(err.Error(), "--token") {
		t.Errorf("no token: want a hint about --token, got %v", err)
	}
	if _, _, err := resolveWorker("hub.example", "t", env(nil), none); err == nil {
		t.Error("a hub without http(s):// should be refused")
	}
}

// The first run of the pasted command (no worker.json yet) keeps hub and token, so a plain `mlxlab worker`
// finds them later.
func TestWorkerSavesFromEnv(t *testing.T) {
	file := filepath.Join(t.TempDir(), "mlxlab", "worker.json")
	cfg, save, err := resolveWorker("https://hub.example/", "", env(map[string]string{"MLXLAB_WORKER_TOKEN": "web-token"}), file)
	if err != nil || !save || cfg.Hub != "https://hub.example" || cfg.Token != "web-token" {
		t.Fatalf("got %+v save=%v err=%v", cfg, save, err)
	}
	if err := saveWorker(file, cfg); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("worker.json: %v %v", fi, err)
	}
	again, save, err := resolveWorker("", "", env(nil), file)
	if err != nil || save || !reflect.DeepEqual(again, cfg) {
		t.Fatalf("later run: %+v save=%v err=%v", again, save, err)
	}
}

func testManager(t *testing.T) *jobs.Manager {
	cat := &engine.Catalog{Models: []*engine.Model{{ID: "chat-a", Kind: engine.Chat, Backend: "a"}}}
	return jobs.New(cat, nil, t.TempDir())
}

// The worker's API, served through the tunnel, reached through the hub's transport.
func TestWorkerThroughHub(t *testing.T) {
	hub := &tunnel.Hub{Token: "secret"}
	mux := http.NewServeMux()
	mux.Handle(tunnel.Path, hub)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	mgr := testManager(t)
	handler := (&web.Server{Jobs: mgr, Prompts: prompts.Open(mgr.Dir)}).Handler()
	ctx, cancel := context.WithCancel(context.Background())
	connected := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- tunnel.Serve(ctx, ts.URL, "secret", "test-mac", handler, func(up bool, _ error) {
			if up {
				connected <- struct{}{}
			}
		})
	}()
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not connect")
	}
	// "connected" comes once the WebSocket is up, a moment before the hub has taken the session
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if _, ok := hub.Lookup(tunnel.Legacy); ok {
			break
		}
	}
	if c, ok := hub.Lookup(tunnel.Legacy); !ok || c.Name != "test-mac" {
		t.Fatalf("hub sees worker %+v ok=%v", c, ok)
	}

	client := &http.Client{Transport: hub.Transport(), Timeout: 10 * time.Second}
	get := func(path string, v any) {
		t.Helper()
		resp, err := client.Get("http://" + tunnel.Host(tunnel.Legacy) + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %s", path, resp.Status)
		}
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	var info map[string]any
	get("/api/info", &info)
	if info["app"] != "mlxlab" || info["role"] != "worker" || info["name"] != web.HostName() {
		t.Errorf("/api/info = %v", info)
	}
	if info["routes"] != true || info["specs"] == nil || info["state"] == nil {
		t.Errorf("/api/info without routes, specs, state: %v", info)
	}
	if _, ok := info["paired"]; ok {
		t.Error("/api/info still has paired")
	}
	var ms struct{ Models []engine.Model }
	get("/api/models", &ms)
	if len(ms.Models) != 1 || ms.Models[0].ID != "chat-a" {
		t.Errorf("/api/models = %+v", ms.Models)
	}

	// a write through the tunnel carries the hub's Origin; the worker must not refuse it
	req, _ := http.NewRequest(http.MethodPost, "http://"+tunnel.Host(tunnel.Legacy)+"/api/jobs/nope/cancel", nil)
	req.Header.Set("Origin", "https://mlxlab.example")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Error("a write through the tunnel was refused as cross-origin")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

// A wrong token stops the worker at once, with a message that says how to set it.
func TestWorkerUnauthorized(t *testing.T) {
	hub := &tunnel.Hub{Token: "secret"}
	mux := http.NewServeMux()
	mux.Handle(tunnel.Path, hub)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	mgr := testManager(t)
	err := worker(mgr, workerConfig{Hub: ts.URL, Token: "wrong"}, "test-mac", http.NotFoundHandler())
	if err == nil || !strings.Contains(err.Error(), "--token") || !strings.Contains(err.Error(), "MLXLAB_WORKER_TOKEN") {
		t.Fatalf("want a hint on setting the token, got %v", err)
	}
}

func TestServeRefusesOtherNetworks(t *testing.T) {
	mgr := testManager(t)
	if _, err := serve(mgr, "0.0.0.0:0"); err == nil || !strings.Contains(err.Error(), "hub") {
		t.Fatalf("want a refusal pointing at the hub, got %v", err)
	}
	url, err := serve(mgr, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// a page of another site must not post to the local server
	req, _ := http.NewRequest(http.MethodPost, url+"/api/unload", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin write: %s, want 403", resp.Status)
	}
}

// A second worker on the same Mac: its own config file and state dir (flag, else env, else the config dir).
func TestWorkerFileAndState(t *testing.T) {
	root := "/cfg/mlxlab"
	if got := workerFile("", env(nil), root); got != filepath.Join(root, "worker.json") {
		t.Fatalf("default config: %s", got)
	}
	if got := workerFile("", env(map[string]string{"MLXLAB_WORKER_CONFIG": "/l/worker.json"}), root); got != "/l/worker.json" {
		t.Fatalf("env config: %s", got)
	}
	if got := workerFile("/f.json", env(map[string]string{"MLXLAB_WORKER_CONFIG": "/l/worker.json"}), root); got != "/f.json" {
		t.Fatalf("flag config: %s", got)
	}
	if got := stateDir("", env(nil), root); got != root {
		t.Fatalf("default state: %s", got)
	}
	if got := stateDir("", env(map[string]string{"MLXLAB_STATE": "/l/state"}), root); got != "/l/state" {
		t.Fatalf("env state: %s", got)
	}
	if got := stateDir("/s", env(map[string]string{"MLXLAB_STATE": "/l/state"}), root); got != "/s" {
		t.Fatalf("flag state: %s", got)
	}
}

// setup with a state dir: the jobs, prompts and sessions are there, the models are looked up in the config dir.
func TestSetupState(t *testing.T) {
	root, own := t.TempDir(), t.TempDir()
	_, mgr := setup(root, own)
	if mgr.Dir != own || mgr.OutDir() != filepath.Join(own, "outputs") {
		t.Fatalf("jobs in %s", mgr.Dir)
	}
	if claudeAgents == nil || claudeAgents.Root != filepath.Join(own, "agents") || len(claudeAgents.Shared) != 1 || claudeAgents.Shared[0] != root {
		t.Fatalf("agents: %+v", claudeAgents)
	}
	if _, err := os.Stat(filepath.Join(root, "history.json")); err == nil {
		t.Fatal("history.json written to the config dir")
	}
}

func TestIdleSettings(t *testing.T) {
	defer func(i string, p []string) { idleUnload, pinned = i, p }(idleUnload, pinned)
	idleUnload, pinned = "", nil
	if d, p := idleSettings(env(nil)); d != defaultIdleUnload || p != nil {
		t.Errorf("default: %v %v", d, p)
	}
	idleUnload, pinned = "1h", []string{"qwen3.6-35b-a3b"}
	if d, p := idleSettings(env(nil)); d != time.Hour || len(p) != 1 {
		t.Errorf("from the file: %v %v", d, p)
	}
	if d, p := idleSettings(env(map[string]string{"MLXLAB_IDLE_UNLOAD": "off", "MLXLAB_PIN": "a, b"})); d != 0 || len(p) != 2 || p[1] != "b" {
		t.Errorf("from the environment: %v %v", d, p)
	}
}
