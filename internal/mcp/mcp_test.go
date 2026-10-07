package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/agent"
)

func TestParseConfig(t *testing.T) {
	c, err := ParseConfig([]byte(`{"servers":{
		"vboard":{"url":"https://v/mcp","headers":{"Authorization":"Bearer ${T}"}},
		"smart-home":{"command":"sh-mcp","readOnly":["get_*","list_*"]},
		"old":{"url":"http://x/sse","type":"sse","disabled":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range c.Enabled() {
		names = append(names, s.Name+":"+s.transport())
	}
	if got := strings.Join(names, ","); got != "smart-home:stdio,vboard:http" {
		t.Fatalf("enabled = %s", got)
	}
	sh := c.Servers["smart-home"]
	if !sh.IsReadOnly("get_lights") || !sh.IsReadOnly("list_rooms") || sh.IsReadOnly("set_light") {
		t.Fatal("readOnly patterns")
	}
	for _, bad := range []string{
		`{"servers":{"a":{"url":"https://x","command":"y"}}}`,
		`{"servers":{"a":{}}}`,
		`{"servers":{"a__b":{"command":"y"}}}`,
		`{"servers":{"a b":{"command":"y"}}}`,
		`{"servers":{"a":{"command":"y","readOnlyy":["x"]}}}`, // typo: unknown field
		`{"servers":{"a":{"command":"y","readOnly":["[x"]}}}`,
		`{"servers":{"a":{"url":"ftp://x"}}}`,
		`{"servers":{"a":{"type":"ws","url":"https://x"}}}`,
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestExampleConfig(t *testing.T) {
	b, err := os.ReadFile("../../docs/mcp.example.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if v := c.Servers["vboard"]; v == nil || v.transport() != "http" || !v.IsReadOnly("search_cards") {
		t.Fatalf("vboard: %+v", v)
	}
}

func TestLoadConfigPermissions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mcp.json")
	if c, err := LoadConfig(p); err != nil || len(c.Servers) != 0 {
		t.Fatalf("missing file: %v %v", c, err)
	}
	os.WriteFile(p, []byte(`{"servers":{"a":{"command":"x"}}}`), 0o644)
	os.Chmod(p, 0o644)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("group/world readable accepted: %v", err)
	}
	os.Chmod(p, 0o600)
	if c, err := LoadConfig(p); err != nil || c.Servers["a"] == nil {
		t.Fatalf("0600: %v", err)
	}
}

func TestExpand(t *testing.T) {
	env := map[string]string{"TOKEN": "abc"}
	get := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	if v, err := expand("Bearer ${TOKEN}", get); err != nil || v != "Bearer abc" {
		t.Fatal(v, err)
	}
	if v, _ := expand("a$b ${", get); v != "a$b ${" {
		t.Fatal("other $ must stay:", v)
	}
	if _, err := expand("${NOPE}", get); err == nil || !strings.Contains(err.Error(), "$NOPE is not set") {
		t.Fatal(err)
	}
}

func TestSSEReader(t *testing.T) {
	r := newSSEReader(strings.NewReader(": hi\n\nevent: endpoint\ndata: /m\n\ndata: a\r\ndata: b\r\n\r\ndata: last"))
	want := []event{{"endpoint", "/m"}, {"message", "a\nb"}, {"message", "last"}}
	for _, w := range want {
		ev, err := r.next()
		if err != nil || ev != w {
			t.Fatalf("got %+v %v, want %+v", ev, err, w)
		}
	}
	if _, err := r.next(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

func httpServer(t *testing.T, sse bool) (*httpFake, *Server) {
	h := newHTTPFake(sse)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	t.Setenv("FAKE_TOKEN", "secret")
	return h, &Server{Name: "vboard", URL: ts.URL + "/mcp", Headers: map[string]string{"Authorization": "Bearer ${FAKE_TOKEN}"},
		ReadOnly: []string{"get_*"}}
}

func open(t *testing.T, s *Server) *Toolbox {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := Open(ctx, s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// checkToolbox runs the common checks against a toolbox of the fake.
func checkToolbox(t *testing.T, b *Toolbox, prefix string) {
	t.Helper()
	ctx := context.Background()
	tools, err := b.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != len(fakeTools) {
		t.Fatalf("%d tools, want %d (pagination)", len(tools), len(fakeTools))
	}
	byName := map[string]agent.Tool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
		if tl.Zone != agent.ZoneHome || tl.Untrusted {
			t.Errorf("%s: zone %q untrusted %v", tl.Name, tl.Zone, tl.Untrusted)
		}
		if !json.Valid(tl.Parameters) {
			t.Errorf("%s: parameters %s", tl.Name, tl.Parameters)
		}
	}
	changes := map[string]bool{"search_cards": false, "get_card": false, "create_card": true, "echo": true}
	for name, want := range changes {
		tl, ok := byName[prefix+"__"+name]
		if !ok {
			t.Fatalf("no %s__%s in %v", prefix, name, tools)
		}
		if tl.Changes != want {
			t.Errorf("%s: changes %v, want %v", tl.Name, tl.Changes, want)
		}
	}
	if s := byName[prefix+"__search_cards"]; !strings.Contains(string(s.Parameters), `"q"`) || s.Description != "Search the cards." {
		t.Errorf("schema/description not passed through: %+v", s)
	}

	res, err := b.Call(ctx, prefix+"__echo", json.RawMessage(`{"x":1}`))
	if err != nil || res.IsError || res.Text != `args {"x":1}` {
		t.Fatalf("echo: %+v %v", res, err)
	}
	res, err = b.Call(ctx, prefix+"__get_card", nil)
	if err != nil || res.Text != `args {}` {
		t.Fatalf("no args: %+v %v", res, err)
	}
	res, err = b.Call(ctx, prefix+"__img", nil)
	if err != nil || res.Text != "here\n[image image/png, 3 bytes, not shown]\ncard one" {
		t.Fatalf("img: %q %v", res.Text, err)
	}
	res, err = b.Call(ctx, prefix+"__fail", nil)
	if err != nil || !res.IsError || res.Text != "no such card" {
		t.Fatalf("fail: %+v %v", res, err)
	}
	res, err = b.Call(ctx, prefix+"__nope", nil)
	if err != nil || !res.IsError || !strings.Contains(res.Text, "unknown tool nope") {
		t.Fatalf("unknown tool: %+v %v", res, err)
	}
	if _, err := b.Call(ctx, "other__echo", nil); err == nil {
		t.Fatal("a tool of another server was accepted")
	}
	if err := b.Client().Ping(ctx); err != nil {
		t.Fatal(err)
	}

	// a timeout ends the call, and the connection stays usable
	tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := b.Call(tctx, prefix+"__slow", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the timeout took %v", d)
	}
	if res, err := b.Call(ctx, prefix+"__echo", nil); err != nil || res.Text != "args {}" {
		t.Fatalf("after timeout: %+v %v", res, err)
	}
}

func TestStreamableHTTPJSON(t *testing.T) {
	h, s := httpServer(t, false)
	h.f.pageSize = 2
	b := open(t, s)
	checkToolbox(t, b, "vboard")
	_, _, proto, kind := b.Client().Info()
	if proto != LatestProtocol || kind != "http" {
		t.Fatalf("protocol %s transport %s", proto, kind)
	}
	if h.versionHdr != LatestProtocol {
		t.Fatalf("MCP-Protocol-Version header %q", h.versionHdr)
	}
	b.Close()
	if h.deletes != 1 {
		t.Fatalf("%d DELETEs on close", h.deletes)
	}
}

func TestStreamableHTTPEventStream(t *testing.T) {
	h, s := httpServer(t, true)
	h.f.pageSize = 4
	checkToolbox(t, open(t, s), "vboard")
}

func TestProtocolNegotiation(t *testing.T) {
	h, s := httpServer(t, false)
	h.f.protocol = "2025-03-26"
	b := open(t, s)
	if _, _, proto, _ := b.Client().Info(); proto != "2025-03-26" {
		t.Fatal(proto)
	}
	h.f.protocol = "1999-01-01"
	if _, err := Open(context.Background(), s, Options{}); err == nil || !strings.Contains(err.Error(), "1999-01-01") {
		t.Fatalf("unknown version accepted: %v", err)
	}
}

func TestUnauthorized(t *testing.T) {
	_, s := httpServer(t, false)
	s.Headers = map[string]string{"Authorization": "Bearer wrong"}
	_, err := Open(context.Background(), s, Options{})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 401 || !strings.Contains(err.Error(), "Authorization") {
		t.Fatalf("got %v", err)
	}
	s.Headers = map[string]string{"Authorization": "Bearer ${MISSING_TOKEN_X}"}
	if _, err := Open(context.Background(), s, Options{}); err == nil || !strings.Contains(err.Error(), "MISSING_TOKEN_X") {
		t.Fatalf("got %v", err)
	}
}

func TestSessionExpiredReconnects(t *testing.T) {
	h, s := httpServer(t, false)
	b := open(t, s)
	ctx := context.Background()
	h.expire()
	if _, err := b.Client().ListTools(ctx); err != nil {
		t.Fatal(err)
	}
	h.expire()
	if res, err := b.Call(ctx, "vboard__echo", nil); err != nil || res.Text != "args {}" {
		t.Fatalf("call after expiry: %+v %v", res, err)
	}
	if h.f.inits != 3 {
		t.Fatalf("%d initializes, want 3", h.f.inits)
	}
}

func TestBrokenConnectionNotRetriedForCalls(t *testing.T) {
	if !retryable("tools/list", io.ErrUnexpectedEOF) || retryable("tools/call", io.ErrUnexpectedEOF) {
		t.Fatal("tools/call may change something: only retried when it surely did not run")
	}
	if !retryable("tools/call", errSessionExpired) || !retryable("tools/call", &net.OpError{Op: "dial", Err: io.EOF}) {
		t.Fatal("expired session / failed dial must be retried")
	}
	if retryable("tools/list", context.DeadlineExceeded) || retryable("tools/list", &RPCError{Code: 1}) {
		t.Fatal("no retry for timeouts and server errors")
	}
}

func TestPaginationLoop(t *testing.T) {
	h, s := httpServer(t, false)
	b := open(t, s)
	h.f.badPages = true
	if _, err := b.Client().ListTools(context.Background()); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("cursor loop not stopped: %v", err)
	}
}

func TestLegacySSEFallback(t *testing.T) {
	l := &legacyFake{f: &fake{pageSize: 5}}
	ts := httptest.NewServer(l)
	t.Cleanup(ts.Close)
	b := open(t, &Server{Name: "old", URL: ts.URL + "/sse", ReadOnly: []string{"get_*"}})
	if _, _, _, kind := b.Client().Info(); kind != "sse" {
		t.Fatalf("transport %s", kind)
	}
	checkToolbox(t, b, "old")
}

func TestSSEEndpointOtherOrigin(t *testing.T) {
	ts := httptest.NewServer(&legacyFake{f: &fake{}})
	t.Cleanup(ts.Close)
	tr := &sseTransport{url: ts.URL + "/sse"}
	if err := tr.setEndpoint("https://evil.example/messages"); err == nil {
		t.Fatal("endpoint on another origin accepted (the token would go there)")
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func stdioServer(t *testing.T) *Server {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Name: "home", Command: exe, Args: []string{"-test.run=^$"},
		Env: map[string]string{"MCP_FAKE_STDIO": "${FAKE_FLAG}"}, ReadOnly: []string{"get_*"}}
}

func TestStdio(t *testing.T) {
	t.Setenv("FAKE_FLAG", "1")
	var stderr lockedBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := Open(ctx, stdioServer(t), Options{Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	checkToolbox(t, b, "home")
	if _, _, _, kind := b.Client().Info(); kind != "stdio" {
		t.Fatal(kind)
	}
	start := time.Now()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("close took %v", d)
	}
	if s := stderr.String(); !strings.Contains(s, "fake stdio server up") || !strings.Contains(s, "not JSON-RPC") {
		t.Fatalf("stderr log: %q", s)
	}
	if _, err := b.Call(context.Background(), "home__echo", nil); err == nil {
		t.Fatal("call after close")
	}
}

func TestStdioServerExits(t *testing.T) {
	_, err := Open(context.Background(), &Server{Name: "x", Command: "/bin/sh", Args: []string{"-c", "exit 3"}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "exit") {
		t.Fatalf("got %v", err)
	}
	_, err = Open(context.Background(), &Server{Name: "x", Command: "/nonexistent/mcp"}, Options{})
	if err == nil {
		t.Fatal("missing command accepted")
	}
}

func TestToolboxes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MLXLAB_HOME", home)
	t.Setenv("FAKE_FLAG", "1")
	if boxes, err := Toolboxes(context.Background()); err != nil || len(boxes) != 0 {
		t.Fatalf("no mcp.json: %v %v", boxes, err)
	}
	_, hs := httpServer(t, false)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + ln.Addr().String() + "/mcp"
	ln.Close()
	cfg := map[string]any{"servers": map[string]any{
		"vboard": map[string]any{"url": hs.URL, "headers": hs.Headers, "readOnly": []string{"get_*"}},
		"home":   map[string]any{"command": stdioServer(t).Command, "args": []string{"-test.run=^$"}, "env": map[string]string{"MCP_FAKE_STDIO": "1"}},
		"off":    map[string]any{"url": dead, "disabled": true},
		"dead":   map[string]any{"url": dead},
	}}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(ConfigPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	boxes, err := Toolboxes(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dead") || strings.Contains(err.Error(), "off") {
		t.Fatalf("error: %v", err)
	}
	if len(boxes) != 2 {
		t.Fatalf("%d toolboxes", len(boxes))
	}
	for _, b := range boxes {
		tools, _ := b.Tools(context.Background())
		if len(tools) != len(fakeTools) {
			t.Fatalf("%d tools", len(tools))
		}
		b.Close()
	}
	if log, _ := os.ReadFile(LogPath("home")); !strings.Contains(string(log), "fake stdio server up") {
		t.Fatalf("stdio log: %q", log)
	}
}
