package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/pyenv"
)

// ServerBackend runs the models of a server runtime (RuntimeManifest.Server: mlx-lm, llama.cpp, …): it starts the
// runtime's server with the model on a free port of 127.0.0.1, waits until it answers, and asks it through the
// OpenAI-compatible API like a local runtime found running (OpenAIBackend). Unlike those it is heavy: one model in
// memory, the server stopped when another heavy model comes, or when the worker unloads.
type ServerBackend struct {
	Root    string
	Store   *models.Store
	Runtime string
	// StartWait is how long a server may take to load its model (0: 10 minutes).
	StartWait time.Duration

	mu    sync.Mutex
	cmd   *exec.Cmd
	exit  chan struct{}
	model string     // the model the server has
	url   string     // its API base, …/v1
	key   string     // the API key of this start ("" the server takes none)
	lmu   sync.Mutex // logs
	logs  []string
	oa    OpenAIBackend
	state status
}

func (b *ServerBackend) Name() string   { return b.Runtime }
func (b *ServerBackend) Heavy() bool    { return true }
func (b *ServerBackend) Loaded() string { return b.state.get() }

func (b *ServerBackend) Unload() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stop()
}

func (b *ServerBackend) stop() {
	if b.cmd != nil && b.cmd.Process != nil {
		b.cmd.Process.Kill()
		<-b.exit
	}
	b.cmd, b.model, b.url, b.key = nil, "", "", ""
	b.state.set("")
}

func (b *ServerBackend) running() bool {
	if b.cmd == nil {
		return false
	}
	select {
	case <-b.exit:
		return false
	default:
		return true
	}
}

func (b *ServerBackend) Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rt := runtimeManifest(b.Runtime)
	dir, err := b.files(ctx, m, emit)
	if err != nil {
		return err
	}
	model := dir
	if m.workerModel != "" {
		model = filepath.Join(dir, filepath.FromSlash(m.workerModel))
	}
	if b.model != m.ID || !b.running() {
		b.stop()
		if err := b.start(ctx, rt, dir, model, emit); err != nil {
			return err
		}
		b.model = m.ID
	}
	b.state.set(m.ID)
	served := *m
	served.served = &servedModel{Runtime: &Runtime{Name: b.Runtime, URL: b.url, Key: b.key}, Model: orStr(expand(rt.Server.Model, model, dir, ""), model)}
	if err := b.oa.Run(ctx, &served, r, outDir, emit); err != nil {
		if !b.running() {
			b.stop()
			return fmt.Errorf("%s stopped: %w\n%s", b.Runtime, err, b.lastLogs())
		}
		return err
	}
	return nil
}

// files are the model's files: fetched (and verified) into the store.
func (b *ServerBackend) files(ctx context.Context, m *Model, emit Emit) (string, error) {
	if m.repo == "" {
		return "", fmt.Errorf("%s: no source to fetch", m.ID)
	}
	var keep func(string) bool
	switch {
	case len(m.files) > 0:
		keep = func(p string) bool { return models.Matches(p, m.files...) }
	case len(m.ignore) > 0:
		keep = func(p string) bool { return !models.Matches(p, m.ignore...) }
	}
	hm, err := hubModel(ctx, b.Store, m, keep)
	if err != nil {
		return "", err
	}
	dir, err := fetch(ctx, b.Store, hm, emit)
	if err == nil {
		m.Here, m.Where = true, dir
	}
	return dir, err
}

func (b *ServerBackend) program(ctx context.Context, rt *RuntimeManifest, emit Emit) (string, error) {
	name := rt.Server.Start[0]
	if env, ok := envFor(rt.Name, Platform()); ok {
		if err := env.Ensure(ctx, b.Root, emit.Log); err != nil {
			return "", err
		}
		return env.Bin(b.Root, name), nil
	}
	dir, err := ensureTools(ctx, b.Root, rt.Name, emit.Log)
	if err != nil {
		return "", err
	}
	return findTool(dir, name)
}

