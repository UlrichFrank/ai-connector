// Package mcpserver serves internal/transit as an MCP server (JSON-RPC 2.0 over stdio, or streamable HTTP) with the
// read-only tools find_stop, departures, journeys and trip. cmd/transit-mcp and "mlxlab mcp serve transit" run it.
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/transit"
)

// Version of the server.
const Version = "1.0.0"

// Protocols the server speaks, newest first (as internal/mcp's client).
var Protocols = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// CallTimeout bounds one tool call (a fallback from DB to EFA may need two providers).
const CallTimeout = 45 * time.Second

// Server is the MCP server.
type Server struct {
	svc   *transit.Service
	tools []tool
	log   *log.Logger
}

// New makes a server over svc; logs go to logw (nil: discarded).
func New(svc *transit.Service, logw io.Writer) *Server {
	if logw == nil {
		logw = io.Discard
	}
	return &Server{svc: svc, tools: tools(), log: log.New(logw, "transit-mcp: ", log.LstdFlags)}
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// handle answers one message; nil for notifications and responses.
func (s *Server) handle(ctx context.Context, m *message) *message {
	if m.JSONRPC != "2.0" {
		if len(m.ID) == 0 {
			return nil
		}
		return errorMsg(m.ID, codeInvalidRequest, "jsonrpc must be 2.0")
	}
	if len(m.ID) == 0 || m.Method == "" {
		return nil // a notification (initialized, cancelled) or a response to us
	}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		v := Protocols[0]
		for _, x := range Protocols {
			if x == p.ProtocolVersion {
				v = x
			}
		}
		return result(m.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "transit", "title": "Public transport timetables", "version": Version},
			"instructions":    Instructions,
		})
	case "ping":
		return result(m.ID, map[string]any{})
	case "tools/list":
		var list []map[string]any
		for _, t := range s.tools {
			list = append(list, t.listing())
		}
		return result(m.ID, map[string]any{"tools": list})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return errorMsg(m.ID, codeInvalidParams, "bad params")
		}
		for _, t := range s.tools {
			if t.Name == p.Name {
				return result(m.ID, s.call(ctx, t, p.Arguments))
			}
		}
		return errorMsg(m.ID, codeInvalidParams, "unknown tool: "+p.Name)
	}
	return errorMsg(m.ID, codeMethodNotFound, "method not found: "+m.Method)
}

// call runs a tool; failures are a result with isError (the model can read and fix them).
func (s *Server) call(ctx context.Context, t tool, args json.RawMessage) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	start := time.Now()
	text, structured, err := t.run(ctx, s.svc, args)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			msg = "the timetable servers did not answer in time; try again or another provider"
		}
		s.log.Printf("%s %s: error after %v: %s", t.Name, compact(args), time.Since(start).Round(time.Millisecond), msg)
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "Error: " + msg}}, "isError": true}
	}
	s.log.Printf("%s %s: ok in %v", t.Name, compact(args), time.Since(start).Round(time.Millisecond))
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "structuredContent": structured}
}

func compact(b json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return "?"
	}
	s := buf.String()
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func result(id json.RawMessage, v any) *message {
	b, err := json.Marshal(v)
	if err != nil {
		return errorMsg(id, -32603, "internal error: "+err.Error())
	}
	return &message{JSONRPC: "2.0", ID: id, Result: b}
}

func errorMsg(id json.RawMessage, code int, msg string) *message {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &message{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// --- stdio

// maxLine caps one message on stdin.
const maxLine = 4 << 20

// ServeStdio reads newline-delimited JSON-RPC from r and writes the answers to w until r ends (or ctx is done).
// Calls run concurrently (at most 4 at a time); notifications/cancelled cancels a running call.
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wmu      sync.Mutex
		wg       sync.WaitGroup
		mu       sync.Mutex
		inflight = map[string]context.CancelFunc{}
		sem      = make(chan struct{}, 4)
	)
	write := func(m *message) {
		b, err := json.Marshal(m)
		if err != nil {
			return
		}
		wmu.Lock()
		defer wmu.Unlock()
		_, _ = w.Write(append(b, '\n'))
	}
	br := bufio.NewReaderSize(r, 64<<10)
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		defer close(lines)
		for {
			line, err := readLine(br)
			if len(bytes.TrimSpace(line)) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					readErr <- err
				}
				return
			}
		}
	}()
	for {
		var line []byte
		var ok bool
		select {
		case line, ok = <-lines:
		case <-ctx.Done():
			ok = false
		}
		if !ok {
			break
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			write(errorMsg(nil, codeParse, "parse error"))
			continue
		}
		if m.Method == "notifications/cancelled" {
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			_ = json.Unmarshal(m.Params, &p)
			mu.Lock()
			if c := inflight[string(p.RequestID)]; c != nil {
				c()
			}
			mu.Unlock()
			continue
		}
		if m.Method != "tools/call" {
			if resp := s.handle(ctx, &m); resp != nil {
				write(resp)
			}
			continue
		}
		cctx, ccancel := context.WithCancel(ctx)
		key := string(m.ID)
		mu.Lock()
		inflight[key] = ccancel
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer ccancel()
			sem <- struct{}{}
			defer func() { <-sem }()
			resp := s.handle(cctx, &m)
			mu.Lock()
			delete(inflight, key)
			mu.Unlock()
			// a call the client cancelled gets no answer (as the spec says)
			if resp != nil && cctx.Err() == nil {
				write(resp)
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-readErr:
		return err
	default:
	}
	return nil
}

