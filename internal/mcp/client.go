package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Default timeouts (a context deadline of the caller wins when it is shorter).
const (
	ConnectTimeout = 20 * time.Second
	CallTimeout    = 60 * time.Second
	ListTimeout    = 30 * time.Second
)

// Client is a connection to one MCP server.
type Client struct {
	srv    *Server
	stderr io.Writer
	http   *http.Client

	mu        sync.Mutex
	t         transport
	transport string // "http", "sse" or "stdio" (what is in use)
	init      initResult
	closed    bool

	nextID atomic.Int64
}

type initResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    struct {
		Tools *struct {
			ListChanged bool `json:"listChanged"`
		} `json:"tools"`
	} `json:"capabilities"`
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	Instructions string `json:"instructions"`
}

// Options of Connect.
type Options struct {
	// Stderr gets a stdio server's stderr (nil: discarded).
	Stderr io.Writer
	// HTTPClient for the HTTP transports (nil: a default with dial and TLS timeouts).
	HTTPClient *http.Client
}

// Connect starts or reaches the server and runs initialize.
func Connect(ctx context.Context, s *Server, o Options) (*Client, error) {
	c := &Client{srv: s, stderr: o.Stderr, http: o.HTTPClient}
	if c.http == nil {
		c.http = newHTTPClient()
	}
	ctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	t, kind, res, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Name, err)
	}
	c.t, c.transport, c.init = t, kind, res
	return c, nil
}

// dial makes a transport and initializes it; streamable HTTP falls back to HTTP+SSE when the server answers the
// POST with a 4xx (as the spec says for backwards compatibility).
func (c *Client) dial(ctx context.Context) (transport, string, initResult, error) {
	switch c.srv.transport() {
	case "stdio":
		t, err := startStdio(c.srv, c.stderr)
		if err != nil {
			return nil, "", initResult{}, err
		}
		res, err := c.initialize(ctx, t)
		if err != nil {
			_ = t.close()
			return nil, "", initResult{}, err
		}
		return t, "stdio", res, nil
	case "sse":
		return c.dialSSE(ctx)
	}
	headers, err := expandAll("header", c.srv.Headers)
	if err != nil {
		return nil, "", initResult{}, err
	}
	t := &httpTransport{url: c.srv.URL, headers: headers, client: c.http}
	res, err := c.initialize(ctx, t)
	if err == nil {
		return t, "http", res, nil
	}
	_ = t.close()
	var he *HTTPError
	if errors.As(err, &he) && he.Status >= 400 && he.Status < 500 && he.Status != 401 && he.Status != 403 {
		st, kind, res, err2 := c.dialSSE(ctx)
		if err2 == nil {
			return st, kind, res, nil
		}
		return nil, "", initResult{}, fmt.Errorf("streamable HTTP: %v; HTTP+SSE: %v", err, err2)
	}
	return nil, "", initResult{}, err
}

func (c *Client) dialSSE(ctx context.Context) (transport, string, initResult, error) {
	headers, err := expandAll("header", c.srv.Headers)
	if err != nil {
		return nil, "", initResult{}, err
	}
	t, err := dialSSE(ctx, c.srv.URL, headers, c.http)
	if err != nil {
		return nil, "", initResult{}, err
	}
	res, err := c.initialize(ctx, t)
	if err != nil {
		_ = t.close()
		return nil, "", initResult{}, err
	}
	return t, "sse", res, nil
}

// initialize negotiates the protocol: we ask for the latest, the server answers the one it speaks.
func (c *Client) initialize(ctx context.Context, t transport) (initResult, error) {
	var res initResult
	params := map[string]any{
		"protocolVersion": LatestProtocol,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "mlxlab", "version": "1.0"},
	}
	if err := c.roundTrip(ctx, t, "initialize", params, &res); err != nil {
		return res, fmt.Errorf("initialize: %w", err)
	}
	if !supported(res.ProtocolVersion) {
		return res, fmt.Errorf("initialize: the server speaks MCP %q; this client knows %s", res.ProtocolVersion,
			strings.Join(Protocols, ", "))
	}
	t.negotiated(res.ProtocolVersion)
	if err := t.notify(ctx, &message{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		return res, fmt.Errorf("notifications/initialized: %w", err)
	}
	return res, nil
}

func (c *Client) roundTrip(ctx context.Context, t transport, method string, params, out any) error {
	m := &message{JSONRPC: "2.0", ID: json.RawMessage(strconv.FormatInt(c.nextID.Add(1), 10)), Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		m.Params = b
	}
	r, err := t.request(ctx, m)
	if err != nil {
		return err
	}
	if r.Error != nil {
		return r.Error
	}
	if out != nil {
		if len(r.Result) == 0 {
			return errors.New("empty result")
		}
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("bad result: %w", err)
		}
	}
	return nil
}