func (b *ServerBackend) start(ctx context.Context, rt *RuntimeManifest, dir, model string, emit Emit) error {
	prog, err := b.program(ctx, rt, emit)
	if err != nil {
		return err
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	// a key of this start: only the worker may ask the server (another program of this machine may not)
	key, usesKey := randomKey(), rt.Server.KeyEnv != ""
	var args []string
	for _, a := range rt.Server.Start[1:] {
		usesKey = usesKey || strings.Contains(a, "{key}")
		args = append(args, strings.ReplaceAll(expand(a, model, dir, strconv.Itoa(port)), "{key}", key))
	}
	emit(Event{Type: "status", Text: "starting " + rt.Name + " …"})
	cmd := exec.Command(prog, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "HF_HUB_OFFLINE=1")
	if rt.Server.KeyEnv != "" {
		cmd.Env = append(cmd.Env, rt.Server.KeyEnv+"="+key)
	}
	b.key = ""
	if usesKey {
		b.key = key
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	exit := make(chan struct{})
	b.lmu.Lock()
	b.cmd, b.exit, b.logs = cmd, exit, nil
	b.lmu.Unlock()
	starting := true
	go pyenv.Lines(out, func(s string) {
		b.lmu.Lock()
		defer b.lmu.Unlock()
		b.logs = append(b.logs, s)
		if len(b.logs) > 20 {
			b.logs = b.logs[1:]
		}
		if starting {
			emit.Log(s)
		}
	})
	go func() { cmd.Wait(); close(exit) }()
	b.url = fmt.Sprintf("http://127.0.0.1:%d", port)
	wait := b.StartWait
	if wait == 0 {
		wait = 10 * time.Minute
	}
	deadline := time.Now().Add(wait)
	for {
		select {
		case <-ctx.Done():
			b.stop()
			return ctx.Err()
		case <-exit:
			b.cmd = nil
			return fmt.Errorf("%s ended while starting:\n%s", rt.Name, b.lastLogs())
		case <-time.After(250 * time.Millisecond):
		}
		if b.healthy(rt.Server.Health) {
			break
		}
		if time.Now().After(deadline) {
			b.stop()
			return fmt.Errorf("%s did not answer within %s", rt.Name, wait)
		}
	}
	b.lmu.Lock()
	starting = false
	b.lmu.Unlock()
	// the port is the started server's, not another program's that took it between freePort and the start
	if pids, ok := listeners(port); ok && !ownedBy(pids, cmd.Process.Pid) {
		b.stop()
		return fmt.Errorf("port %d answers, but not from %s (process %v): not used", port, rt.Name, pids)
	}
	b.url += "/v1"
	emit.Log(rt.Name + " is ready")
	return nil
}

func (b *ServerBackend) healthy(path string) bool {
	req, err := http.NewRequest(http.MethodGet, b.url+path, nil)
	if err != nil {
		return false
	}
	if b.key != "" {
		req.Header.Set("Authorization", "Bearer "+b.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func randomKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// listeners are the processes listening on a TCP port of this machine (lsof; ok false: lsof is not there, as on
// Windows — then the port is not checked).
func listeners(port int) ([]int, bool) {
	if _, err := exec.LookPath("lsof"); err != nil {
		return nil, false
	}
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fp").Output()
	if err != nil && len(out) == 0 {
		return nil, true // nobody (lsof exits 1)
	}
	var pids []int
	for _, l := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(l, "p"); ok {
			if n, err := strconv.Atoi(p); err == nil {
				pids = append(pids, n)
			}
		}
	}
	return pids, true
}

// ownedBy: some listener is the process or a child of it (a server that forks its workers).
func ownedBy(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid || parentOf(p) == pid {
			return true
		}
	}
	return false
}

func parentOf(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func (b *ServerBackend) lastLogs() string {
	b.lmu.Lock()
	defer b.lmu.Unlock()
	return strings.Join(b.logs, "\n")
}

// expand fills a start argument's {model}, {dir} and {port}.
func expand(s, model, dir, port string) string {
	return strings.NewReplacer("{model}", model, "{dir}", dir, "{port}", port).Replace(s)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
