package hub

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeInvite asks for a QR code with the device token and returns the answer.
func makeInvite(t *testing.T, base, token string) (code, link string, expires time.Time) {
	t.Helper()
	r := do(t, "POST", base+"/api/invites", token, nil)
	if r.StatusCode != 200 {
		t.Fatalf("invite: %d", r.StatusCode)
	}
	var out struct {
		Code    string    `json:"code"`
		URL     string    `json:"url"`
		Expires time.Time `json:"expires"`
	}
	decode(t, r, &out)
	return out.Code, out.URL, out.Expires
}

// redeem signs in with an invite and returns the status and the answer.
func redeem(t *testing.T, base, code, device, user string) (int, map[string]any) {
	t.Helper()
	r, out := loginWith(t, base, map[string]string{"invite": code, "device": device, "user": user})
	return r.StatusCode, out
}

func devicesOf(t *testing.T, base, token string) map[string]map[string]any {
	t.Helper()
	r := do(t, "GET", base+"/api/devices", token, nil)
	if r.StatusCode != 200 {
		t.Fatalf("devices: %d", r.StatusCode)
	}
	var list []map[string]any
	decode(t, r, &list)
	out := map[string]map[string]any{}
	for _, d := range list {
		out[d["name"].(string)] = d
	}
	return out
}

func infoOf(t *testing.T, base, token string) map[string]any {
	t.Helper()
	var out map[string]any
	decode(t, do(t, "GET", base+"/api/info", token, nil), &out)
	return out
}

func patch(t *testing.T, base, token, id, body string) (int, map[string]any) {
	t.Helper()
	r := do(t, "PATCH", base+"/api/devices/"+id, token, strings.NewReader(body))
	var out map[string]any
	decode(t, r, &out)
	return r.StatusCode, out
}

func TestLoginUser(t *testing.T) {
	_, srv := newHub(t, t.TempDir())

	// the user name is required (checked before the password)
	for _, u := range []string{"", "   "} {
		r, out := loginWith(t, srv.URL, map[string]string{"password": password, "device": "x", "user": u})
		if r.StatusCode != 400 || out["error"] != "user" || len(r.Cookies()) != 0 {
			t.Fatalf("user %q: %d %v", u, r.StatusCode, out)
		}
	}

	r, out := loginWith(t, srv.URL, map[string]string{"password": password, "device": "iPhone", "user": "  Anna  "})
	if r.StatusCode != 200 {
		t.Fatalf("login: %d %v", r.StatusCode, out)
	}
	dev := out["device"].(map[string]any)
	if dev["user"] != "Anna" || dev["owner"] != true || dev["canInvite"] != true || dev["invitedBy"] != "" {
		t.Fatalf("device %v", dev)
	}

	// trimmed and cut to 40 characters (not bytes)
	long := strings.Repeat("ä", 50)
	_, out = loginWith(t, srv.URL, map[string]string{"password": password, "device": "y", "user": long})
	if u := out["device"].(map[string]any)["user"].(string); u != strings.Repeat("ä", 40) {
		t.Fatalf("long user %q", u)
	}
}

