package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalAIFile(t *testing.T) {
	root := t.TempDir()
	if err := writeLocalAI(root, localAI{URL: "http://127.0.0.1:4711", Token: "aic_x", PID: 42}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, localAIFile)
	st, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", st.Mode().Perm())
	}
	b, _ := os.ReadFile(file)
	var got localAI
	if err := json.Unmarshal(b, &got); err != nil || got.URL != "http://127.0.0.1:4711" || got.Token != "aic_x" || got.PID != 42 {
		t.Fatalf("%s: %+v %v", b, got, err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Fatalf("%d files, want only %s", len(entries), localAIFile)
	}
	// another process's file stays
	removeLocalAI(root, 7)
	if _, err := os.Stat(file); err != nil {
		t.Fatal("removed another process's file")
	}
	removeLocalAI(root, 42)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("the own file is still there")
	}
}
