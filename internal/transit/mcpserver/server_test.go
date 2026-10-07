package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/mcp"
	"github.com/philipparndt/ai-connector/internal/transit"
	"github.com/philipparndt/ai-connector/internal/transit/transittest"
)

// TestMain doubles as the stdio server: the round-trip test runs this test binary with TRANSIT_MCP_HELPER=1, against
// the fake timetable APIs at TRANSIT_FAKE_URL.
func TestMain(m *testing.M) {
	if os.Getenv("TRANSIT_MCP_HELPER") == "1" {
		svc := fakeService(os.Getenv("TRANSIT_FAKE_URL"))
		if err := New(svc, os.Stderr).ServeStdio(context.Background(), os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeService(base string) *transit.Service {
	return transit.NewService(func() time.Time { return transittest.Now }, transit.NewDB(base+"/web/api"),
		transit.NewEFA("efa", base+"/nvbw/"), transit.NewEFA("naldo", base+"/naldo/"))
}

// syncBuffer is a bytes.Buffer safe for the stderr copier and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func checkTools(t *testing.T, ctx context.Context, b *mcp.Toolbox) {
	t.Helper()
	tools, err := b.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
		if tl.Changes {
			t.Errorf("%s is not read-only", tl.Name)
		}
		if len(tl.Description) < 80 || !json.Valid(tl.Parameters) {
			t.Errorf("%s: description or schema missing", tl.Name)
		}
	}
	if got := strings.Join(names, ","); got != "transit__find_stop,transit__departures,transit__journeys,transit__trip" {
		t.Errorf("tools = %s", got)
	}
}

func call(t *testing.T, ctx context.Context, b *mcp.Toolbox, name, args string) (string, bool) {
	t.Helper()
	res, err := b.Call(ctx, name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res.Text, res.IsError
}

func TestStdioRoundTrip(t *testing.T) {
	fake := transittest.Start()
	defer fake.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stderr syncBuffer
	b, err := mcp.Open(ctx, &mcp.Server{Name: "transit", Command: os.Args[0], Args: []string{"-test.run=^$"},
		Env: map[string]string{"TRANSIT_MCP_HELPER": "1", "TRANSIT_FAKE_URL": fake.URL}}, mcp.Options{Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	name, version, proto, kind := b.Client().Info()
	if name != "transit" || version != Version || proto != mcp.LatestProtocol || kind != "stdio" {
		t.Errorf("info = %s %s %s %s", name, version, proto, kind)
	}
	if !strings.Contains(b.Client().Instructions(), "naldo") {
		t.Error("instructions")
	}
	checkTools(t, ctx, b)

	text, isErr := call(t, ctx, b, "transit__find_stop", `{"query":"Mössingen","limit":2}`)
	if isErr || !strings.Contains(text, "Mössingen [id db:8004070]") {
		t.Errorf("find_stop:\n%s", text)
	}
	text, isErr = call(t, ctx, b, "transit__departures", `{"stop":"Tübingen Hbf","products":["train"],"limit":3}`)
	if isErr || !strings.Contains(text, "07:00 +10 (07:10) · RE6a → Stuttgart Hbf · platform 3") ||
		strings.Count(text, "\n") != 5 {
		t.Errorf("departures:\n%s", text)
	}
	text, isErr = call(t, ctx, b, "transit__journeys", `{"from":"Mössingen","to":"Tübingen Hbf","when":"2026-10-05T07:00","results":2}`)
	if isErr || !strings.Contains(text, `from "Mössingen" → Mössingen (db:8004070)`) ||
		!strings.Contains(text, "07:11 Mössingen (pl. 1) → 07:18 Dußlingen (pl. 2) · RB66") {
		t.Errorf("journeys:\n%s", text)
	}
	id := text[strings.Index(text, "[id j-")+4:]
	id = id[:strings.Index(id, "]")]
	text, isErr = call(t, ctx, b, "transit__trip", `{"id":"`+id+`"}`)
	if isErr || !strings.Contains(text, "Journey "+id) {
		t.Errorf("trip %s:\n%s", id, text)
	}
	// errors the model can fix are results, not broken connections
	text, isErr = call(t, ctx, b, "transit__departures", `{"stop":"Tübingen Hbf","when":"tomorrow"}`)
	if !isErr || !strings.Contains(text, "2026-10-05T07:00") {
		t.Errorf("bad when: %v %s", isErr, text)
	}
	text, isErr = call(t, ctx, b, "transit__departures", `{"stop":"x","bogus":1}`)
	if !isErr || !strings.Contains(text, "bogus") {
		t.Errorf("unknown argument: %v %s", isErr, text)
	}
	text, isErr = call(t, ctx, b, "transit__nope", `{}`)
	if !isErr || !strings.Contains(text, "unknown tool") {
		t.Errorf("unknown tool: %v %s", isErr, text)
	}
	if !strings.Contains(stderr.String(), "journeys ") {
		t.Errorf("stderr log: %s", stderr.String())
	}
}

func TestStructuredContent(t *testing.T) {
	fake := transittest.Start()
	defer fake.Close()
	s := New(fakeService(fake.URL), nil)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`not json`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"departures","arguments":{"stop":"db:8000141","limit":2}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON on stdout: %q", line)
		}
		id, _ := json.Marshal(m["id"])
		byID[string(id)] = m
	}
	if v := byID["1"]["result"].(map[string]any)["protocolVersion"]; v != "2024-11-05" {
		t.Errorf("negotiated %v", v)
	}
	if byID["null"]["error"] == nil || byID["3"]["error"] == nil {
		t.Errorf("parse error / unknown method: %v", byID)
	}
	res := byID["2"]["result"].(map[string]any)
	sc, _ := json.Marshal(res["structuredContent"])
	var d transit.DeparturesResult
	if err := json.Unmarshal(sc, &d); err != nil || d.Provider != "db" || len(d.Departures) != 2 {
		t.Fatalf("structuredContent = %s (%v)", sc, err)
	}
	if !strings.HasSuffix(d.Departures[0].Planned.Format(time.RFC3339), "+02:00") {
		t.Errorf("times are not Berlin: %v", d.Departures[0].Planned)
	}
}

func TestHTTP(t *testing.T) {
	fake := transittest.Start()
	defer fake.Close()
	hs := httptest.NewServer(New(fakeService(fake.URL), nil).Handler())
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	b, err := mcp.Open(ctx, &mcp.Server{Name: "transit", URL: hs.URL + "/mcp"}, mcp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, _, _, kind := b.Client().Info(); kind != "http" {
		t.Errorf("transport %s", kind)
	}
	checkTools(t, ctx, b)
	text, isErr := call(t, ctx, b, "transit__departures", `{"stop":"Uni/Neue Aula","products":["bus"],"limit":3}`)
	if isErr || !strings.Contains(text, "source: EFA-BW") {
		t.Errorf("departures over HTTP:\n%s", text)
	}
	// a page of another site must not drive the server (DNS rebinding)
	req, _ := http.NewRequest("POST", hs.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign origin: %v %v", resp.StatusCode, err)
	}
}

func TestPrintConfig(t *testing.T) {
	var out bytes.Buffer
	err := Main(context.Background(), []string{"--print-config"}, []string{"/opt/mlxlab", "mcp", "serve", "transit"},
		nil, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := mcp.ParseConfig(out.Bytes())
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	s := cfg.Servers["transit"]
	if s.Command != "/opt/mlxlab" || strings.Join(s.Args, " ") != "mcp serve transit" || !s.IsReadOnly("journeys") {
		t.Errorf("config = %+v", s)
	}
	if err := Main(context.Background(), []string{"--bogus"}, []string{"x"}, nil, &out, &out); err == nil {
		t.Error("bogus flag accepted")
	}
}
