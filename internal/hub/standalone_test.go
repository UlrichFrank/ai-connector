package hub

import (
	"strings"
	"testing"
)

func TestLocalWorkerKeepsItsToken(t *testing.T) {
	dir := t.TempDir()
	s, _ := newHub(t, dir)
	tok, err := s.LocalWorker("this-mac", "local")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.LocalWorker("this-mac", "local")
	if err != nil || again != tok {
		t.Fatalf("second call: %q, %v (want the same token)", again, err)
	}
	if w, ok := s.workers.lookup(tok); !ok || w.Owner != "local" {
		t.Fatalf("worker %+v %v", w, ok)
	}
	// a hub opened again on the same files keeps the worker
	s2, _ := newHub(t, dir)
	if third, _ := s2.LocalWorker("this-mac", "local"); third != tok {
		t.Fatal("a new hub on the same data made another worker")
	}
	if n := len(s2.workers.list()); n != 1 {
		t.Fatalf("%d workers, want 1", n)
	}
}

func TestEnsureConnectorMakesThenChangesOne(t *testing.T) {
	s, _ := newHub(t, t.TempDir())
	set := ConnectorSettings{Name: "mac (ai-connector)", URL: "http://127.0.0.1:1/", Token: "vbp_a", Owner: "local"}
	a, err := s.EnsureConnector(set)
	if err != nil {
		t.Fatal(err)
	}
	if a.URL != "http://127.0.0.1:1" || a.Owner != "local" || !a.Enabled {
		t.Fatalf("made %+v", a)
	}
	set.Token, set.Board = "vbp_b", "b_0123456789abcdef"
	set.Agents = []ConnectorAgent{{Model: "qwen3.5-4b"}, {Model: "claude-code/sonnet"}}
	b, err := s.EnsureConnector(set)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != a.ID || b.Board != set.Board || len(b.Agents) != 2 || b.Agents[1].Kind != kindAgent {
		t.Fatalf("changed %+v (want the same connector, its board and agents)", b)
	}
	if n := len(s.Connectors()); n != 1 {
		t.Fatalf("%d connectors, want 1", n)
	}
	s.connectors.mu.Lock()
	tok := s.connectors.list[0].conf.Token
	s.connectors.mu.Unlock()
	if tok != "vbp_b" {
		t.Fatalf("token %q, want the new one", tok)
	}
	s.Close()
}

func TestEnsureConnectorChecksItsSettings(t *testing.T) {
	s, _ := newHub(t, t.TempDir())
	defer s.Close()
	for _, c := range []struct {
		set   ConnectorSettings
		field string
	}{
		{ConnectorSettings{URL: "ftp://x", Token: "t"}, "url"},
		{ConnectorSettings{URL: "http://x", Token: ""}, "token"},
		{ConnectorSettings{URL: "http://x", Token: "t", Agents: []ConnectorAgent{{Model: "claude-code/opus"}}}, "agents"},
	} {
		if _, err := s.EnsureConnector(c.set); ConnectorErrorField(err) != c.field {
			t.Errorf("%+v: %v, want an error about %s", c.set, err, c.field)
		}
	}
}

func TestLocalClientKeepsItsToken(t *testing.T) {
	dir := t.TempDir()
	s, _ := newHub(t, dir)
	worker, err := s.LocalWorker("this-mac", "local")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.LocalClient("local apps", "local")
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := s.clients.lookup(tok); !ok || c.Owner != "local" || c.Name != "local apps" {
		t.Fatalf("client %+v %v", c, ok)
	}
	// a hub opened again on the same files keeps the client, and the worker beside it
	s2, _ := newHub(t, dir)
	if again, _ := s2.LocalClient("local apps", "local"); again != tok {
		t.Fatal("a new hub on the same data made another client")
	}
	if w, _ := s2.LocalWorker("this-mac", "local"); w != worker {
		t.Fatal("the client token replaced the worker token")
	}
	if n := len(s2.clients.list()); n != 1 {
		t.Fatalf("%d clients, want 1", n)
	}
}

// The standalone hub of `ai-connector local --lan` (Config.Owner): a device signed in with the password acts for the
// hub's one owner whatever user name it gives, so an app linked with a PIN it approves runs Claude Code on the hub's
// own worker (owner "local").
func TestStandaloneLinkedClientRunsClaude(t *testing.T) {
	s, srv := newHubWith(t, Config{Password: password, Data: t.TempDir(), Tools: t.TempDir(), Owner: "local"})
	base := srv.URL
	tok, err := s.LocalWorker("this-mac", "local")
	if err != nil {
		t.Fatal(err)
	}
	wk, _ := s.workers.lookup(tok)
	dev := signInAs(t, base, "phone", "Philipp")
	var info map[string]any
	decode(t, do(t, "GET", base+"/api/info", dev, nil), &info)
	if info["user"] != "local" {
		t.Fatalf("signed in as %v, want the hub's owner", info["user"])
	}

	st := startLink(t, base, "OpenCAD")
	if code, out := decideLink(t, base, dev, normalizePIN(st.UserCode), true); code != 200 || out["client"].(map[string]any)["owner"] != "local" {
		t.Fatalf("approve: %d %v", code, out)
	}
	code, out := pollLink(t, base, st.DeviceCode)
	if code != 200 {
		t.Fatalf("token: %d %v", code, out)
	}
	app := out["token"].(string)

	chat, _ := startClaudeChatMac(t, base, tok)
	waitFor(t, "the worker known", func() bool { return workerItems(t, base, dev)[wk.ID].Specs != nil })
	code, b := post(t, base+"/v1/chat/completions", app, `{"model":"claude-code/sonnet","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 || !strings.Contains(string(b), "Hello!") {
		t.Fatalf("v1: %d %s", code, b)
	}
	if got := chat.last(); got.Owner != "local" || got.Client != "OpenCAD" {
		t.Fatalf("job: owner %q client %q", got.Owner, got.Client)
	}
}

// Without an owner (a server) each device keeps the user it names.
func TestServerDeviceKeepsItsUser(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	dev := signInAs(t, srv.URL, "phone", "Philipp")
	var info map[string]any
	decode(t, do(t, "GET", srv.URL+"/api/info", dev, nil), &info)
	if info["user"] != "Philipp" {
		t.Fatalf("user %v", info["user"])
	}
}
