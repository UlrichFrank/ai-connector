package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		Server: &ServerSpec{Start: []string{"fake-server", "--model", "{model}", "--port", "{port}"}, Health: "/health"},
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
	os.WriteFile(srv, []byte(`import http.server, json, sys
model = sys.argv[sys.argv.index("--model") + 1]
port = int(sys.argv[sys.argv.index("--port") + 1])
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"ok")
    def do_POST(self):
        self.rfile.read(int(self.headers["Content-Length"]))
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
