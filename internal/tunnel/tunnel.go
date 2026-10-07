// Package tunnel connects the Mac (the worker: models, jobs, files) to the hub on the Raspberry Pi (web UI,
// sign-in). The worker dials out — no port opens on the Mac — and the hub then makes plain HTTP requests to the
// worker through that one connection: a WebSocket carrying yamux streams, one stream per request (SSE and file
// downloads included).
//
//	worker                                  hub
//	Serve(ctx, hubURL, token, handler) ──►  GET /api/worker/connect (Authorization: Bearer <worker token>)
//	http.Serve(yamux session, handler) ◄──  Hub.Transport(): a request = a new stream (URL host: Host(id))
//
// The hub keeps every worker connected at once (hub.go), one session per worker id.
package tunnel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// Path is where the hub accepts the worker.
const Path = "/api/worker/connect"

// HeaderName carries the worker's name (its host name) for the hub to show.
const HeaderName = "X-Mlxlab-Worker"

func config() *yamux.Config {
	c := yamux.DefaultConfig()
	c.LogOutput = io.Discard
	c.KeepAliveInterval = 15 * time.Second
	c.ConnectionWriteTimeout = 30 * time.Second
	c.MaxStreamWindowSize = 4 << 20 // files stream quicker with a larger window
	return c
}

// ---------- worker side ----------

// Serve connects to the hub and serves handler to it until ctx ends, reconnecting (with backoff) whenever the
// connection drops. status is told about every change (connected, the error that ended a connection).
func Serve(ctx context.Context, hubURL, token, name string, handler http.Handler, status func(connected bool, err error)) error {
	if status == nil {
		status = func(bool, error) {}
	}
	wait := time.Second
	for {
		start := time.Now()
		err := serveOnce(ctx, hubURL, token, name, handler, func() { status(true, nil); wait = time.Second })
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrUnauthorized) {
			status(false, err)
			return err
		}
		status(false, err)
		if time.Since(start) > time.Minute {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

// ErrUnauthorized: the hub does not know the worker token (retrying will not help).
var ErrUnauthorized = errors.New("the hub refused the worker token")

func serveOnce(ctx context.Context, hubURL, token, name string, handler http.Handler, connected func()) error {
	u := strings.TrimRight(hubURL, "/") + Path
	u = "ws" + strings.TrimPrefix(u, "http") // http→ws, https→wss
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set(HeaderName, name)
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	ws, resp, err := websocket.Dial(dctx, u, &websocket.DialOptions{HTTPHeader: h})
	cancel()
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return ErrUnauthorized
		}
		return err
	}
	ws.SetReadLimit(-1)
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	sess, err := yamux.Server(conn, config())
	if err != nil {
		conn.Close()
		return err
	}
	connected()
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		select {
		case <-ctx.Done():
		case <-sess.CloseChan():
		}
		srv.Close()
		sess.Close()
	}()
	err = srv.Serve(sess) // a yamux session is a net.Listener of the hub's streams
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, io.EOF) {
		err = errors.New("the hub closed the connection")
	}
	return err
}
