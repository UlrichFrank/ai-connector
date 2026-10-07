package approvals

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	if l, err := s.List(); err != nil || len(l) != 0 {
		t.Fatal(l, err)
	}
	if s.Has("a__b") {
		t.Fatal("empty list allows")
	}
	for _, n := range []string{"smarthome__set_light", "vboard__create_card", "smarthome__set_light"} {
		if err := s.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	l, _ := s.List()
	if !slices.Equal(l, []string{"smarthome__set_light", "vboard__create_card"}) {
		t.Fatal(l)
	}
	if !s.Has("vboard__create_card") || s.Has("vboard__delete_card") {
		t.Fatal("has")
	}
	b, _ := os.ReadFile(filepath.Join(dir, File))
	if string(b) != "{\n  \"always\": [\n    \"smarthome__set_light\",\n    \"vboard__create_card\"\n  ]\n}\n" {
		t.Fatalf("%q", b)
	}
	if err := s.Remove("smarthome__set_light"); err != nil {
		t.Fatal(err)
	}
	if s.Has("smarthome__set_light") {
		t.Fatal("removed")
	}
	// another process edits the file: seen at the next call
	if err := os.WriteFile(filepath.Join(dir, File), []byte(`{"always":["x__y","x__z"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !s.Has("x__z") || s.Has("vboard__create_card") {
		t.Fatal("not read again")
	}
	// a broken file allows nothing and is an error for the list
	_ = os.WriteFile(filepath.Join(dir, File), []byte(`{`), 0o644)
	if s.Has("x__z") {
		t.Fatal("broken file allows")
	}
	if _, err := s.List(); err == nil {
		t.Fatal("no error")
	}
	if err := s.Add("q__r"); err == nil {
		t.Fatal("a broken list was overwritten")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatal("temp files left", ents)
	}
}
