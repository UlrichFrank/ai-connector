package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

type linkStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// postJSON posts v (no token) and returns the status and the answer.
func postJSON(t *testing.T, url string, v any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(v)
	r, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var out map[string]any
	json.NewDecoder(r.Body).Decode(&out)
	return r.StatusCode, out
}

func startLink(t *testing.T, base, name string) linkStart {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"name": name})
	r, err := http.Post(base+"/api/link/start", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 200 {
		t.Fatalf("start: %d", r.StatusCode)
	}
	var out linkStart
	decode(t, r, &out)
	return out
}

func pollLink(t *testing.T, base, code string) (int, map[string]any) {
	t.Helper()
	return postJSON(t, base+"/api/link/token", map[string]string{"device_code": code})
}

func decideLink(t *testing.T, base, token, pin string, approve bool) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]bool{"approve": approve})
	r := do(t, "POST", base+"/api/link/"+pin, token, bytes.NewReader(b))
	defer r.Body.Close()
	var out map[string]any
	json.NewDecoder(r.Body).Decode(&out)
	return r.StatusCode, out
}

func getLink(t *testing.T, base, token, pin string) (int, map[string]any) {
	t.Helper()
	r := do(t, "GET", base+"/api/link/"+pin, token, nil)
	defer r.Body.Close()
	var out map[string]any
	json.NewDecoder(r.Body).Decode(&out)
	return r.StatusCode, out
}

// linkClock lets a test move the links' clock: the returned func sets it.
func linkClock(s *Server) func(time.Time) {
	now := time.Now()
	s.links.mu.Lock()
	s.links.now = func() time.Time { return now }
	s.links.mu.Unlock()
	return func(t time.Time) {
		s.links.mu.Lock()
		now = t
		s.links.mu.Unlock()
	}
}

func TestLinkFlow(t *testing.T) {
	s, srv := newHub(t, t.TempDir())
	owner := signIn(t, srv.URL, "laptop")
	set := linkClock(s)
	t0 := time.Now()
	set(t0)

	st := startLink(t, srv.URL, "  Wall Display  ")
	if len(st.DeviceCode) < 22 { // 16 bytes = 128 bits, base64url
		t.Fatalf("device code %q", st.DeviceCode)
	}
	if !regexp.MustCompile(`^\d{3} \d{3}$`).MatchString(st.UserCode) {
		t.Fatalf("user code %q", st.UserCode)
	}
	pin := normalizePIN(st.UserCode)
	if st.VerificationURI != srv.URL+"/pin" || st.VerificationURIComplete != srv.URL+"/pin/"+pin ||
		st.ExpiresIn != 600 || st.Interval != 5 {
		t.Fatalf("start %+v", st)
	}

	if code, out := pollLink(t, srv.URL, st.DeviceCode); code != 400 || out["error"] != "authorization_pending" {
		t.Fatalf("pending: %d %v", code, out)
	}
	// polled again too soon
	set(t0.Add(2 * time.Second))
	if code, out := pollLink(t, srv.URL, st.DeviceCode); code != 400 || out["error"] != "slow_down" {
		t.Fatalf("slow down: %d %v", code, out)
	}
	// lenient: a second early is fine
	set(t0.Add(6 * time.Second))
	if code, out := pollLink(t, srv.URL, st.DeviceCode); code != 400 || out["error"] != "authorization_pending" {
		t.Fatalf("pending after the interval: %d %v", code, out)
	}

	code, out := getLink(t, srv.URL, owner, pin)
	if code != 200 || out["name"] != "Wall Display" || out["userCode"] != st.UserCode {
		t.Fatalf("get: %d %v", code, out)
	}
	if exp, err := time.Parse(time.RFC3339, out["expires"].(string)); err != nil || exp.Sub(t0) < 9*time.Minute || exp.Sub(t0) > 11*time.Minute {
		t.Fatalf("expires %v", out["expires"])
	}

	code, out = decideLink(t, srv.URL, owner, pin, true)
	if code != 200 {
		t.Fatalf("approve: %d %v", code, out)
	}
	c := out["client"].(map[string]any)
	if c["name"] != "Wall Display" || c["owner"] != "Philipp" {
		t.Fatalf("client %v", c)
	}
	// decided: neither shown nor decided again
	if code, _ := getLink(t, srv.URL, owner, pin); code != 404 {
		t.Fatalf("get after approval: %d", code)
	}
	if code, _ := decideLink(t, srv.URL, owner, pin, true); code != 404 {
		t.Fatalf("approved twice: %d", code)
	}

	// the token once, right away (no slow_down once decided), then the link is gone
	code, out = pollLink(t, srv.URL, st.DeviceCode)
	if code != 200 || out["client"].(map[string]any)["id"] != c["id"] {
		t.Fatalf("token: %d %v", code, out)
	}
	token := out["token"].(string)
	if got, ok := s.clients.lookup(token); !ok || got.ID != c["id"] || got.Owner != "Philipp" {
		t.Fatalf("token %q is not the client's: %v %v", token, got, ok)
	}
	if code, out := pollLink(t, srv.URL, st.DeviceCode); code != 400 || out["error"] != "expired_token" {
		t.Fatalf("second pickup: %d %v", code, out)
	}
	s.links.mu.Lock()
	n := len(s.links.byHash) + len(s.links.byPIN)
	s.links.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d entries kept", n)
	}
}

