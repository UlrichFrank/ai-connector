package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"sync"
	"time"
)

// maxJSON caps a JSON answer.
const maxJSON = 32 << 20

// newHTTPClient is the client of the HTTP transports: no overall timeout (answers may be streams; every request
// has a context), but bounded dialing and TLS.
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 10 * time.Second
	return &http.Client{Transport: tr}
}

// httpTransport is the streamable HTTP transport (protocol 2025-03-26 and later): every message is a POST; a
// request's answer is application/json or a text/event-stream that ends with the response. The session id the
// server gives in initialize goes with every later request; DELETE ends the session.
type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client

	mu       sync.Mutex
	session  string
	protocol string
}

func (t *httpTransport) negotiated(v string) {
	t.mu.Lock()
	t.protocol = v
	t.mu.Unlock()
}

func (t *httpTransport) newRequest(ctx context.Context, method string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.url, r)
	if err != nil {
		return nil, err
	}
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	t.mu.Lock()
	if t.session != "" {
		req.Header.Set("Mcp-Session-Id", t.session)
	}
	if t.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", t.protocol)
	}
	t.mu.Unlock()
	return req, nil
}

// post sends m; the caller closes the body of a successful answer.
func (t *httpTransport) post(ctx context.Context, m *message) (*http.Response, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	req, err := t.newRequest(ctx, http.MethodPost, b)
	if err != nil {
		return nil, err
	}
	hadSession := req.Header.Get("Mcp-Session-Id") != ""
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		t.mu.Lock()
		if t.session == "" {
			t.session = id
		}
		t.mu.Unlock()
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound && hadSession {
			return nil, errSessionExpired
		}
		return nil, &HTTPError{Status: resp.StatusCode, Body: snippet(body)}
	}
	return resp, nil
}

func (t *httpTransport) request(ctx context.Context, m *message) (*message, error) {
	resp, err := t.post(ctx, m)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch ct {
	case "application/json":
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxJSON))
		if err != nil {
			return nil, err
		}
		return pick(b, m.ID)
	case "text/event-stream":
		r := newSSEReader(resp.Body)
		for {
			ev, err := r.next()
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if errors.Is(err, io.EOF) {
					return nil, fmt.Errorf("%w: the event stream ended without the response", errClosed)
				}
				return nil, err
			}
			if ev.name != "message" {
				continue
			}
			var x message
			if json.Unmarshal([]byte(ev.data), &x) != nil {
				continue
			}
			switch {
			case x.isResponse() && bytes.Equal(x.ID, m.ID):
				return &x, nil
			case x.isRequest(): // e.g. ping from the server while it works
				go t.reply(answer(&x))
			}
		}
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("unexpected answer (status %d, %q): %s", resp.StatusCode, ct, snippet(b))
	}
}

// pick finds the response to id in a JSON answer (one message or a batch).
func pick(b []byte, id json.RawMessage) (*message, error) {
	b = bytes.TrimSpace(b)
	var all []message
	if len(b) > 0 && b[0] == '[' {
		if err := json.Unmarshal(b, &all); err != nil {
			return nil, err
		}
	} else {
		var m message
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("bad JSON answer: %w: %s", err, snippet(b))
		}
		all = []message{m}
	}
	for i := range all {
		if all[i].isResponse() && bytes.Equal(all[i].ID, id) {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("the answer has no response to request %s", id)
}

func (t *httpTransport) reply(m *message) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if resp, err := t.post(ctx, m); err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
}

func (t *httpTransport) notify(ctx context.Context, m *message) error {
	resp, err := t.post(ctx, m)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.Body.Close()
}

// close ends the session (servers may not support DELETE; that is fine).
func (t *httpTransport) close() error {
	t.mu.Lock()
	session := t.session
	t.mu.Unlock()
	if session == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := t.newRequest(ctx, http.MethodDelete, nil)
	if err != nil {
		return err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil
	}
	resp.Body.Close()
	return nil
}
