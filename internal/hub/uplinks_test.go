package hub

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/tunnel"
)

// An uplink: the hub dials an application, which then reaches the client API through it — as the uplink's client,
// owned by the person who made it — and nothing else.
func TestUplinks(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	base := srv.URL
	alice := signInAs(t, base, "Alice's laptop", "Alice")
	_, wtok := enroll(t, base, alice, "Alice's Mac")
	startAPIMac(t, base, wtok)

	// the application: it accepts the hub at /ai/uplink with its own token
	app := &tunnel.Hub{Check: func(token string) (string, string, bool) { return "hub", "AI", token == "app-secret" }}
	mux := http.NewServeMux()
	mux.Handle("GET /ai/uplink", app)
	appSrv := httptest.NewServer(mux)
	t.Cleanup(appSrv.Close)

	var up Uplink
	r := do(t, "POST", base+"/api/uplinks", alice, strings.NewReader(`{"name":"Board app","url":"`+appSrv.URL+`/ai/uplink","token":"app-secret"}`))
	decode(t, r, &up)
	if r.StatusCode != 201 || up.Owner != "Alice" || up.Client == "" || up.Name != "Board app" {
		t.Fatalf("made %d %+v", r.StatusCode, up)
	}
	state := func(id string) Uplink {
		var list []Uplink
		decode(t, do(t, "GET", base+"/api/uplinks", alice, nil), &list)
		for _, u := range list {
			if u.ID == id {
				return u
			}
		}
		return Uplink{}
	}
	waitFor(t, "the uplink connected", func() bool { return state(up.ID).State == "connected" })

	// the application's requests through it
	hc := &http.Client{Transport: app.Transport()}
	via := "http://" + tunnel.Host("hub")
	call := func(method, path, auth, body string) (int, string) {
		req, _ := http.NewRequest(method, via+path, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if c, b := call("GET", "/api/capabilities", "", ""); c != 200 || !strings.Contains(b, `"user":"Alice"`) {
		t.Fatalf("capabilities %d %s", c, b)
	}
	c, b := call("POST", "/api/jobs", "", `{"model":"qwen3.6-35b-a3b","messages":[{"role":"user","content":"hi"}]}`)
	if c != 200 || !strings.Contains(b, `"client":"Board app"`) || !strings.Contains(b, `"clientId":"`+up.Client+`"`) {
		t.Fatalf("job %d %s", c, b)
	}
	if c, b := call("POST", "/v1/chat/completions", "", `{"model":"qwen3.6-35b-a3b","messages":[{"role":"user","content":"hi"}]}`); c != 200 {
		t.Fatalf("chat %d %s", c, b)
	}
	// nothing else, not even with a device's token
	for _, p := range []string{"/api/devices", "/api/workers", "/api/clients", "/api/uplinks", "/", "/console/", "/setup"} {
		if c, _ := call("GET", p, alice, ""); c != 404 && c != 401 {
			t.Errorf("GET %s through the uplink: %d", p, c)
		}
	}
	if c, _ := call("POST", "/api/login", "", `{"name":"x","password":"y"}`); c != 404 {
		t.Errorf("login through the uplink: %d", c)
	}
	// the uplinks are a device's business
	_, appTok := makeClient(t, base, alice, "app")
	if r := do(t, "GET", base+"/api/uplinks", appTok, nil); r.StatusCode != 401 {
		t.Fatalf("a client lists uplinks: %d", r.StatusCode)
	}

	// its client revoked: the uplink stays, its requests are refused
	if r := do(t, "DELETE", base+"/api/clients/"+up.Client, alice, nil); r.StatusCode != 204 {
		t.Fatalf("revoke %d", r.StatusCode)
	}
	if c, _ := call("GET", "/api/capabilities", "", ""); c != 401 {
		t.Fatalf("after the revocation: %d", c)
	}

	// deleted: the connection ends
	if r := do(t, "DELETE", base+"/api/uplinks/"+up.ID, alice, nil); r.StatusCode != 204 {
		t.Fatalf("delete %d", r.StatusCode)
	}
	waitFor(t, "the uplink gone", func() bool { return len(app.Connected()) == 0 })

	// a token the application does not take: it stops with an error
	r = do(t, "POST", base+"/api/uplinks", alice, strings.NewReader(`{"url":"`+appSrv.URL+`/ai/uplink","token":"wrong"}`))
	decode(t, r, &up)
	waitFor(t, "the uplink refused", func() bool { return state(up.ID).State == "error" })
	if u := state(up.ID); !strings.Contains(u.Error, "refused") {
		t.Fatalf("refused %+v", u)
	}
	for _, bad := range []string{`{"url":"ftp://x","token":"t"}`, `{"url":"https://x/ai","token":""}`, `{"url":"https://x/ai","token":"a b"}`} {
		if r := do(t, "POST", base+"/api/uplinks", alice, strings.NewReader(bad)); r.StatusCode != 400 {
			t.Errorf("%s: %d", bad, r.StatusCode)
		}
	}
}
