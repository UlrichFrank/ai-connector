package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/llm"
	"github.com/philipparndt/ai-connector/internal/models"
)

func tarGz(t *testing.T, entries ...tar.Header) string {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		h := h
		body := []byte("x")
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		tw.WriteHeader(&h)
		if h.Typeflag == tar.TypeReg {
			tw.Write(body)
		}
	}
	tw.Close()
	gz.Close()
	f := filepath.Join(t.TempDir(), "a.tar.gz")
	os.WriteFile(f, b.Bytes(), 0o644)
	return f
}

// A release archive unpacks with its folders and links; a path or a link out of the folder is refused.
func TestUnpack(t *testing.T) {
	dir := t.TempDir()
	f := tarGz(t, tar.Header{Name: "llama-b1/", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "llama-b1/libggml.0.dylib", Typeflag: tar.TypeReg, Mode: 0o644},
		tar.Header{Name: "llama-b1/libggml.dylib", Typeflag: tar.TypeSymlink, Linkname: "libggml.0.dylib"},
		tar.Header{Name: "llama-b1/llama-server", Typeflag: tar.TypeReg, Mode: 0o755})
	if err := unpack(f, "x.tar.gz", dir); err != nil {
		t.Fatal(err)
	}
	if p, err := findTool(dir, "llama-server"); err != nil || filepath.Base(filepath.Dir(p)) != "llama-b1" {
		t.Errorf("%s %v", p, err)
	}
	if l, _ := os.Readlink(filepath.Join(dir, "llama-b1", "libggml.dylib")); l != "libggml.0.dylib" {
		t.Errorf("link %q", l)
	}
	// a hard link to an entry before it: there too; one out of the folder, or an entry of another kind: refused
	hard := tarGz(t, tar.Header{Name: "lib/libggml.0.dylib", Typeflag: tar.TypeReg, Mode: 0o644},
		tar.Header{Name: "lib/libggml.1.dylib", Typeflag: tar.TypeLink, Linkname: "lib/libggml.0.dylib"})
	hd := t.TempDir()
	if err := unpack(hard, "x.tar.gz", hd); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(hd, "lib", "libggml.1.dylib")); err != nil {
		t.Errorf("the hard link: %v", err)
	}
	if err := unpack(tarGz(t, tar.Header{Name: "x", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd"}), "x.tar.gz", t.TempDir()); err == nil {
		t.Error("a hard link out of the folder")
	}
	if err := unpack(tarGz(t, tar.Header{Name: "p", Typeflag: tar.TypeFifo, Mode: 0o644}), "x.tar.gz", t.TempDir()); err == nil {
		t.Error("a pipe unpacked without a word")
	}
	// a chain of links: "a" → "." passes, then "b" below it must not
	chain := tarGz(t, tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "."},
		tar.Header{Name: "a/b", Typeflag: tar.TypeSymlink, Linkname: ".."},
		tar.Header{Name: "a/b/x", Typeflag: tar.TypeReg, Mode: 0o644})
	if err := unpack(chain, "x.tar.gz", t.TempDir()); err == nil || !strings.Contains(err.Error(), "below the link") {
		t.Errorf("a chain of links: %v", err)
	}
	for _, bad := range []tar.Header{{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "a/link", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd"},
		{Name: "a/abs", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}} {
		if err := unpack(tarGz(t, bad), "x.tar.gz", t.TempDir()); err == nil {
			t.Errorf("%s → %s unpacked", bad.Name, bad.Linkname)
		}
	}
}

// A server runtime: the worker starts the program with the model, waits for its health check, asks it through the
// OpenAI API; another model restarts it, Unload ends it.
func TestServerBackend(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 plays the server")
	}
	root := t.TempDir()
	rt := &RuntimeManifest{Name: "fake-server", Kinds: []Kind{Chat},
		Server: &ServerSpec{Start: []string{"fake-server", "--model", "{model}", "--port", "{port}"}, Health: "/health", KeyEnv: "FAKE_KEY"},
		Tools:  map[string][]ToolArchive{Platform(): {{URL: "https://example.invalid/x.tar.gz", SHA256: strings.Repeat("0", 64)}}}}
	runtimesMu.Lock()
	runtimes()[rt.Name] = rt
	runtimesMu.Unlock()
	t.Cleanup(func() {
		runtimesMu.Lock()
		delete(runtimes(), rt.Name)
		runtimesMu.Unlock()
	})
	// its program is there already (no download): a Python server that answers with the model it was started with
	dir, _, _ := toolDir(root, rt.Name)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ".mlxlab-ready"), nil, 0o644)
	srv := filepath.Join(dir, "server.py")
	os.WriteFile(srv, []byte(`import http.server, json, os, sys
model = sys.argv[sys.argv.index("--model") + 1]
port = int(sys.argv[sys.argv.index("--port") + 1])
key = os.environ.get("FAKE_KEY", "")
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"ok")
    def do_POST(self):
        self.rfile.read(int(self.headers["Content-Length"]))
        if len(key) < 32 or self.headers.get("Authorization") != "Bearer " + key:
            self.send_response(401); self.end_headers(); return
        self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.end_headers()
        chunk = {"choices": [{"delta": {"content": "hello from " + model.split("/")[-1]}}]}
        self.wfile.write(("data: " + json.dumps(chunk) + "\n\ndata: [DONE]\n\n").encode())
print("listening", flush=True)
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
`), 0o644)
	os.WriteFile(filepath.Join(dir, "fake-server"), []byte("#!/bin/sh\nexec "+py+" "+srv+" \"$@\"\n"), 0o755)

	store := &models.Store{Dir: filepath.Join(root, "models")}
	mk := func(id, file string) *Model {
		m := &Model{ID: id, Name: id, Kind: Chat, Backend: rt.Name, repo: "org/" + id, rev: strings.Repeat("a", 40), workerModel: file}
		d := filepath.Join(store.Dir, "org", id, m.rev)
		os.MkdirAll(d, 0o755)
		// the store has the file, verified: nothing to fetch
		os.WriteFile(filepath.Join(d, file), []byte("abc"), 0o644)
		os.WriteFile(filepath.Join(d, file+".sha256"), []byte("s"), 0o644)
		os.WriteFile(filepath.Join(d, ".files.json"), []byte(`[{"path":"`+file+`","size":3,"sha256":"s"}]`), 0o644)
		return m
	}
	b := &ServerBackend{Root: root, Store: store, Runtime: rt.Name, StartWait: 20 * time.Second}
	t.Cleanup(b.Unload)
	ask := func(m *Model) string {
		var text strings.Builder
		err := b.Run(context.Background(), m, Request{Messages: []llm.Message{{Role: "user", Content: "hi"}}}, t.TempDir(), func(e Event) {
			if e.Type == "token" || e.Type == "text" {
				text.WriteString(e.Text)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		return text.String()
	}
	if got := ask(mk("one", "one.gguf")); !strings.Contains(got, "one.gguf") || b.Loaded() != "one" {
		t.Errorf("answer %q, loaded %q", got, b.Loaded())
	}
	first := b.cmd.Process.Pid
	if got := ask(mk("two", "two.gguf")); !strings.Contains(got, "two.gguf") || b.cmd.Process.Pid == first {
		t.Errorf("another model: answer %q, the server restarted: %v", got, b.cmd.Process.Pid != first)
	}
	b.Unload()
	if b.running() || b.Loaded() != "" {
		t.Error("still running after Unload")
	}
}

// A server a dead worker left behind (its pid file) is ended before the next start; a pid now of another program is
// left alone.
// toolsRuntime is a server runtime whose program is the archive the handler serves (the archive's sum when it is given).
func toolsRuntime(t *testing.T, name string, archive []byte, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(archive)
	rt := &RuntimeManifest{Name: name, Kinds: []Kind{Chat},
		Tools: map[string][]ToolArchive{Platform(): {{URL: srv.URL + "/x.tar.gz", SHA256: hex.EncodeToString(sum[:])}}}}
	runtimesMu.Lock()
	runtimes()[name] = rt
	runtimesMu.Unlock()
	t.Cleanup(func() {
		runtimesMu.Lock()
		delete(runtimes(), name)
		runtimesMu.Unlock()
	})
	return t.TempDir()
}

// A connection that stands still counts as broken: it is tried again, and the job does not hang; a slow one that keeps
// sending is not ended.
func TestToolDownloadStalls(t *testing.T) {
	old := toolStall
	toolStall = 200 * time.Millisecond
	t.Cleanup(func() { toolStall = old })
	archive, err := os.ReadFile(tarGz(t, tar.Header{Name: "server", Typeflag: tar.TypeReg, Mode: 0o755}))
	if err != nil {
		t.Fatal(err)
	}
	var tries atomic.Int32
	mode := atomic.Value{}
	mode.Store("stall")
	root := toolsRuntime(t, "tools-stall", archive, func(w http.ResponseWriter, r *http.Request) {
		tries.Add(1)
		if mode.Load() == "stall" {
			w.Write(archive[:1])
			w.(http.Flusher).Flush()
			<-r.Context().Done() // sends nothing more, until the client goes
			return
		}
		for _, b := range archive { // slower than toolStall in all, never as slow between two pieces
			w.Write([]byte{b})
			w.(http.Flusher).Flush()
			time.Sleep(time.Duration(int64(600*time.Millisecond) / int64(len(archive))))
		}
	})
	t0 := time.Now()
	_, err = ensureTools(context.Background(), root, "tools-stall", func(string) {})
	if !errors.Is(err, errToolStalled) {
		t.Fatalf("got %v", err)
	}
	if n := tries.Load(); n != 3 {
		t.Errorf("%d tries", n)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Errorf("took %s", d)
	}
	mode.Store("slow")
	if _, err := ensureTools(context.Background(), root, "tools-stall", func(string) {}); err != nil {
		t.Errorf("a slow download that keeps sending: %v", err)
	}
}

// A download's preparing and a job's start of the same runtime: one fetches, the other waits and finds it ready.
func TestEnsureToolsTogether(t *testing.T) {
	archive, err := os.ReadFile(tarGz(t, tar.Header{Name: "bin/server", Typeflag: tar.TypeReg, Mode: 0o755}))
	if err != nil {
		t.Fatal(err)
	}
	var fetched atomic.Int32
	root := toolsRuntime(t, "tools-together", archive, func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		time.Sleep(50 * time.Millisecond) // the others arrive meanwhile
		w.Write(archive)
	})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir, err := ensureTools(context.Background(), root, "tools-together", func(string) {})
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := os.Stat(filepath.Join(dir, "bin", "server")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := fetched.Load(); n != 1 {
		t.Errorf("fetched %d times", n)
	}
	// while another fetches, a canceled job does not wait
	dir, _, _ := toolDir(root, "tools-together")
	os.Remove(filepath.Join(dir, ".mlxlab-ready"))
	slot, _ := toolLocks.Load(dir)
	slot.(chan struct{}) <- struct{}{}
	defer func() { <-slot.(chan struct{}) }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ensureTools(ctx, root, "tools-together", func(string) {}); err != context.Canceled {
		t.Errorf("canceled while waiting: %v", err)
	}
}

// startLeftover is a process of its own group, like a server its worker left behind; done closes when it has ended.
func startLeftover(t *testing.T) (*exec.Cmd, string, chan struct{}) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	cmd := exec.Command(sleep, "60")
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		killGroup(cmd.Process.Pid)
		<-done
	})
	return cmd, sleep, done
}

