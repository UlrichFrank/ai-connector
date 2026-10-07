package hub

import (
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/tunnel"
)

func offline(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "offline", "message": "The Mac is offline"})
}

// newProxy forwards a request to the Mac dispatch chose (forward), unchanged except for the device's credentials
// (the worker trusts the tunnel and has no business seeing them) and the route headers (the hub's own: a
// client's are dropped).
func (s *Server) newProxy() http.Handler {
	s.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			n := target(pr.In)
			pr.SetURL(&url.URL{Scheme: "http", Host: tunnel.Host(n.id)})
			pr.SetXForwarded()
			if secure(pr.In) { // traefik ended TLS; the worker should still see https
				pr.Out.Header.Set("X-Forwarded-Proto", "https")
			}
			pr.Out.Host = pr.In.Host
			stripCredentials(pr.Out)
			for _, h := range []string{routeHeader, workerIDHeader, workerNameHeader} {
				pr.Out.Header.Del(h)
			}
			if n.isRouted() {
				setRoute(pr.Out.Header, n)
			}
		},
		Transport:     s.tunnel.Transport(),
		FlushInterval: -1, // SSE (/api/events) must reach the client at once
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// a pooled stream of a session that just died fails with a plain I/O error, hence the second check
			n := target(r)
			if _, ok := s.tunnel.Lookup(n.id); errors.Is(err, tunnel.ErrOffline) || !ok {
				if len(s.tunnel.Connected()) == 0 {
					offline(w)
				} else {
					offlineMac(w, n.name)
				}
				return
			}
			if r.Context().Err() != nil {
				return // the client went away
			}
			log.Printf("proxy %s %s to %q: %v", r.Method, r.URL.Path, n.name, err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "worker", "message": err.Error()})
		},
	}
	return http.HandlerFunc(s.dispatch)
}

// stripCredentials removes the device token from the header, the cookie and the query.
func stripCredentials(r *http.Request) {
	r.Header.Del("Authorization")
	if cookies := r.Cookies(); len(cookies) > 0 {
		var keep []string
		for _, c := range cookies {
			if c.Name != CookieName {
				keep = append(keep, c.Name+"="+c.Value)
			}
		}
		r.Header.Del("Cookie")
		if len(keep) > 0 {
			r.Header.Set("Cookie", strings.Join(keep, "; "))
		}
	}
	if q := r.URL.Query(); q.Has("token") {
		q.Del("token")
		r.URL.RawQuery = q.Encode()
	}
}

// limiter allows n attempts per key (an IP) per window: a fixed window, which is plenty for a sign-in form.
type limiter struct {
	n      int
	window time.Duration

	mu   sync.Mutex
	hits map[string]*window
}

type window struct {
	start time.Time
	count int
}

func newLimiter(n int, d time.Duration) *limiter {
	return &limiter{n: n, window: d, hits: map[string]*window{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.hits) > 10000 { // forget old windows now and then, so the map cannot grow without end
		for k, w := range l.hits {
			if now.Sub(w.start) > l.window {
				delete(l.hits, k)
			}
		}
	}
	w := l.hits[key]
	if w == nil || now.Sub(w.start) > l.window {
		w = &window{start: now}
		l.hits[key] = w
	}
	w.count++
	return w.count <= l.n
}
