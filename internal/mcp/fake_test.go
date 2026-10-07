package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain doubles as a fake stdio MCP server: the test runs its own binary with MCP_FAKE_STDIO=1.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_FAKE_STDIO") == "1" {
		runStdioFake()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fake is an MCP server in memory.
type fake struct {
	mu       sync.Mutex
	protocol string // the version it answers; "" = the one asked for
	pageSize int    // tools per tools/list page (0 = all)
	badPages bool   // hand out the same cursor forever
	inits    int
	called   []string
}

var fakeTools = []map[string]any{
	{"name": "search_cards", "description": "Search the cards.", "annotations": map[string]any{"readOnlyHint": true},
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}},
	{"name": "get_card", "description": "One card."},
	{"name": "create_card", "description": "Make a card.", "annotations": map[string]any{"readOnlyHint": false}},
	{"name": "echo", "description": "Echo."},
	{"name": "img", "description": "A picture."},
	{"name": "fail", "description": "Fails."},
	{"name": "slow", "description": "Takes long."},
}

func (f *fake) handle(ctx context.Context, m *message) *message {
	if len(m.ID) == 0 {
		return nil // notification
	}
	r := &message{JSONRPC: "2.0", ID: m.ID}
	ok := func(v any) *message {
		r.Result, _ = json.Marshal(v)
		return r
	}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		f.mu.Lock()
		f.inits++
		v := f.protocol
		f.mu.Unlock()
		if v == "" {
			v = p.ProtocolVersion
		}
		return ok(map[string]any{"protocolVersion": v, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "fake", "version": "0.1"}})
	case "ping":
		return ok(map[string]any{})
	case "tools/list":
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(m.Params, &p)
		start, _ := strconv.Atoi(p.Cursor)
		end := len(fakeTools)
		if f.pageSize > 0 && start+f.pageSize < end {
			end = start + f.pageSize
		}
		res := map[string]any{"tools": fakeTools[start:end]}
		if end < len(fakeTools) {
			res["nextCursor"] = strconv.Itoa(end)
		}
		if f.badPages {
			res["nextCursor"] = "again"
			res["tools"] = fakeTools[:1]
		}
		return ok(res)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(m.Params, &p)
		f.mu.Lock()
		f.called = append(f.called, p.Name)
		f.mu.Unlock()
		text := func(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }
		switch p.Name {
		case "echo", "get_card", "search_cards", "create_card":
			return ok(map[string]any{"content": text("args " + string(p.Arguments))})
		case "img":
			return ok(map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "here"},
				map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"},
				map[string]any{"type": "resource", "resource": map[string]any{"uri": "card://1", "text": "card one"}},
			}})
		case "fail":
			return ok(map[string]any{"content": text("no such card"), "isError": true})
		case "slow":
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
			return ok(map[string]any{"content": text("late")})
		}
		r.Error = &RPCError{Code: -32602, Message: "unknown tool " + p.Name}
		return r
	}
	r.Error = &RPCError{Code: -32601, Message: "method not found"}
	return r
}

// httpFake serves the fake over streamable HTTP (answers as JSON or as an event stream), with sessions.
type httpFake struct {
	f   *fake
	sse bool

	mu         sync.Mutex
	sessions   map[string]bool
	next       int
	deletes    int
	versionHdr string // the MCP-Protocol-Version of the last request after initialize
}

func newHTTPFake(sse bool) *httpFake {
	return &httpFake{f: &fake{}, sse: sse, sessions: map[string]bool{}}
}

func (h *httpFake) expire() {
	h.mu.Lock()
	h.sessions = map[string]bool{}
	h.mu.Unlock()
}

func (h *httpFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer secret" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")
	switch r.Method {
	case http.MethodDelete:
		h.mu.Lock()
		delete(h.sessions, sid)
		h.deletes++
		h.mu.Unlock()
		return
	case http.MethodPost:
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var m message
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	if m.Method == "initialize" {
		h.next++
		sid = fmt.Sprintf("s%d", h.next)
		h.sessions[sid] = true
		w.Header().Set("Mcp-Session-Id", sid)
	} else if !h.sessions[sid] {
		h.mu.Unlock()
		http.Error(w, "no session", http.StatusNotFound)
		return
	} else {
		h.versionHdr = r.Header.Get("MCP-Protocol-Version")
	}
	h.mu.Unlock()
	if m.isResponse() { // our answer to the server's ping
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := h.f.handle(r.Context(), &m)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	b, _ := json.Marshal(resp)
	if !h.sse {
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fl := w.(http.Flusher)
	// a notification and a request of the server before the answer, the answer in many data lines
	fmt.Fprint(w, ": keep-alive\n\n")
	fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
	fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"srv-1\",\"method\":\"ping\"}\n\n")
	fl.Flush()
	b, _ = json.MarshalIndent(resp, "", " ")
	fmt.Fprint(w, "id: 1\r\nevent: message\r\n")
	for _, line := range strings.Split(string(b), "\n") {
		fmt.Fprintf(w, "data: %s\r\n", line)
	}
	fmt.Fprint(w, "\r\n")
	fl.Flush()
}

// legacyFake serves the fake over the HTTP+SSE transport of 2024-11-05; POST to the stream URL is 405, as an old
// server answers the streamable HTTP attempt.
type legacyFake struct {
	f      *fake
	mu     sync.Mutex
	stream chan []byte
}

func (l *legacyFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/sse" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		ch := make(chan []byte, 16)
		l.mu.Lock()
		l.stream = ch
		l.mu.Unlock()
		fmt.Fprint(w, "event: endpoint\ndata: /messages?sessionId=1\n\n")
		fl.Flush()
		for {
			select {
			case b := <-ch:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	case r.URL.Path == "/messages" && r.Method == http.MethodPost:
		var m message
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		go func() {
			if resp := l.f.handle(context.Background(), &m); resp != nil {
				b, _ := json.Marshal(resp)
				l.mu.Lock()
				ch := l.stream
				l.mu.Unlock()
				ch <- b
			}
		}()
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

// runStdioFake is the fake as a stdio server.
func runStdioFake() {
	f := &fake{pageSize: 3}
	fmt.Fprintln(os.Stderr, "fake stdio server up")
	fmt.Println("this is not JSON-RPC")
	var wmu sync.Mutex
	in := bufio.NewReader(os.Stdin)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			var m message
			if json.Unmarshal(line, &m) == nil {
				go func() {
					if r := f.handle(ctx, &m); r != nil {
						b, _ := json.Marshal(r)
						wmu.Lock()
						os.Stdout.Write(append(b, '\n'))
						wmu.Unlock()
					}
				}()
			}
		}
		if err == io.EOF || err != nil {
			return
		}
	}
}