func TestLinkDeny(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	owner := signIn(t, srv.URL, "laptop")
	st := startLink(t, srv.URL, "Wall Display")
	pin := normalizePIN(st.UserCode)

	if code, _ := decideLink(t, srv.URL, owner, pin, false); code != 204 {
		t.Fatalf("deny: %d", code)
	}
	if code, _ := decideLink(t, srv.URL, owner, pin, true); code != 404 {
		t.Fatalf("approve after deny: %d", code)
	}
	if code, out := pollLink(t, srv.URL, st.DeviceCode); code != 400 || out["error"] != "access_denied" {
		t.Fatalf("denied: %d %v", code, out)
	}
	if code, out := pollLink(t, srv.URL, st.DeviceCode); code != 400 || out["error"] != "expired_token" {
		t.Fatalf("after denied: %d %v", code, out)
	}
	if r := do(t, "GET", srv.URL+"/api/clients", owner, nil); r.StatusCode != 200 {
		t.Fatalf("clients: %d", r.StatusCode)
	} else {
		var list []any
		decode(t, r, &list)
		if len(list) != 0 {
			t.Fatalf("clients after deny %v", list)
		}
	}
	// no approve field
	st = startLink(t, srv.URL, "Wall Display")
	r := do(t, "POST", srv.URL+"/api/link/"+normalizePIN(st.UserCode), owner, strings.NewReader(`{}`))
	r.Body.Close()
	if r.StatusCode != 400 {
		t.Fatalf("no approve: %d", r.StatusCode)
	}
}

func TestLinkExpiry(t *testing.T) {
	s, srv := newHub(t, t.TempDir())
	owner := signIn(t, srv.URL, "laptop")
	set := linkClock(s)
	t0 := time.Now()
	set(t0)
	st := startLink(t, srv.URL, "Wall Display")
	pin := normalizePIN(st.UserCode)

	set(t0.Add(linkTTL + time.Second))
	if code, _ := getLink(t, srv.URL, owner, pin); code != 404 {
		t.Fatalf("get expired: %d", code)
	}
	if code, _ := decideLink(t, srv.URL, owner, pin, true); code != 404 {
		t.Fatalf("approve expired: %d", code)
	}
	if code, out := pollLink(t, srv.URL, st.DeviceCode); code != 400 || out["error"] != "expired_token" {
		t.Fatalf("poll expired: %d %v", code, out)
	}
	if code, out := pollLink(t, srv.URL, "not-a-code"); code != 400 || out["error"] != "expired_token" {
		t.Fatalf("unknown: %d %v", code, out)
	}
	s.links.mu.Lock()
	n := len(s.links.byHash) + len(s.links.byPIN)
	s.links.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d expired entries kept", n)
	}
}

func TestLinkPINNormalised(t *testing.T) {
	s, srv := newHub(t, t.TempDir())
	owner := signIn(t, srv.URL, "laptop")
	pins := []string{"012345", "012345", "678901"} // the second collides with the first: drawn again
	s.links.mu.Lock()
	s.links.pin = func() (string, error) { p := pins[0]; pins = pins[1:]; return p, nil }
	s.links.mu.Unlock()

	a, b := startLink(t, srv.URL, "A"), startLink(t, srv.URL, "B")
	if a.UserCode != "012 345" || b.UserCode != "678 901" || a.VerificationURIComplete != srv.URL+"/pin/012345" {
		t.Fatalf("codes %q %q %q", a.UserCode, b.UserCode, a.VerificationURIComplete)
	}
	for _, spelling := range []string{"012345", "012%20345", "012-345", "%20012%20345%20"} {
		if code, out := getLink(t, srv.URL, owner, spelling); code != 200 || out["name"] != "A" {
			t.Fatalf("%q: %d %v", spelling, code, out)
		}
	}
	if code, _ := getLink(t, srv.URL, owner, "12345"); code != 404 {
		t.Fatalf("short PIN: %d", code)
	}
}

func TestLinkAuth(t *testing.T) {
	s, srv := newHub(t, t.TempDir())
	owner := signIn(t, srv.URL, "laptop")
	st := startLink(t, srv.URL, "Wall Display")
	pin := normalizePIN(st.UserCode)
	clientToken, _, err := s.clients.add("script", "Philipp")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "wrong", clientToken} {
		if code, out := getLink(t, srv.URL, token, pin); code != 401 || out["error"] != "signin" {
			t.Fatalf("GET with %q: %d %v", token, code, out)
		}
		if code, out := decideLink(t, srv.URL, token, pin, true); code != 401 || out["error"] != "signin" {
			t.Fatalf("POST with %q: %d %v", token, code, out)
		}
	}
	// still pending
	if code, _ := getLink(t, srv.URL, owner, pin); code != 200 {
		t.Fatalf("get: %d", code)
	}
}

