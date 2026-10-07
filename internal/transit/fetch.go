package transit

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"sync"
	"time"
)

// UserAgent is sent with every request. (bahn.de's bot protection refuses agents that carry a GitHub URL.)
const UserAgent = "mlxlab-transit/1.0 (personal timetable assistant)"

// RequestTimeout bounds one request to a provider.
const RequestTimeout = 15 * time.Second

const maxBody = 16 << 20

// Error is a provider failure, phrased for the model (and the user).
type Error struct {
	Provider string
	Msg      string
	// Unavailable: the provider is down, blocked us or timed out; another provider may help.
	Unavailable bool
}

func (e *Error) Error() string { return e.Provider + ": " + e.Msg }

// ErrUnsupported: the provider cannot do this.
var ErrUnsupported = errors.New("not supported by this provider")

// fetcher does the HTTP of one provider: timeouts, the User-Agent, a minimum gap between requests (to stay well
// below any rate limit), a size cap, gzip, and errors mapped to *Error.
type fetcher struct {
	provider string
	client   *http.Client
	gap      time.Duration
	// headers are set on every request.
	headers map[string]string

	mu   sync.Mutex
	next time.Time
}

func newFetcher(provider string, gap time.Duration, headers map[string]string) *fetcher {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ResponseHeaderTimeout = RequestTimeout
	return &fetcher{provider: provider, client: &http.Client{Transport: tr, Timeout: RequestTimeout}, gap: gap,
		headers: headers}
}

// wait keeps the gap between requests.
func (f *fetcher) wait(ctx context.Context) error {
	f.mu.Lock()
	now := time.Now()
	at := f.next
	if at.Before(now) {
		at = now
	}
	f.next = at.Add(f.gap)
	f.mu.Unlock()
	if d := time.Until(at); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// do sends the request and returns the JSON body of a 2xx answer.
func (f *fetcher) do(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range f.headers {
		req.Header.Set(k, v)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return nil, &Error{Provider: f.provider, Msg: "no answer in time", Unavailable: true}
		}
		return nil, &Error{Provider: f.provider, Msg: "cannot reach the server", Unavailable: true}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, &Error{Provider: f.provider, Msg: "the answer broke off", Unavailable: true}
	}
	if len(b) > maxBody {
		return nil, &Error{Provider: f.provider, Msg: "the answer is too large"}
	}
	// some servers send gzip although we asked for identity
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err == nil {
			b, err = io.ReadAll(io.LimitReader(zr, maxBody+1))
		}
		if err != nil || len(b) > maxBody {
			return nil, &Error{Provider: f.provider, Msg: "cannot decompress the answer"}
		}
	}
	switch {
	case resp.StatusCode == 403 || resp.StatusCode == 429:
		return nil, &Error{Provider: f.provider, Unavailable: true,
			Msg: fmt.Sprintf("the server refused the request (HTTP %d: rate limit or bot protection)", resp.StatusCode)}
	case resp.StatusCode >= 500:
		return nil, &Error{Provider: f.provider, Msg: fmt.Sprintf("server error (HTTP %d)", resp.StatusCode), Unavailable: true}
	case resp.StatusCode/100 != 2:
		return nil, &Error{Provider: f.provider, Msg: fmt.Sprintf("the server did not accept the request (HTTP %d)", resp.StatusCode)}
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if ct != "application/json" && !bytes.HasPrefix(bytes.TrimSpace(b), []byte("{")) &&
		!bytes.HasPrefix(bytes.TrimSpace(b), []byte("[")) {
		// an HTML error or maintenance page: never pass it on
		return nil, &Error{Provider: f.provider, Msg: "unexpected answer (not JSON; maintenance?)", Unavailable: true}
	}
	return b, nil
}
