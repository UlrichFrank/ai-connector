package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	// ExtraArgs come after the runtime's own start arguments (ComfyUI: its model paths)
	ExtraArgs []string

	mu    sync.Mutex
	cmd   *exec.Cmd // under lmu too: Unload kills it without waiting for a running job
	exit  chan struct{}
	model string     // the model the server has
	url   string     // its API base, …/v1
	key   string     // the API key of this start ("" the server takes none)
	base  string     // the server's address (http://127.0.0.1:port)
	app   string     // the folder of its app (Server.App), "" none
	lmu   sync.Mutex // logs, cmd
	logs  []string
	oa    OpenAIBackend
	state status
}

func (b *ServerBackend) Name() string   { return b.Runtime }
func (b *ServerBackend) Heavy() bool    { return true }
func (b *ServerBackend) Loaded() string { return b.state.get() }

// Unload ends the server — at once, also while a job waits for it to start or answer (the job then fails), so a
// worker that ends or another heavy model does not wait up to the start's ten minutes.
func (b *ServerBackend) Unload() {
	b.lmu.Lock()
	cmd := b.cmd
	b.lmu.Unlock()
	if cmd != nil && cmd.Process != nil {
		killGroup(cmd.Process.Pid)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stop()
}

func (b *ServerBackend) stop() {
	b.lmu.Lock()
	cmd, exit := b.cmd, b.exit
	b.cmd = nil
	b.lmu.Unlock()
	if cmd != nil && cmd.Process != nil {
		killGroup(cmd.Process.Pid)
		<-exit
		removePidFile(b.Root, b.Runtime)
	}
	b.model, b.url, b.key, b.base, b.app = "", "", "", "", ""
	b.state.set("")
}

func (b *ServerBackend) running() bool {
	b.lmu.Lock()
	cmd, exit := b.cmd, b.exit
	b.lmu.Unlock()
	if cmd == nil {
		return false
	}
	select {
	case <-exit:
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

// program is the server's program (a script of its environment, else found in its tools) and its app (Server.App in
// its tools; "" none).
func (b *ServerBackend) program(ctx context.Context, rt *RuntimeManifest, emit Emit) (prog, app string, err error) {
	name := rt.Server.Start[0]
	var tools string
	if len(rt.Tools[Platform()]) > 0 {
		if tools, err = ensureTools(ctx, b.Root, rt.Name, emit.Log); err != nil {
			return "", "", err
		}
		if rt.Server.App != "" {
			if app, err = findFile(tools, rt.Server.App); err != nil {
				return "", "", err
			}
		}
	}
	if env, ok := envFor(rt.Name, Platform()); ok {
		if err := env.Ensure(ctx, b.Root, emit.Log); err != nil {
			return "", "", err
		}
		return env.Bin(b.Root, name), app, nil
	}
	prog, err = findTool(tools, name)
	return prog, app, err
}

func (b *ServerBackend) start(ctx context.Context, rt *RuntimeManifest, dir, model string, emit Emit) error {
	prog, app, err := b.program(ctx, rt, emit)
	if err != nil {
		return err
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	// a key of this start: only the worker may ask the server (another program of this machine may not); in the
	// environment, not the arguments (which every user of the machine can read)
	key := randomKey()
	var args []string
	for _, a := range rt.Server.Start[1:] {
		args = append(args, strings.ReplaceAll(expand(a, model, dir, strconv.Itoa(port)), "{app}", app))
	}
	args = append(args, b.ExtraArgs...)
	killLeftover(b.Root, rt.Name, prog) // a server of a worker that died before it could end it
	emit(Event{Type: "status", Text: "starting " + rt.Name + " …"})
	cmd := exec.Command(prog, args...)
	ownGroup(cmd)
	cmd.Dir = dir
	if app != "" {
		cmd.Dir = filepath.Dir(app)
	}
	// not the worker's secrets (its token, HF_TOKEN, API keys): the server needs none of them
	cmd.Env = append(cleanEnv(os.Environ()), "HF_HUB_OFFLINE=1")
	if rt.Server.KeyEnv != "" {
		cmd.Env = append(cmd.Env, rt.Server.KeyEnv+"="+key)
	}
	b.key = ""
	if rt.Server.KeyEnv != "" {
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
	writePidFile(b.Root, rt.Name, cmd.Process.Pid, prog)
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
			b.lmu.Lock()
			b.cmd = nil
			b.lmu.Unlock()
			removePidFile(b.Root, rt.Name)
			return fmt.Errorf("%s ended while starting:\n%s", rt.Name, b.lastLogs())
		case <-time.After(250 * time.Millisecond):
		}
		if b.healthy(ctx, rt.Server.Health) {
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
	b.base = b.url
	if app != "" {
		b.app = filepath.Dir(app)
	}
	if p := rt.Server.Protocol; p == "" || p == "openai" {
		b.url += "/v1"
	}
	emit.Log(rt.Name + " is ready")
	return nil
}

// localHTTP asks a server of this machine: a server that takes the connection but does not answer (stopped, swapping)
// does not hold a job forever.
var localHTTP = &http.Client{Timeout: 10 * time.Second}

func (b *ServerBackend) healthy(ctx context.Context, path string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url+path, nil)
	if err != nil {
		return false
	}
	if b.key != "" {
		req.Header.Set("Authorization", "Bearer "+b.key)
	}
	resp, err := localHTTP.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// cleanEnv is the environment without the worker's secrets: names with TOKEN, SECRET, PASSWORD or KEY, and the
// worker's own settings (MLXLAB_*).
func cleanEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(name)
		if strings.Contains(u, "TOKEN") || strings.Contains(u, "SECRET") || strings.Contains(u, "PASSWORD") ||
			strings.HasSuffix(u, "_KEY") || strings.HasSuffix(u, "APIKEY") || strings.HasPrefix(u, "MLXLAB_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// The pid file of a runtime's server (<config dir>/run/<runtime>.pid: pid and program): a worker that died (killed,
// crashed) leaves its server running with its model in memory; the next start ends it first.
func pidFile(root, name string) string { return filepath.Join(root, "run", name+".pid") }

func writePidFile(root, name string, pid int, prog string) {
	os.MkdirAll(filepath.Join(root, "run"), 0o700)
	os.WriteFile(pidFile(root, name), []byte(strconv.Itoa(pid)+"\n"+prog+"\n"), 0o600)
}

func removePidFile(root, name string) { os.Remove(pidFile(root, name)) }

// killLeftover ends the server a pid file names if it is still that program (a pid may have been reused).
func killLeftover(root, name, prog string) {
	b, err := os.ReadFile(pidFile(root, name))
	if err != nil {
		return
	}
	defer removePidFile(root, name)
	pidS, was, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	pid, err := strconv.Atoi(pidS)
	if err != nil || pid <= 1 || was != prog || !alive(pid) || !runs(pid, prog) {
		return
	}
	killGroup(pid)
}

// runs: the process pid runs prog (its command line names it).
func runs(pid int, prog string) bool {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil { // no ps (Windows): the pid file is all there is
		return runtime.GOOS == "windows"
	}
	return strings.Contains(string(out), filepath.Base(prog))
}

func randomKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// listeners are the processes listening on a TCP port of this machine (lsof, else Linux's /proc; ok false: neither,
// as on Windows — then the port is not checked).
func listeners(port int) ([]int, bool) {
	if _, err := exec.LookPath("lsof"); err != nil {
		return procListeners(port)
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
