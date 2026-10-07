package models

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func gitSHA1(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// Small files of a Hugging Face listing are pinned by their git blob hash; a wrong one is refused.
func TestGitSHA1Pin(t *testing.T) {
	content := []byte(`{"model_type":"x"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()
	src, _ := Mirror(srv.URL, nil)
	m := Model{Repo: "org/m", Rev: "r1", Files: []File{{Path: "config.json", Size: int64(len(content)), GitSHA1: gitSHA1(content)}}}
	s := &Store{Dir: t.TempDir(), Sources: []Source{src}}
	if _, err := s.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if s.Missing(m) != 0 {
		t.Fatal("not marked as here")
	}
	bad := m
	bad.Rev = "r2"
	bad.Files = []File{{Path: "config.json", Size: int64(len(content)), GitSHA1: gitSHA1([]byte("other content!!!!!"))}}
	if _, err := s.Ensure(context.Background(), bad); err == nil {
		t.Fatal("wrong content accepted")
	}
}

// A file the Hugging Face cache holds is hard-linked, not downloaded, and counted as progress.
func TestLinkFromHFCache(t *testing.T) {
	hub := t.TempDir()
	t.Setenv("HF_HUB_CACHE", hub)
	content := []byte("weights weights weights")
	sum := sha256.Sum256(content)
	blobs := filepath.Join(hub, "models--org--m", "blobs")
	os.MkdirAll(blobs, 0o755)
	os.WriteFile(filepath.Join(blobs, hex.EncodeToString(sum[:])), content, 0o644)
	m := Model{Repo: "org/m", Rev: "r", Files: []File{{Path: "w.safetensors", Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}}}
	var got int64
	s := &Store{Dir: t.TempDir(), Bytes: func(n int64) { got += n }} // no sources: only the link can work
	s.Sources = []Source{{Name: "none", URL: func(Model, File) string { return "http://127.0.0.1:1/x" }}}
	if _, err := s.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if got != int64(len(content)) || s.Missing(m) != 0 {
		t.Fatalf("bytes %d, missing %d", got, s.Missing(m))
	}
}

func TestMatches(t *testing.T) {
	for _, c := range []struct {
		p    string
		pats []string
		want bool
	}{
		{"transformer/x.safetensors", []string{"transformer/"}, true},
		{"flux-2-klein-4b.safetensors", []string{"transformer/", "vae/"}, false},
		{"ltx-2.5-22b-distilled-lora-450-bf16.safetensors", []string{"*distilled-lora*"}, true},
		{"transformer-dev.safetensors", []string{"transformer-dev.safetensors"}, true},
		{"sub/.gitattributes", []string{".gitattributes"}, true},
	} {
		if got := Matches(c.p, c.pats...); got != c.want {
			t.Errorf("Matches(%q, %q) = %v", c.p, c.pats, got)
		}
	}
}
