package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
)

// LatestProtocol is the MCP version this client asks for; Protocols are those it accepts from a server.
const LatestProtocol = "2025-11-25"

var Protocols = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

func supported(v string) bool {
	for _, p := range Protocols {
		if p == v {
			return true
		}
	}
	return false
}

// message is a JSON-RPC 2.0 request, notification (no id) or response.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func (m *message) isResponse() bool { return m.Method == "" && len(m.ID) > 0 }
func (m *message) isRequest() bool  { return m.Method != "" && len(m.ID) > 0 }

// RPCError is an error the server answered.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	s := fmt.Sprintf("mcp error %d: %s", e.Code, e.Message)
	if len(e.Data) > 0 && string(e.Data) != "null" {
		s += " (" + string(e.Data) + ")"
	}
	return s
}

// HTTPError is an HTTP status other than the expected ones.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	s := fmt.Sprintf("http status %d", e.Status)
	switch e.Status {
	case 401, 403:
		s += " (check the Authorization header in mcp.json)"
	}
	if e.Body != "" {
		s += ": " + e.Body
	}
	return s
}

var (
	// errSessionExpired: the server forgot our session (HTTP 404 with a session id); a new one is needed.
	errSessionExpired = errors.New("mcp session expired")
	// errClosed: the connection is gone (the stream ended, the process exited, Close was called).
	errClosed = errors.New("mcp connection closed")
)

// transport carries messages to one server and back.
type transport interface {
	// request sends a request and waits for its response.
	request(ctx context.Context, m *message) (*message, error)
	// notify sends a notification.
	notify(ctx context.Context, m *message) error
	// negotiated tells the transport the protocol version agreed on in initialize.
	negotiated(version string)
	close() error
}

// mux matches the responses of an asynchronous transport (stdio, HTTP+SSE) to the waiting requests, and answers the
// server's own requests (ping; everything else is "method not found": this client offers no sampling, roots or
// elicitation).
type mux struct {
	mu      sync.Mutex
	pending map[string]chan *message
	err     error // set once the connection is gone
	reply   func(m *message)
}

func newMux(reply func(*message)) *mux {
	return &mux{pending: map[string]chan *message{}, reply: reply}
}

// wait registers a request id; the channel gets its response.
func (x *mux) wait(id json.RawMessage) (chan *message, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.err != nil {
		return nil, x.err
	}
	ch := make(chan *message, 1)
	x.pending[string(id)] = ch
	return ch, nil
}

func (x *mux) forget(id json.RawMessage) {
	x.mu.Lock()
	delete(x.pending, string(id))
	x.mu.Unlock()
}

// deliver handles one message from the server.
func (x *mux) deliver(m *message) {
	switch {
	case m.isResponse():
		x.mu.Lock()
		ch := x.pending[string(m.ID)]
		delete(x.pending, string(m.ID))
		x.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	case m.isRequest():
		if x.reply != nil {
			go x.reply(answer(m))
		}
	}
	// notifications (progress, logging, list_changed) are not used
}

// fail ends the connection: every waiting request gets err.
func (x *mux) fail(err error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.err == nil {
		x.err = err
	}
	for id, ch := range x.pending {
		close(ch)
		delete(x.pending, id)
	}
}

func (x *mux) failure() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.err == nil {
		return errClosed
	}
	return x.err
}

// await waits for the response on ch.
func (x *mux) await(ctx context.Context, id json.RawMessage, ch chan *message) (*message, error) {
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, x.failure()
		}
		return r, nil
	case <-ctx.Done():
		x.forget(id)
		return nil, ctx.Err()
	}
}

// answer is the client's response to a request of the server.
func answer(m *message) *message {
	r := &message{JSONRPC: "2.0", ID: m.ID}
	if m.Method == "ping" {
		r.Result = json.RawMessage(`{}`)
	} else {
		r.Error = &RPCError{Code: -32601, Message: "method not found: " + m.Method}
	}
	return r
}

// retryable says a request may be sent again on a new connection: the server certainly did not run it (an expired
// session, a failed dial), or running it twice is harmless (everything but tools/call).
func retryable(method string, err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var rpc *RPCError
	var he *HTTPError
	if errors.As(err, &rpc) || errors.As(err, &he) {
		return false
	}
	if errors.Is(err, errSessionExpired) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return method != "tools/call"
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
