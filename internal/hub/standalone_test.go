package hub

import (
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
