package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// testModel has two files; their bytes are served by the test servers.
func testModel() (Model, map[string][]byte) {
	files := map[string][]byte{"sub/config.json": []byte(`{"a":1}`), "sub/weights.bin": []byte(strings.Repeat("w", 5000))}
	m := Model{Name: "test", Repo: "org/model", Rev: "abc123", Subdir: "sub"}
	for _, p := range []string{"sub/config.json", "sub/weights.bin"} {
		m.Files = append(m.Files, File{Path: p, Size: int64(len(files[p])), SHA256: sum(files[p])})
	}
	return m, files
}

// server serves files at /<repo>/<path> (Range requests like a CDN), counting requests and recording headers.
type server struct {
	*httptest.Server
	mu      sync.Mutex
	hits    map[string]int
	headers []http.Header
}

func newServer(t *testing.T, files map[string][]byte, prefix string) *server {
	s := &server{hits: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		b, ok := files[strings.TrimPrefix(r.URL.Path, prefix)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if rg := r.Header.Get("Range"); rg != "" {
			from, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(b)-1, len(b)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(b[from:])
			return
		}
		w.Write(b)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestEnsureDownloadsVerifiesAndKeeps(t *testing.T) {
	m, files := testModel()
	srv := newServer(t, files, "/org/model/")
	src, err := Mirror(srv.URL, []string{"X-JFrog-Art-Api: secret"})
	if err != nil {
		t.Fatal(err)
	}
	var progress []string
	st := &Store{Dir: t.TempDir(), Sources: []Source{src}, Progress: func(f string, a ...any) { progress = append(progress, fmt.Sprintf(f, a...)) }}
	if st.Missing(m) != m.Size() {
		t.Fatalf("missing %d", st.Missing(m))
	}
	dir, err := st.Ensure(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(st.Dir, "org", "model", "abc123", "sub") {
		t.Fatalf("dir %s", dir)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "weights.bin")); string(b) != string(files["sub/weights.bin"]) {
		t.Fatal("weights not written")
	}
	if srv.headers[0].Get("X-JFrog-Art-Api") != "secret" {
		t.Fatalf("header not sent: %v", srv.headers[0])
	}
	// second start: nothing is fetched again (the files are there, verified)
	if _, err := st.Ensure(context.Background(), m); err != nil || len(srv.headers) != 2 || st.Missing(m) != 0 {
		t.Fatalf("second Ensure fetched again: %d requests, %v", len(srv.headers), err)
	}
	// offline (no sources) works once the files are there
	off := &Store{Dir: st.Dir}
	if _, err := off.Ensure(context.Background(), m); err != nil {
		t.Fatalf("offline with files: %v", err)
	}
	// a file changed on disk (another size) is fetched again
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("x"), 0o644)
	if _, err := off.Ensure(context.Background(), m); err == nil || !strings.Contains(err.Error(), "--models-from none") {
		t.Fatalf("offline with a damaged file: %v", err)
	}
}

// A source with other bytes (or without the file) is skipped, the next one is used; a wrong file never stays.
func TestEnsureChecksumAndFallback(t *testing.T) {
	m, files := testModel()
	bad := map[string][]byte{"sub/config.json": []byte(`{"a":2}`)} // tampered, and no weights at all
	evil := newServer(t, bad, "/org/model/")
	good := newServer(t, files, "/org/model/")
	e, _ := Mirror(evil.URL, nil)
	g, _ := Mirror(good.URL, nil)
	st := &Store{Dir: t.TempDir(), Sources: []Source{e, g}}
	if _, err := st.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if evil.hits["/org/model/sub/config.json"] != 1 || good.hits["/org/model/sub/config.json"] != 1 {
		t.Fatalf("hits evil %v good %v", evil.hits, good.hits)
	}
	// only the evil source: the error names the checksum and nothing is kept
	st2 := &Store{Dir: t.TempDir(), Sources: []Source{e}}
	_, err := st2.Ensure(context.Background(), m)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st2.Dir, "org", "model", "abc123", "sub", "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a tampered file was kept")
	}
	matches, _ := filepath.Glob(filepath.Join(st2.Dir, "org", "model", "abc123", "sub", "*"))
	if len(matches) != 0 {
		t.Fatalf("left behind: %v", matches)
	}
}