func TestInviteFlow(t *testing.T) {
	s, srv := newHub(t, t.TempDir())
	owner := signIn(t, srv.URL, "laptop")
	ownerID := devicesOf(t, srv.URL, owner)["laptop"]["id"].(string)

	code, link, exp := makeInvite(t, srv.URL, owner)
	if len(code) < 22 { // 16 bytes = 128 bits, base64url
		t.Fatalf("code %q", code)
	}
	if d := time.Until(exp); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("expires in %v", d)
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "mlxlab" || u.Host != "signin" || u.Query().Get("hub") != srv.URL || u.Query().Get("code") != code {
		t.Fatalf("url %q", link)
	}

	// a missing user name does not use the code up
	if st, out := redeem(t, srv.URL, code, "iPhone", ""); st != 400 || out["error"] != "user" {
		t.Fatalf("no user: %d %v", st, out)
	}

	st, out := redeem(t, srv.URL, code, "Anna's iPhone", "Anna")
	if st != 200 {
		t.Fatalf("redeem: %d %v", st, out)
	}
	token := out["token"].(string)
	dev := out["device"].(map[string]any)
	if dev["user"] != "Anna" || dev["owner"] != false || dev["canInvite"] != false || dev["invitedBy"] != ownerID {
		t.Fatalf("invited device %v", dev)
	}
	if r := do(t, "GET", srv.URL+"/api/devices", token, nil); r.StatusCode != 200 {
		t.Fatalf("invited device signed in: %d", r.StatusCode)
	}

	// used up
	if st, out := redeem(t, srv.URL, code, "again", "Eve"); st != 401 || out["error"] != "invite" {
		t.Fatalf("used: %d %v", st, out)
	}
	// wrong
	if st, out := redeem(t, srv.URL, "not-a-code", "x", "Eve"); st != 401 || out["error"] != "invite" {
		t.Fatalf("wrong: %d %v", st, out)
	}
	// expired (and purged)
	code2, _, _ := makeInvite(t, srv.URL, owner)
	s.invites.mu.Lock()
	s.invites.now = func() time.Time { return time.Now().Add(inviteTTL + time.Second) }
	s.invites.mu.Unlock()
	if st, out := redeem(t, srv.URL, code2, "late", "Eve"); st != 401 || out["error"] != "invite" {
		t.Fatalf("expired: %d %v", st, out)
	}
	s.invites.mu.Lock()
	pending := len(s.invites.byHash)
	s.invites.now = time.Now
	s.invites.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d expired invites kept", pending)
	}

	// a code made by a device that was revoked since does not sign in; the devices it invited stay
	code3, _, _ := makeInvite(t, srv.URL, owner)
	if r := do(t, "DELETE", srv.URL+"/api/devices/"+ownerID, owner, nil); r.StatusCode != 200 {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	if st, _ := redeem(t, srv.URL, code3, "z", "Eve"); st != 401 {
		t.Fatalf("invite of a revoked device: %d", st)
	}
	if r := do(t, "GET", srv.URL+"/api/devices", token, nil); r.StatusCode != 200 {
		t.Fatalf("invited device after its inviter was revoked: %d", r.StatusCode)
	}
}

func TestInviteRateLimit(t *testing.T) {
	s, srv := newHub(t, t.TempDir())
	owner := signIn(t, srv.URL, "laptop") // one of the 10 attempts
	code, _, _ := makeInvite(t, srv.URL, owner)
	s.failDelay = 0
	for i := 0; i < 9; i++ {
		if st, _ := redeem(t, srv.URL, "guess", "x", "Eve"); st != 401 {
			t.Fatalf("attempt %d: %d", i, st)
		}
	}
	if st, out := redeem(t, srv.URL, code, "x", "Eve"); st != 429 || out["error"] != "rate" {
		t.Fatalf("11th attempt: %d %v", st, out)
	}
}

