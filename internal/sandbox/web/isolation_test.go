//go:build sandbox

package web

// The isolation check against the real sandbox (Apple `container` must be running):
//
//	go test -tags sandbox -v -count=1 -timeout 30m ./internal/sandbox/web
//
// It builds the images if needed, starts the sandbox, runs every probe (see Check) and the two tools, and stops
// the sandbox again. MLXLAB_BRAVE_KEY / ~/.config/mlxlab/brave.key select Brave instead of SearXNG.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestUp only starts the sandbox and leaves it running (for poking at it with `container exec`):
// MLXLAB_SANDBOX_UP=1 go test -tags sandbox -run TestUp ./internal/sandbox/web
func TestUp(t *testing.T) {
	if os.Getenv("MLXLAB_SANDBOX_UP") == "" {
		t.Skip("set MLXLAB_SANDBOX_UP=1")
	}
	key, err := LoadBraveKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := New(Config{BraveKey: key, Logf: t.Logf}).Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIsolation(t *testing.T) {
	key, err := LoadBraveKey()
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{BraveKey: key, Logf: t.Logf})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	probes, err := s.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var failed, warned int
	for _, p := range probes {
		t.Log(p.String())
		if !p.Pass() {
			if p.Advisory {
				warned++
			} else {
				failed++
			}
		}
	}
	if warned > 0 {
		t.Logf("%d advisory probes reached the Mac from an unlocked VM on the isolated network: load the pf anchor (make sandbox-firewall)", warned)
	}
	if failed > 0 {
		t.Errorf("%d of %d probes failed", failed, len(probes))
	}

	// the tools themselves
	res, err := s.Call(ctx, ToolSearch, json.RawMessage(`{"query":"golang net/netip package","count":3}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("web_search: isError=%v sources=%d\n%s", res.IsError, len(res.Sources), res.Text)
	if res.IsError || len(res.Sources) == 0 {
		t.Errorf("web_search gave no results")
	}
	res, err = s.Call(ctx, ToolFetch, json.RawMessage(`{"url":"https://example.com/"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("web_fetch: isError=%v sources=%v\n%s", res.IsError, res.Sources, res.Text)
	if res.IsError || !strings.Contains(res.Text, "Example Domain") {
		t.Errorf("web_fetch example.com failed")
	}
	for _, u := range []string{"http://192.168.1.1.nip.io/", "https://example.org/", "http://127.0.0.1:8484/", "file:///etc/passwd"} {
		res, err = s.Call(ctx, ToolFetch, json.RawMessage(`{"url":"`+u+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("web_fetch %s: isError=%v %s", u, res.IsError, strings.TrimSpace(res.Text))
		if !res.IsError {
			t.Errorf("web_fetch %s was not refused", u)
		}
	}
}