func stillRuns(t *testing.T, done chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
		t.Fatal(what)
	case <-time.After(200 * time.Millisecond):
	}
}

func hasEnded(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the leftover server still runs")
	}
}

func TestKillLeftover(t *testing.T) {
	cmd, sleep, done := startLeftover(t)
	root := t.TempDir()
	pid := strconv.Itoa(cmd.Process.Pid)
	write := func(content string) {
		os.MkdirAll(filepath.Join(root, "run"), 0o700)
		os.WriteFile(pidFile(root, "x"), []byte(content), 0o600)
	}
	// a pid reused: it started at another time, also if it runs the same program
	write(pid + "\n" + sleep + "\nMon Jan  1 00:00:00 2001\n")
	killLeftover(root, "x")
	stillRuns(t, done, "another process of that pid was killed")
	if _, err := os.Stat(pidFile(root, "x")); err == nil {
		t.Error("the pid file is still there")
	}
	// no time known: the same name in another place (ComfyUI's program is "python") is not the same program
	write(pid + "\n/somewhere/else/" + filepath.Base(sleep) + "\n")
	killLeftover(root, "x")
	stillRuns(t, done, "another program of that name was killed")
	// the program's path has changed since (an update), the process is the same: ended
	writePidFile(root, "x", cmd.Process.Pid, "/an/older/environment/server")
	killLeftover(root, "x")
	hasEnded(t, done)
	if _, err := os.Stat(pidFile(root, "x")); err == nil {
		t.Error("the pid file is still there")
	}
}