func TestInvitePermissions(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	owner := signIn(t, srv.URL, "laptop")
	code, _, _ := makeInvite(t, srv.URL, owner)
	st, out := redeem(t, srv.URL, code, "Anna's iPhone", "Anna")
	if st != 200 {
		t.Fatalf("redeem: %d", st)
	}
	anna := out["token"].(string)
	annaID := out["device"].(map[string]any)["id"].(string)
	ownerID := devicesOf(t, srv.URL, owner)["laptop"]["id"].(string)

	// an invited device may not invite until an owner allows it
	r := do(t, "POST", srv.URL+"/api/invites", anna, nil)
	var e map[string]string
	decode(t, r, &e)
	if r.StatusCode != 403 || e["error"] != "forbidden" {
		t.Fatalf("invite without canInvite: %d %v", r.StatusCode, e)
	}
	// and may not change devices (its own neither)
	for _, id := range []string{annaID, ownerID} {
		if st, out := patch(t, srv.URL, anna, id, `{"canInvite":true}`); st != 403 {
			t.Fatalf("PATCH by a non-owner: %d %v", st, out)
		}
	}

	if st, _ := patch(t, srv.URL, owner, "nope", `{"canInvite":true}`); st != 404 {
		t.Fatalf("PATCH unknown: %d", st)
	}
	if st, out := patch(t, srv.URL, owner, annaID, `{"user":"  "}`); st != 400 || out["error"] != "user" {
		t.Fatalf("PATCH empty user: %d %v", st, out)
	}
	st, out = patch(t, srv.URL, owner, annaID, `{"canInvite":true,"user":"Anna B."}`)
	if st != 200 || out["canInvite"] != true || out["user"] != "Anna B." || out["id"] != annaID || out["owner"] != false {
		t.Fatalf("PATCH: %d %v", st, out)
	}
	if d := devicesOf(t, srv.URL, owner)["Anna's iPhone"]; d["canInvite"] != true || d["user"] != "Anna B." {
		t.Fatalf("listed %v", d)
	}

	// now Anna may invite; her guest is invited by her and may not invite
	code2, _, _ := makeInvite(t, srv.URL, anna)
	st, out = redeem(t, srv.URL, code2, "Ben's iPhone", "Ben")
	if st != 200 || out["device"].(map[string]any)["invitedBy"] != annaID || out["device"].(map[string]any)["canInvite"] != false {
		t.Fatalf("invited by Anna: %d %v", st, out)
	}

	// turned off again: her pending code no longer signs in
	code3, _, _ := makeInvite(t, srv.URL, anna)
	if st, _ := patch(t, srv.URL, owner, annaID, `{"canInvite":false}`); st != 200 {
		t.Fatalf("PATCH off: %d", st)
	}
	if st, _ := redeem(t, srv.URL, code3, "x", "Eve"); st != 401 {
		t.Fatalf("code of a device no longer allowed: %d", st)
	}

	// owners always invite: canInvite false does not stick on an owner
	if st, out := patch(t, srv.URL, owner, ownerID, `{"canInvite":false}`); st != 200 || out["canInvite"] != true {
		t.Fatalf("owner canInvite off: %d %v", st, out)
	}
}

func TestInfoUser(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	in := infoOf(t, srv.URL, "")
	if in["user"] != "" || in["owner"] != false || in["canInvite"] != false || in["signedIn"] != false {
		t.Fatalf("signed out %v", in)
	}
	owner := signIn(t, srv.URL, "laptop")
	in = infoOf(t, srv.URL, owner)
	if in["user"] != "Philipp" || in["owner"] != true || in["canInvite"] != true || in["signedIn"] != true {
		t.Fatalf("owner %v", in)
	}
	code, _, _ := makeInvite(t, srv.URL, owner)
	_, out := redeem(t, srv.URL, code, "iPhone", "Anna")
	in = infoOf(t, srv.URL, out["token"].(string))
	if in["user"] != "Anna" || in["owner"] != false || in["canInvite"] != false || in["signedIn"] != true {
		t.Fatalf("invited %v", in)
	}
}

func TestInviteOrigin(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	owner := signIn(t, srv.URL, "laptop")
	req, _ := http.NewRequest("POST", srv.URL+"/api/invites", nil)
	req.Header.Set("Authorization", "Bearer "+owner)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "mlxlab.example.org")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Code string `json:"code"`
		URL  string `json:"url"`
	}
	decode(t, r, &out)
	want := "mlxlab://signin?hub=https%3A%2F%2Fmlxlab.example.org&code=" + out.Code
	if out.URL != want {
		t.Fatalf("url %q, want %q", out.URL, want)
	}
}

func TestDevicesMigration(t *testing.T) {
	dir := t.TempDir()
	token := "old-device-token"
	old := []map[string]any{{
		"id": "abc", "name": "old iPhone", "created": "2026-01-01T00:00:00Z", "lastSeen": "2026-01-02T00:00:00Z",
		"hash": hashToken(token),
	}}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, "devices.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	_, srv := newHub(t, dir)
	d := devicesOf(t, srv.URL, token)["old iPhone"]
	if d["user"] != "" || d["owner"] != true || d["canInvite"] != true || d["invitedBy"] != "" || d["current"] != true {
		t.Fatalf("migrated %v", d)
	}
	// the file is written in the new shape
	var stored []map[string]any
	b, _ = os.ReadFile(filepath.Join(dir, "devices.json"))
	if err := json.Unmarshal(b, &stored); err != nil || len(stored) != 1 || stored[0]["owner"] != true || stored[0]["hash"] != hashToken(token) {
		t.Fatalf("devices.json %s", b)
	}
	// an owner from before may invite
	makeInvite(t, srv.URL, token)
}
