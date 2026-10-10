package hub

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/philipparndt/ai-connector/internal/consoleui"
)

func TestParseHosts(t *testing.T) {
	got := parseHosts(" ai.example.org, AI.Example.com:443 ,,[::1]:8080, x.org. ")
	want := []string{"ai.example.org", "ai.example.com", "::1", "x.org"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseHosts: %q, want %q", got, want)
	}
	if parseHosts("") != nil {
		t.Error("empty: want nil")
	}
}

func TestFromEnvConsoleHosts(t *testing.T) {
	t.Setenv("MLXLAB_CONSOLE_HOSTS", "ai.example.org,Console.Local")
	if got := FromEnv().ConsoleHosts; !reflect.DeepEqual(got, []string{"ai.example.org", "console.local"}) {
		t.Errorf("FromEnv: %q", got)
	}
}

// noRedirect: a client that shows redirects instead of following them.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func TestConsoleHostRedirect(t *testing.T) {
	_, srv := newHubWith(t, Config{Password: password, Data: t.TempDir(), Tools: t.TempDir(), ConsoleHosts: []string{"ai.example.org", "Console.Example.com"}})
	for _, tc := range []struct {
		name, method, path, host, forwarded string
		redirect                            bool
	}{
		{"console host", "GET", "/", "ai.example.org", "", true},
		{"with port, other case", "GET", "/", "AI.example.org:8443", "", true},
		{"configured in other case", "GET", "/", "console.example.com", "", true},
		{"behind traefik", "GET", "/", "10.0.0.5:8080", "ai.example.org", true},
		{"forwarded list", "GET", "/", "10.0.0.5:8080", "ai.example.org:443, proxy.local", true},
		{"HEAD", "HEAD", "/", "ai.example.org", "", true},
		{"mlxlab host", "GET", "/", "mlxlab.example.org", "", false},
		{"forwarded mlxlab host wins over Host", "GET", "/", "ai.example.org", "mlxlab.example.org", false},
		{"other path on the console host", "GET", "/favicon.svg", "ai.example.org", "", false},
		{"POST", "POST", "/", "ai.example.org", "", false},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		req.Host = tc.host
		if tc.forwarded != "" {
			req.Header.Set("X-Forwarded-Host", tc.forwarded)
		}
		r, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		got := r.StatusCode == http.StatusFound && r.Header.Get("Location") == consoleui.Prefix
		if got != tc.redirect {
			t.Errorf("%s: %d %q, redirect want %v", tc.name, r.StatusCode, r.Header.Get("Location"), tc.redirect)
		}
	}
}

func TestNoConsoleHosts(t *testing.T) {
	_, srv := newHubWith(t, Config{Password: password, Data: t.TempDir(), Tools: t.TempDir()})
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "ai.example.org"
	r, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode == http.StatusFound {
		t.Errorf("redirected without MLXLAB_CONSOLE_HOSTS")
	}
}

// The console is served under /console/ on every host, without sign-in (the app signs in through the API); the
// handler itself is tested in internal/consoleui.
func TestConsoleServed(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	r, err := noRedirect.Get(srv.URL + "/console")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	// the mux's own redirect to the subtree (307 in current Go)
	if r.StatusCode/100 != 3 || r.Header.Get("Location") != "/console/" {
		t.Errorf("/console: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	w := httptest.NewRecorder()
	s, _ := New(Config{Password: password, Data: t.TempDir(), Tools: t.TempDir()})
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/console/clients", nil))
	// built: the app; not built (a Go build before `make console`): a clear 404
	if consoleui.Built() {
		if w.Code != 200 || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("/console/clients: %d %q", w.Code, w.Header().Get("Content-Type"))
		}
	} else if w.Code != 404 {
		t.Errorf("/console/clients without a build: %d", w.Code)
	}
	// the API under it is not the console's: /api stays the API
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/workers", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("/api/workers without sign-in: %d", w.Code)
	}
}