// call sends a request; a broken HTTP session is replaced once (only when that cannot run a tool twice).
func (c *Client) call(ctx context.Context, timeout time.Duration, method string, params, out any) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	c.mu.Lock()
	t, kind, closed := c.t, c.transport, c.closed
	c.mu.Unlock()
	if closed {
		return errClosed
	}
	err := c.roundTrip(ctx, t, method, params, out)
	if err == nil || kind == "stdio" || !retryable(method, err) {
		return err
	}
	if rerr := c.reconnect(ctx, t); rerr != nil {
		return fmt.Errorf("%w (reconnecting: %v)", err, rerr)
	}
	c.mu.Lock()
	t = c.t
	c.mu.Unlock()
	return c.roundTrip(ctx, t, method, params, out)
}

// reconnect replaces the transport old with a new session (unless another call already did).
func (c *Client) reconnect(ctx context.Context, old transport) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errClosed
	}
	if c.t != old {
		return nil
	}
	_ = old.close()
	t, kind, res, err := c.dial(ctx)
	if err != nil {
		return err
	}
	c.t, c.transport, c.init = t, kind, res
	return nil
}

// Server is the configuration the client was made from.
func (c *Client) Server() *Server { return c.srv }

// Info says what the server is: its name, version, protocol and the transport in use.
func (c *Client) Info() (name, version, protocol, transport string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.init.ServerInfo.Name, c.init.ServerInfo.Version, c.init.ProtocolVersion, c.transport
}

// Instructions are the server's hints for the model (may be empty).
func (c *Client) Instructions() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.init.Instructions
}

// ToolInfo is a tool as the server lists it.
type ToolInfo struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	Annotations *Annotations    `json:"annotations,omitempty"`
}

// Annotations are the server's hints about a tool.
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ReadOnly says the server marks the tool readOnlyHint.
func (t ToolInfo) ReadOnly() bool {
	return t.Annotations != nil && t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint
}

// maxPages stops a server that keeps handing out cursors.
const maxPages = 100

// ListTools lists all tools, following the pagination cursor.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var all []ToolInfo
	cursor := ""
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page == maxPages {
			return nil, fmt.Errorf("tools/list: more than %d pages", maxPages)
		}
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		var res struct {
			Tools      []ToolInfo `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := c.call(ctx, ListTimeout, "tools/list", params, &res); err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			return all, nil
		}
		if seen[res.NextCursor] {
			return nil, fmt.Errorf("tools/list: the cursor %q came twice", res.NextCursor)
		}
		seen[res.NextCursor] = true
		cursor = res.NextCursor
	}
}

// CallResult is the answer of tools/call.
type CallResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// Content is one block of a tool result.
type Content struct {
	Type     string    `json:"type"` // text, image, audio, resource, resource_link
	Text     string    `json:"text,omitempty"`
	Data     string    `json:"data,omitempty"` // base64 (image, audio)
	MimeType string    `json:"mimeType,omitempty"`
	Resource *Resource `json:"resource,omitempty"`
	URI      string    `json:"uri,omitempty"` // resource_link
	Name     string    `json:"name,omitempty"`
}

// Resource is an embedded resource.
type Resource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// CallTool calls a tool (its name on the server). args nil is {}.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallResult, error) {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	timeout := CallTimeout
	if c.srv.Timeout > 0 {
		timeout = time.Duration(c.srv.Timeout * float64(time.Second))
	}
	var res CallResult
	err := c.call(ctx, timeout, "tools/call", map[string]any{"name": name, "arguments": args}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// Text flattens a result for the model: text as is, images and audio as a note, resources as their text.
func (r *CallResult) Text() string {
	var parts []string
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "image", "audio":
			parts = append(parts, fmt.Sprintf("[%s %s, %s, not shown]", c.Type, c.MimeType, size(c.Data)))
		case "resource":
			if c.Resource == nil {
				continue
			}
			switch {
			case c.Resource.Text != "":
				parts = append(parts, c.Resource.Text)
			default:
				parts = append(parts, fmt.Sprintf("[resource %s (%s), %s, not shown]", c.Resource.URI,
					c.Resource.MimeType, size(c.Resource.Blob)))
			}
		case "resource_link":
			parts = append(parts, strings.TrimSpace(fmt.Sprintf("[resource link %s %s]", c.Name, c.URI)))
		default:
			parts = append(parts, fmt.Sprintf("[%s content]", c.Type))
		}
	}
	if len(parts) == 0 && len(r.StructuredContent) > 0 {
		return string(r.StructuredContent)
	}
	return strings.Join(parts, "\n")
}

func size(b64 string) string {
	n := base64.StdEncoding.DecodedLen(len(b64))
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// Ping checks the server answers.
func (c *Client) Ping(ctx context.Context) error {
	return c.call(ctx, 10*time.Second, "ping", nil, nil)
}

// Close ends the session (DELETE for streamable HTTP, stdin closed and the process ended for stdio).
func (c *Client) Close() error {
	c.mu.Lock()
	t, closed := c.t, c.closed
	c.closed = true
	c.mu.Unlock()
	if closed || t == nil {
		return nil
	}
	return t.close()
}