// A worker that starts ends what a dead one left, of every runtime, but not the server of a worker that still runs.
func TestEndLeftovers(t *testing.T) {
	cmd, sleep, done := startLeftover(t)
	owner, _, _ := startLeftover(t)
	root := t.TempDir()
	writePidFile(root, "x", cmd.Process.Pid, sleep)
	f := func(ownerPid int) {
		b, _ := os.ReadFile(pidFile(root, "x"))
		lines := strings.Split(string(b), "\n")
		start, _ := processStart(ownerPid)
		os.WriteFile(pidFile(root, "x"), []byte(strings.Join([]string{lines[0], lines[1], lines[2], strconv.Itoa(ownerPid), start, ""}, "\n")), 0o600)
	}
	f(owner.Process.Pid)
	EndLeftovers(root)
	stillRuns(t, done, "the server of a worker that runs was killed")
	if _, err := os.Stat(pidFile(root, "x")); err != nil {
		t.Error("its pid file is gone")
	}
	// that worker dies
	killGroup(owner.Process.Pid)
	for deadline := time.Now().Add(5 * time.Second); alive(owner.Process.Pid) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	EndLeftovers(root)
	hasEnded(t, done)
	if _, err := os.Stat(pidFile(root, "x")); err == nil {
		t.Error("the pid file is still there")
	}
	EndLeftovers(t.TempDir()) // no folder, nothing to do
}

