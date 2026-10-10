package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveVboardKeepsWhatWasGiven(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	none := func(string) string { return "" }
	if _, err := resolveVboard(vboardSettings{}, none, file); err == nil {
		t.Fatal("no address and no token: want an error")
	}
	if _, err := resolveVboard(vboardSettings{URL: "https://v"}, none, file); err == nil {
		t.Fatal("no token: want an error")
	}
	got, err := resolveVboard(vboardSettings{URL: "https://v", Token: "vbp_1", Agents: "a,b"}, none, file)
	if err != nil || got.Token != "vbp_1" {
		t.Fatalf("%+v %v", got, err)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file: %v %v (want 0600: it holds the token)", fi, err)
	}
	// later: nothing given → the kept settings; a flag or the environment wins over them
	again, _ := resolveVboard(vboardSettings{}, none, file)
	if again != got {
		t.Fatalf("%+v, want %+v", again, got)
	}
	env := func(k string) string {
		if k == "VBOARD_TOKEN" {
			return "vbp_env"
		}
		return ""
	}
	if s, _ := resolveVboard(vboardSettings{}, env, file); s.Token != "vbp_env" || s.Agents != "a,b" {
		t.Fatalf("%+v", s)
	}
	if s, _ := resolveVboard(vboardSettings{Token: "vbp_flag"}, env, file); s.Token != "vbp_flag" {
		t.Fatalf("%+v", s)
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
	if _, err := vboardAgents("claude-code/opus", ""); err == nil {
		t.Fatal("Claude Code first without --chat: want an error")
	}
	as, err = vboardAgents("claude-code/opus", "qwen3.5-4b")
	if err != nil || len(as) != 2 || as[0].Model != "qwen3.5-4b" {
		t.Fatalf("the chat model goes first: %+v %v", as, err)
	}
}
