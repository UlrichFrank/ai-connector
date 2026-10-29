package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveVboardKeepsWhatWasGiven(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	none := func(string) string { return "" }
	if _, err := resolveVboard(vboardSettings{}, nil, none, file); err == nil {
		t.Fatal("no address and no token: want an error")
	}
	if _, err := resolveVboard(vboardSettings{URL: "https://v"}, nil, none, file); err == nil {
		t.Fatal("no token: want an error")
	}
	got, err := resolveVboard(vboardSettings{URL: "https://v", Token: "vbp_1", Agents: "a,b"}, nil, none, file)
	if err != nil || got.Token != "vbp_1" {
		t.Fatalf("%+v %v", got, err)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file: %v %v (want 0600: it holds the token)", fi, err)
	}
	// later: nothing given → the kept settings; a flag or the environment wins over them
	again, _ := resolveVboard(vboardSettings{}, nil, none, file)
	if again != got {
		t.Fatalf("%+v, want %+v", again, got)
	}
	env := func(k string) string {
		if k == "VBOARD_TOKEN" {
			return "vbp_env"
		}
		return ""
	}
	if s, _ := resolveVboard(vboardSettings{}, nil, env, file); s.Token != "vbp_env" || s.Agents != "a,b" {
		t.Fatalf("%+v", s)
	}
	if s, _ := resolveVboard(vboardSettings{Token: "vbp_flag"}, nil, env, file); s.Token != "vbp_flag" {
		t.Fatalf("%+v", s)
	}
}

func TestResolveVboardClearsAnEmptyFlag(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	none := func(string) string { return "" }
	if _, err := resolveVboard(vboardSettings{URL: "https://v", Token: "vbp_1", Board: "b1"}, nil, none, file); err != nil {
		t.Fatal(err)
	}
	if s, _ := resolveVboard(vboardSettings{}, nil, none, file); s.Board != "b1" {
		t.Fatalf("not given: want the kept board, got %+v", s)
	}
	if s, _ := resolveVboard(vboardSettings{}, map[string]bool{"board": true}, none, file); s.Board != "" {
		t.Fatalf("--board=: want no board, got %+v", s)
	}
	if s, _ := resolveVboard(vboardSettings{}, nil, none, file); s.Board != "" || s.Token != "vbp_1" {
		t.Fatalf("the cleared board is kept cleared: %+v", s)
	}
}

func TestVboardAgents(t *testing.T) {
	as, err := vboardAgents(" qwen3.5-4b , claude-code/sonnet=Sonnet,", "")
	if err != nil || len(as) != 2 || as[0].Model != "qwen3.5-4b" || as[1].Name != "Sonnet" {
		t.Fatalf("%+v %v", as, err)
	}
	if as, _ := vboardAgents("", ""); len(as) != 0 {
		t.Fatalf("empty list: %+v", as)
	}
	// Claude Code first without --chat: the default chat model goes first (Claude Code leads only while there is none)
	if as, err := vboardAgents("claude-code/opus", ""); err != nil || len(as) != 2 || as[0].Model != "" || as[0].ID != "default" {
		t.Fatalf("Claude Code first: %+v %v", as, err)
	}
	as, err = vboardAgents("claude-code/opus", "qwen3.5-4b")
	if err != nil || len(as) != 2 || as[0].Model != "qwen3.5-4b" {
		t.Fatalf("the chat model goes first: %+v %v", as, err)
	}
}

func TestResolveVboardKeepsTheAddress(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	none := func(string) string { return "" }
	if _, err := resolveVboard(vboardSettings{URL: "https://v", Token: "vbp_1", Addr: "0.0.0.0:8486"}, nil, none, file); err == nil {
		t.Fatal("--addr 0.0.0.0: want an error")
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("a refused address was kept")
	}
	if _, err := resolveVboard(vboardSettings{URL: "https://v", Token: "vbp_1", Addr: "127.0.0.1:8486"}, nil, none, file); err != nil {
		t.Fatal(err)
	}
	// the service starts without flags: the address comes from the file
	if s, _ := resolveVboard(vboardSettings{}, nil, none, file); s.Addr != "127.0.0.1:8486" {
		t.Fatalf("%+v: want the kept address", s)
	}
	if s, _ := resolveVboard(vboardSettings{}, map[string]bool{"addr": true}, none, file); s.Addr != "" {
		t.Fatalf("--addr=: want a free port again, got %+v", s)
	}
}
