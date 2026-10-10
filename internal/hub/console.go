package hub

import (
	"net"
	"net/http"
	"strings"

	"github.com/philipparndt/ai-connector/internal/consoleui"
)

// The console (docs/api.md, "Console UI and host names"): ai-connector's own UI under /console/ on every host;
// on the host names in MLXLAB_CONSOLE_HOSTS, / leads to it instead of the mlxlab app.

// parseHosts reads a comma list of host names ("ai.example.com, AI.other.org:443") as lower-case names without
// ports.
func parseHosts(s string) []string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		if h = normalizeHost(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// normalizeHost: lower case, no port, no brackets around an IPv6 address, no trailing dot.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return strings.TrimSuffix(h, ".")
}

// requestHost is the host name the browser asked for: X-Forwarded-Host behind traefik (its first entry), else Host.
func requestHost(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-Host"); xf != "" {
		first, _, _ := strings.Cut(xf, ",")
		if h := normalizeHost(first); h != "" {
			return h
		}
	}
	return normalizeHost(r.Host)
}

// consoleHost: the request came in on one of the console's host names.
func (s *Server) consoleHost(r *http.Request) bool {
	h := requestHost(r)
	for _, c := range s.cfg.ConsoleHosts {
		if h == normalizeHost(c) {
			return true
		}
	}
	return false
}

// withConsoleHosts sends GET / on the console's host names to /console/; everything else goes to next.
func (s *Server) withConsoleHosts(next http.Handler) http.Handler {
	if len(s.cfg.ConsoleHosts) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead) && s.consoleHost(r) {
			http.Redirect(w, r, consoleui.Prefix, http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}
