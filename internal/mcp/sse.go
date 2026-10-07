package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxEvent caps one server-sent event (a tool result is text for a model; 32 MB is plenty).
const maxEvent = 32 << 20

type event struct {
	name string // "message" if the server gave none
	data string
}

// sseReader reads text/event-stream.
type sseReader struct{ r *bufio.Reader }

func newSSEReader(r io.Reader) *sseReader { return &sseReader{bufio.NewReaderSize(r, 64<<10)} }

// next returns the next event with data; io.EOF at the end of the stream.
func (s *sseReader) next() (event, error) {
	var name string
	var data strings.Builder
	has := false
	for {
		line, err := s.r.ReadString('\n')
		if line == "" && err != nil {
			if has {
				break
			}
			return event{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if has {
				return finish(name, &data), nil
			}
			name = ""
		case strings.HasPrefix(line, ":"): // a comment (keep-alive)
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				name = value
			case "data":
				if has {
					data.WriteByte('\n')
				}
				data.WriteString(value)
				has = true
				if data.Len() > maxEvent {
					return event{}, fmt.Errorf("server-sent event larger than %d MB", maxEvent>>20)
				}
			}
		}
		if err != nil { // the last line had no newline
			if has {
				break
			}
			return event{}, err
		}
	}
	return finish(name, &data), nil
}

func finish(name string, data *strings.Builder) event {
	if name == "" {
		name = "message"
	}
	return event{name: name, data: data.String()}
}

// sseTransport is the HTTP+SSE transport of protocol 2024-11-05: a GET opens a stream whose first event
// ("endpoint") names the URL to POST messages to; the responses come on the stream.
type sseTransport struct {
	url      string
	headers  map[string]string
	client   *http.Client
	endpoint string
	cancel   context.CancelFunc
	mux      *mux
	done     chan struct{}
}

func dialSSE(ctx context.Context, rawURL string, headers map[string]string, client *http.Client) (*sseTransport, error) {
	sctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "text/event-stream")
	type opened struct {
		resp *http.Response
		err  error
	}
	och := make(chan opened, 1)
	go func() {
		resp, err := client.Do(req)
		och <- opened{resp, err}
	}()
	var resp *http.Response
	select {
	case o := <-och:
		if o.err != nil {
			cancel()
			return nil, o.err
		}
		resp = o.resp
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		cancel()
		return nil, &HTTPError{Status: resp.StatusCode, Body: snippet(b)}
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("GET %s: not an event stream (%s)", rawURL, ct)
	}
	t := &sseTransport{url: rawURL, headers: headers, client: client, cancel: cancel, done: make(chan struct{})}
	t.mux = newMux(func(m *message) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = t.post(ctx, m)
	})
	endpoint := make(chan string, 1)
	go t.read(resp.Body, endpoint)
	select {
	case e, ok := <-endpoint:
		if !ok {
			cancel()
			return nil, fmt.Errorf("GET %s: the stream ended before the endpoint event: %w", rawURL, t.mux.failure())
		}
		if err := t.setEndpoint(e); err != nil {
			cancel()
			return nil, err
		}
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
	return t, nil
}

// setEndpoint resolves the endpoint against the stream's URL. It must be on the same origin: the headers (the
// token) go there too.
func (t *sseTransport) setEndpoint(e string) error {
	base, err := url.Parse(t.url)
	if err != nil {
		return err
	}
	u, err := base.Parse(strings.TrimSpace(e))
	if err != nil {
		return fmt.Errorf("endpoint %q: %w", e, err)
	}
	if u.Scheme != base.Scheme || u.Host != base.Host {
		return fmt.Errorf("endpoint %q is on another origin than %s", e, t.url)
	}
	t.endpoint = u.String()
	return nil
}

func (t *sseTransport) read(body io.ReadCloser, endpoint chan string) {
	defer close(t.done)
	defer body.Close()
	r := newSSEReader(body)
	sent := false
	for {
		ev, err := r.next()
		if err != nil {
			t.mux.fail(fmt.Errorf("%w: event stream: %v", errClosed, err))
			if !sent {
				close(endpoint)
			}
			return
		}
		switch ev.name {
		case "endpoint":
			if !sent {
				endpoint <- ev.data
				sent = true
			}
		case "message":
			var m message
			if json.Unmarshal([]byte(ev.data), &m) == nil {
				t.mux.deliver(&m)
			}
		}
	}
}

func (t *sseTransport) post(ctx context.Context, m *message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return &HTTPError{Status: resp.StatusCode, Body: snippet(body)}
	}
	return nil
}

func (t *sseTransport) request(ctx context.Context, m *message) (*message, error) {
	ch, err := t.mux.wait(m.ID)
	if err != nil {
		return nil, err
	}
	if err := t.post(ctx, m); err != nil {
		t.mux.forget(m.ID)
		return nil, err
	}
	return t.mux.await(ctx, m.ID, ch)
}

func (t *sseTransport) notify(ctx context.Context, m *message) error { return t.post(ctx, m) }
func (t *sseTransport) negotiated(string)                            {}

func (t *sseTransport) close() error {
	t.cancel()
	t.mux.fail(errClosed)
	<-t.done
	return nil
}