// readLine reads one line, refusing lines longer than maxLine.
func readLine(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > maxLine {
			// skip the rest of the line
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			return []byte("{}"), err
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return out, err
	}
}

// --- streamable HTTP

// Handler serves streamable HTTP (POST a JSON-RPC message, the answer is application/json; sessions by
// Mcp-Session-Id; DELETE ends one). Requests with an Origin header other than localhost are refused (DNS
// rebinding).
func (s *Server) Handler() http.Handler {
	h := &httpHandler{s: s, sessions: map[string]time.Time{}}
	return h
}

type httpHandler struct {
	s        *Server
	mu       sync.Mutex
	sessions map[string]time.Time
}

func localOrigin(o string) bool {
	if o == "" {
		return true
	}
	for _, p := range []string{"http://localhost", "http://127.0.0.1", "http://[::1]", "https://localhost",
		"https://127.0.0.1"} {
		if o == p || strings.HasPrefix(o, p+":") {
			return true
		}
	}
	return false
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !localOrigin(r.Header.Get("Origin")) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		h.mu.Lock()
		delete(h.sessions, sid)
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "this server has no event stream: POST JSON-RPC messages", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLine+1))
	if err != nil || len(body) > maxLine {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		writeJSON(w, http.StatusBadRequest, errorMsg(nil, codeParse, "parse error (one JSON-RPC message per POST)"))
		return
	}
	h.mu.Lock()
	if m.Method == "initialize" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		sid = hex.EncodeToString(b)
		h.sessions[sid] = time.Now()
		h.gc()
		w.Header().Set("Mcp-Session-Id", sid)
	} else if sid != "" {
		if _, ok := h.sessions[sid]; !ok {
			h.mu.Unlock()
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		h.sessions[sid] = time.Now()
	}
	h.mu.Unlock()
	resp := h.s.handle(r.Context(), &m)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// gc forgets sessions idle for a day (called with mu held).
func (h *httpHandler) gc() {
	for id, t := range h.sessions {
		if time.Since(t) > 24*time.Hour {
			delete(h.sessions, id)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// ConfigSnippet is a ready-to-paste mcp.json for running the server with command and args.
func ConfigSnippet(command string, args []string) string {
	type server struct {
		Command  string   `json:"command"`
		Args     []string `json:"args"`
		ReadOnly []string `json:"readOnly"`
	}
	if args == nil {
		args = []string{}
	}
	cfg := map[string]any{"servers": map[string]any{"transit": server{command, args, []string{"*"}}}}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return string(b) + "\n"
}

// errUsage is a usage error.
var errUsage = errors.New("usage")

// Main runs the server with the command line args (after the command name): stdio, or --http ADDR, or
// --print-config. self are the command and the args that start this server (for --print-config).
func Main(ctx context.Context, args []string, self []string, stdin io.Reader, stdout, stderr io.Writer) error {
	var addr string
	printConfig := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--http" || a == "-http":
			if i+1 >= len(args) {
				return fmt.Errorf("%w: --http needs an address like :8790 or 127.0.0.1:8790", errUsage)
			}
			addr = args[i+1]
			i++
		case strings.HasPrefix(a, "--http="):
			addr = strings.TrimPrefix(a, "--http=")
		case a == "--print-config" || a == "-print-config":
			printConfig = true
		case a == "-h" || a == "--help" || a == "help":
			fmt.Fprintf(stderr, "usage: %s [--http ADDR] [--print-config]\n\n"+
				"  (no flags)      MCP over stdio (for mcp.json \"command\")\n"+
				"  --http ADDR     MCP over streamable HTTP at http://ADDR/mcp (\":8790\" listens on 127.0.0.1 only)\n"+
				"  --print-config  print an mcp.json snippet that runs this server\n", strings.Join(self, " "))
			return nil
		default:
			return fmt.Errorf("%w: unknown argument %q", errUsage, a)
		}
	}
	if printConfig {
		_, err := io.WriteString(stdout, ConfigSnippet(self[0], self[1:]))
		return err
	}
	s := New(transit.Default(), stderr)
	if addr == "" {
		return s.ServeStdio(ctx, stdin, stdout)
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.Handler())
	mux.Handle("/", s.Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	fmt.Fprintf(stderr, "transit MCP server on http://%s/mcp\n", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
