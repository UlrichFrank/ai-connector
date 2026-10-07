package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testProxy(t *testing.T) *proxy {
	t.Setenv("ALLOW_CLIENTS", "192.168.240.0/24")
	t.Setenv("BRAVE_KEY", "secret-key")
	t.Setenv("DENY", "")
	p, err := newProxy(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func do(p *proxy, method, target, client string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	if method == http.MethodConnect {
		r.URL.Host, r.Host = target, target
	}
	r.RemoteAddr = client
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}

func TestProxyRefusals(t *testing.T) {
	p := testProxy(t)
	const client = "192.168.240.5:40000"
	cases := []struct {
		method, target, client string
		code                   int
		body                   string
	}{
		{http.MethodGet, "http://example.com/", "10.10.2.1:5000", 403, "client not allowed"},
		{http.MethodGet, "http://192.168.1.1/", client, 403, "not a public address"},
		{http.MethodGet, "http://k8s.example.org/", client, 403, "example.org"},
		{http.MethodGet, "http://example.com:8080/", client, 403, "port 8080"},
		{http.MethodPost, "http://example.com/", client, 405, "method POST"},
		{http.MethodGet, "ftp://example.com/", client, 400, "scheme"},
		{http.MethodGet, "http://u:p@example.com/", client, 400, "credentials"},
		{http.MethodGet, "http://api.search.brave.com/other", client, 403, "only /res/v1/"},
		{http.MethodConnect, "169.254.169.254:443", client, 403, "not a public address"},
		{http.MethodConnect, "localhost:443", client, 403, "localhost"},
		{http.MethodConnect, "example.com:22", client, 403, "port 22"},
	}
	for _, c := range cases {
		w := do(p, c.method, c.target, c.client)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s %s from %s: %d %q, want %d %q", c.method, c.target, c.client, w.Code, w.Body.String(), c.code, c.body)
		}
		if strings.Contains(w.Body.String(), "secret-key") {
			t.Errorf("key leaked in %q", w.Body.String())
		}
	}
	if w := do(p, http.MethodGet, "/healthz", client); w.Code != 200 {
		t.Errorf("healthz %d", w.Code)
	}
}