// An interrupted download resumes with a Range request.
func TestEnsureResumes(t *testing.T) {
	m, files := testModel()
	srv := newServer(t, files, "/org/model/")
	src, _ := Mirror(srv.URL, nil)
	st := &Store{Dir: t.TempDir(), Sources: []Source{src}}
	part := filepath.Join(st.Dir, "org", "model", "abc123", "sub", "weights.bin.part")
	os.MkdirAll(filepath.Dir(part), 0o755)
	os.WriteFile(part, files["sub/weights.bin"][:1234], 0o644)
	if _, err := st.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	var ranged bool
	for _, h := range srv.headers {
		if h.Get("Range") == "bytes=1234-" {
			ranged = true
		}
	}
	if !ranged {
		t.Fatal("no Range request for the partial file")
	}
}

func TestSources(t *testing.T) {
	m, _ := testModel()
	f := m.Files[0]
	srcs, err := Sources("auto", nil)
	if err != nil || len(srcs) != 2 {
		t.Fatalf("auto: %v %v", len(srcs), err)
	}
	if u := srcs[0].URL(m, f); !strings.HasSuffix(u, filepath.Join("models--org--model", "snapshots", "abc123", "sub", "config.json")) || !strings.HasPrefix(u, "file://") {
		t.Fatalf("hf cache: %s", u)
	}
	if u := srcs[1].URL(m, f); u != "https://huggingface.co/org/model/resolve/abc123/sub/config.json" {
		t.Fatalf("hf: %s", u)
	}
	tmpl, _ := Sources("https://art.example/api/huggingfaceml/hf/models/{repo}/resolve/{rev}/{path}", []string{"Authorization: Bearer a"})
	if u := tmpl[0].URL(m, f); u != "https://art.example/api/huggingfaceml/hf/models/org/model/resolve/abc123/sub/config.json" || tmpl[0].Header.Get("Authorization") != "Bearer a" {
		t.Fatalf("template: %s", u)
	}
	if none, err := Sources("none", nil); err != nil || len(none) != 0 {
		t.Fatal("none")
	}
	for _, bad := range []string{"ftp://x", "nonsense"} {
		if _, err := Sources(bad, nil); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := Sources("https://m.example", []string{"no colon"}); err == nil {
		t.Error("bad header accepted")
	}
}

// The local Hugging Face cache (a machine that ran the Python bridge) is a source without any network.
func TestHFCache(t *testing.T) {
	m, files := testModel()
	hub := t.TempDir()
	t.Setenv("HF_HUB_CACHE", hub)
	snap := filepath.Join(hub, "models--org--model", "snapshots", "abc123")
	for p, b := range files {
		os.MkdirAll(filepath.Join(snap, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(snap, p), b, 0o644)
	}
	st := &Store{Dir: t.TempDir(), Sources: []Source{HFCache()}}
	if _, err := st.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if st.Missing(m) != 0 {
		t.Fatal("not copied")
	}
	// not in the cache: not found, so the next source would be asked
	other := m
	other.Rev = "def456"
	if _, err := st.Ensure(context.Background(), other); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing in cache: %v", err)
	}
}

// The pinned models: every file has a size and a SHA-256.
func TestPinned(t *testing.T) {
	for _, m := range []Model{Laya, Qwen3Embedding} {
		if len(m.Rev) != 40 || m.Size() < 1<<30/2 {
			t.Errorf("%s: rev %q size %d", m.Name, m.Rev, m.Size())
		}
		for _, f := range m.Files {
			if len(f.SHA256) != 64 || f.Size <= 0 || !strings.HasPrefix(f.Path, m.Subdir) {
				t.Errorf("%s: %+v", m.Name, f)
			}
		}
	}
}
