package hub

import (
	"io"
	"testing"
)

// GET /healthz answers 200 "ok" without a token and without a worker (Kubernetes probes).
func TestHealthz(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	r := do(t, "GET", srv.URL+"/healthz", "", nil)
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || string(b) != "ok" {
		t.Fatalf("healthz: %d %q", r.StatusCode, b)
	}
	if ct := r.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("healthz content type: %q", ct)
	}
}
