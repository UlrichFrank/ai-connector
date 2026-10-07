package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/agent"
	websandbox "github.com/philipparndt/ai-connector/sandbox/web"
)

func TestReadKeyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "brave.key")
	if k, err := readKeyFile(p); k != "" || err != nil {
		t.Fatalf("missing file: %q %v", k, err)
	}
	if err := os.WriteFile(p, []byte("abc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readKeyFile(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("world-readable key accepted: %v", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := readKeyFile(p); k != "abc123" || err != nil {
		t.Errorf("got %q %v", k, err)
	}
}

func TestLoadBraveKeyEnv(t *testing.T) {
	t.Setenv("MLXLAB_BRAVE_KEY", " k1 ")
	if k, err := LoadBraveKey(); k != "k1" || err != nil {
		t.Errorf("got %q %v", k, err)
	}
}

func TestTools(t *testing.T) {
	s := New(Config{CLI: "/nonexistent"})
	ts, err := s.Tools(context.Background())
	if err != nil || len(ts) != 2 {
		t.Fatalf("%v %v", ts, err)
	}
	for _, tool := range ts {
		if tool.Zone != agent.ZoneWeb || !tool.Untrusted || tool.Changes {
			t.Errorf("%s: zone %q untrusted %v changes %v", tool.Name, tool.Zone, tool.Untrusted, tool.Changes)
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
			t.Errorf("%s: schema: %v", tool.Name, err)
		}
	}
	// bad arguments are refused before the sandbox is started (the CLI does not exist here)
	for _, c := range []struct{ name, args string }{
		{ToolSearch, `{}`}, {ToolSearch, `{"query":"  "}`}, {ToolFetch, `{"url":"file:///etc/passwd"}`},
		{ToolFetch, `{"url":"data:text/html,x"}`}, {ToolFetch, `{"url":"javascript:alert(1)"}`}, {"other", `{}`},
	} {
		res, err := s.Call(context.Background(), c.name, json.RawMessage(c.args))
		if err != nil || !res.IsError {
			t.Errorf("%s %s: %+v %v", c.name, c.args, res, err)
		}
	}
}

// TestCallAgainstFakeService runs the toolbox against a fake tools service on the unix socket.
func TestCallAgainstFakeService(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "t.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix socket:", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	mux.HandleFunc("POST /search", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"title":"A  title","url":"https://a.example/","snippet":"first\nhit"},` +
			`{"title":"B","url":"https://b.example/","snippet":""}]}`))
	})
	mux.HandleFunc("POST /fetch", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ URL string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if strings.Contains(in.URL, "private") {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"403 Forbidden: denied"}`))
			return
		}
		_, _ = w.Write([]byte(`{"url":"https://a.example/x","title":"Page","text":"Hello.","truncated":true}`))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	s := New(Config{CLI: "/nonexistent", Socket: sock})
	res, err := s.Call(context.Background(), ToolSearch, json.RawMessage(`{"query":"q"}`))
	if err != nil || res.IsError {
		t.Fatalf("%+v %v", res, err)
	}
	if len(res.Sources) != 2 || res.Sources[0].Title != "A title" || !strings.Contains(res.Text, "1. A title\n   https://a.example/\n   first hit") {
		t.Errorf("search result:\n%s\n%+v", res.Text, res.Sources)
	}
	res, err = s.Call(context.Background(), ToolFetch, json.RawMessage(`{"url":"https://a.example/x"}`))
	if err != nil || res.IsError || len(res.Sources) != 1 || res.Sources[0].URL != "https://a.example/x" ||
		!strings.Contains(res.Text, "# Page\nURL: https://a.example/x\n\nHello.") || !strings.Contains(res.Text, "cut") {
		t.Errorf("fetch result: %+v %v", res, err)
	}
	res, err = s.Call(context.Background(), ToolFetch, json.RawMessage(`{"url":"https://private.example/"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Text, "denied") {
		t.Errorf("refused fetch: %+v %v", res, err)
	}
}

func TestBuildContext(t *testing.T) {
	dir := t.TempDir()
	if err := websandbox.WriteContext(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"go.mod", "Containerfile.proxy", "Containerfile.tools", "Containerfile.searxng",
		"proxy/main.go", "proxy/policy.go", "tools/main.go", "tools/html.go", "lockdown/main_linux.go",
		"searxng/settings.yml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("build context: %v", err)
		}
	}
	if h := websandbox.Hash(); len(h) != 12 {
		t.Errorf("hash %q", h)
	}
}