func TestLinkStartChecks(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	for _, name := range []string{"", "   "} {
		if code, out := postJSON(t, srv.URL+"/api/link/start", map[string]string{"name": name}); code != 400 || out["error"] != "name" {
			t.Fatalf("name %q: %d %v", name, code, out)
		}
	}
	// 10 per minute per IP, the two above included
	for i := 0; i < 8; i++ {
		startLink(t, srv.URL, "app")
	}
	if code, out := postJSON(t, srv.URL+"/api/link/start", map[string]string{"name": "app"}); code != 429 || out["error"] != "rate" {
		t.Fatalf("11th start: %d %v", code, out)
	}
}

func TestLinkNameCut(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	owner := signIn(t, srv.URL, "laptop")
	st := startLink(t, srv.URL, strings.Repeat("x", 150))
	if code, out := getLink(t, srv.URL, owner, normalizePIN(st.UserCode)); code != 200 || len(out["name"].(string)) != 100 {
		t.Fatalf("long name: %d %v", code, out)
	}
}

func TestLinkMaxPending(t *testing.T) {
	s, srv := newHub(t, t.TempDir())
	for i := 0; i < maxLinks; i++ {
		if _, _, ok, err := s.links.add("app"); !ok || err != nil {
			t.Fatalf("add %d: %v %v", i, ok, err)
		}
	}
	if code, out := postJSON(t, srv.URL+"/api/link/start", map[string]string{"name": "app"}); code != 429 || out["error"] != "rate" {
		t.Fatalf("over the cap: %d %v", code, out)
	}
}

func TestLinkLookupRateLimit(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	owner := signIn(t, srv.URL, "laptop")
	other := signIn(t, srv.URL, "phone")
	for i := 0; i < 30; i++ {
		if code, _ := getLink(t, srv.URL, owner, "000000"); code != 404 {
			t.Fatalf("guess %d: %d", i, code)
		}
	}
	if code, out := getLink(t, srv.URL, owner, "000000"); code != 429 || out["error"] != "rate" {
		t.Fatalf("31st guess: %d %v", code, out)
	}
	if code, out := decideLink(t, srv.URL, owner, "000000", true); code != 429 {
		t.Fatalf("approve over the limit: %d %v", code, out)
	}
	// per device
	if code, _ := getLink(t, srv.URL, other, "000000"); code != 404 {
		t.Fatalf("other device: %d", code)
	}
}

func TestLinkPublicURL(t *testing.T) {
	_, srv := newHubWith(t, Config{Password: password, Data: t.TempDir(), Tools: t.TempDir(), PublicURL: " https://ai.example.org/ "})
	st := startLink(t, srv.URL, "Wall Display")
	pin := normalizePIN(st.UserCode)
	if st.VerificationURI != "https://ai.example.org/pin" || st.VerificationURIComplete != "https://ai.example.org/pin/"+pin {
		t.Fatalf("start %+v", st)
	}

	// without one, the origin the request came to (behind traefik: the forwarded one)
	_, srv = newHub(t, t.TempDir())
	req, _ := http.NewRequest("POST", srv.URL+"/api/link/start", strings.NewReader(`{"name":"Wall Display"}`))
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "hub.example.org")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	decode(t, r, &st)
	if st.VerificationURI != "https://hub.example.org/pin" {
		t.Fatalf("forwarded %+v", st)
	}
}

func TestPinRedirect(t *testing.T) {
	_, srv := newHubWith(t, Config{Password: password, Data: t.TempDir(), Tools: t.TempDir(), ConsoleHosts: []string{"ai.example.org"}})
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for path, want := range map[string]string{
		"/pin":             "/console/#/link",
		"/pin/":            "/console/#/link",
		"/pin/123456":      "/console/#/link/123456",
		"/pin/123%20456":   "/console/#/link/123456",
		"/pin/123-456":     "/console/#/link/123456",
		"/pin/12345":       "/console/#/link",
		"/pin/abc123456":   "/console/#/link",
		"/pin/1234567":     "/console/#/link",
		"/pin/123456/more": "/console/#/link",
	} {
		for _, host := range []string{"", "ai.example.org", "app.example.org"} { // console and app host names
			req, _ := http.NewRequest("GET", srv.URL+path, nil)
			if host != "" {
				req.Host = host
			}
			r, err := noFollow.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			r.Body.Close()
			if r.StatusCode != http.StatusFound || r.Header.Get("Location") != want {
				t.Fatalf("%s on %q: %d %q, want %q", path, host, r.StatusCode, r.Header.Get("Location"), want)
			}
		}
	}
}