// A server that ends while starting says why: its last lines are in the error, also after much output. A child that
// lives on with its output does not hold the worker.
func TestServerEndsWhileStarting(t *testing.T) {
	for _, c := range []struct{ name, script string }{
		{"ends", "#!/bin/sh\ni=0\nwhile [ $i -lt 2000 ]; do echo line $i; i=$((i+1)); done\necho failed to load model\nexit 1\n"},
		{"child", "#!/bin/sh\nsleep 8 &\necho failed to load model\nexit 1\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			rt := &RuntimeManifest{Name: "ends-" + c.name, Kinds: []Kind{Chat}, Server: &ServerSpec{Start: []string{"fake-server"}, Health: "/health"},
				Tools: map[string][]ToolArchive{Platform(): {{URL: "https://example.invalid/x.tar.gz", SHA256: strings.Repeat("0", 64)}}}}
			runtimesMu.Lock()
			runtimes()[rt.Name] = rt
			runtimesMu.Unlock()
			t.Cleanup(func() {
				runtimesMu.Lock()
				delete(runtimes(), rt.Name)
				runtimesMu.Unlock()
			})
			dir, _, _ := toolDir(root, rt.Name)
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, ".mlxlab-ready"), nil, 0o644)
			os.WriteFile(filepath.Join(dir, "fake-server"), []byte(c.script), 0o755)
			b := &ServerBackend{Root: root, Runtime: rt.Name, StartWait: 20 * time.Second}
			t0 := time.Now()
			err := b.start(context.Background(), rt, root, root, func(Event) {})
			if err == nil || !strings.Contains(err.Error(), "failed to load model") {
				t.Errorf("got %v", err)
			}
			if d := time.Since(t0); d > 6*time.Second {
				t.Errorf("took %s", d)
			}
		})
	}
}

func TestCleanEnv(t *testing.T) {
	got := strings.Join(cleanEnv([]string{"PATH=/bin", "HF_TOKEN=x", "MLXLAB_WORKER_TOKEN=y", "OPENAI_API_KEY=z", "HOME=/h",
		"MLXLAB_GPU=rocm", "AWS_SECRET_ACCESS_KEY=s", "LANG=de", "AWS_ACCESS_KEY_ID=a", "API_KEY_FILE=f", "GITHUB_PAT=p", "DB_URL=u",
		"HTTPS_PROXY=http://me:pw@proxy", "NVIDIA_API_KEY=n", "CUDA_VISIBLE_DEVICES=0", "LC_ALL=C", "SystemRoot=C:\\Windows"}), " ")
	if got != "PATH=/bin HOME=/h LANG=de CUDA_VISIBLE_DEVICES=0 LC_ALL=C SystemRoot=C:\\Windows" {
		t.Errorf("%s", got)
	}
}

// Several processes share a config dir: each keeps its own pid file of a runtime, so none overwrites or removes
// another's; a file of an older version (no owner in its name) is still ended.
func TestPidFilePerOwner(t *testing.T) {
	cmd, sleep, done := startLeftover(t)
	root := t.TempDir()
	writePidFile(root, "x", cmd.Process.Pid, sleep)
	// another process's file of the same runtime
	other := filepath.Join(root, "run", "x@999999.pid")
	os.WriteFile(other, []byte("1\nother\n\n999999\n\n"), 0o600)
	removePidFile(root, "x")
	if _, err := os.Stat(other); err != nil {
		t.Fatal("removing this process's file removed another's")
	}
	if got := pidFiles(root, "x"); len(got) != 1 || got[0] != other {
		t.Fatalf("files %v", got)
	}
	// an older version's file: found, and its dead owner's server ended
	old := filepath.Join(root, "run", "x.pid")
	start, _ := processStart(cmd.Process.Pid)
	os.WriteFile(old, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"+sleep+"\n"+start+"\n999998\n\n"), 0o600)
	killLeftover(root, "x")
	hasEnded(t, done)
	if _, err := os.Stat(old); err == nil {
		t.Error("the old pid file is still there")
	}
}

// On Windows a process is known by tasklist's image name.
func TestTasklistImage(t *testing.T) {
	out := "\"llama-server.exe\",\"4242\",\"Console\",\"1\",\"1.234 K\"\r\n"
	if img, ok := tasklistImage(out, 4242); !ok || img != "llama-server.exe" {
		t.Fatalf("%q %v", img, ok)
	}
	if _, ok := tasklistImage("INFO: No tasks are running which match the specified criteria.\r\n", 4242); ok {
		t.Fatal("no such process")
	}
	if _, ok := tasklistImage(out, 42); ok {
		t.Fatal("another pid")
	}
}
