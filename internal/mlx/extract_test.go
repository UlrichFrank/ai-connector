package mlx

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func payload(name, content string) Payload {
	s := sha256.Sum256([]byte(content))
	return Payload{Name: name, SHA256: hex.EncodeToString(s[:]), Open: func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(content)), nil
	}}
}

func TestExtract(t *testing.T) {
	dir := t.TempDir()
	files := []Payload{payload("libmlxc.dylib", "code"), payload("mlx.metallib", "kernels")}
	got, err := Extract(dir, "v1", files)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "v1") {
		t.Fatalf("dir %s", got)
	}
	b, _ := os.ReadFile(filepath.Join(got, "mlx.metallib"))
	if string(b) != "kernels" {
		t.Fatalf("content %q", b)
	}
	// a verified directory is used as it is (Open is not called again)
	calls := 0
	again := []Payload{files[0], files[1]}
	for i := range again {
		f := again[i]
		again[i].Open = func() (io.ReadCloser, error) { calls++; return f.Open() }
	}
	if _, err := Extract(dir, "v1", again); err != nil || calls != 0 {
		t.Fatalf("second extract: %v, %d opens", err, calls)
	}
	// a damaged file is replaced
	os.WriteFile(filepath.Join(got, "mlx.metallib"), []byte("broken"), 0o644)
	if _, err := Extract(dir, "v1", again); err != nil || calls != 2 {
		t.Fatalf("repair: %v, %d opens", err, calls)
	}
	b, _ = os.ReadFile(filepath.Join(got, "mlx.metallib"))
	if string(b) != "kernels" {
		t.Fatalf("repaired content %q", b)
	}
	// no temporary directories are left behind
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("left behind: %v", ents)
	}
}

func TestExtractBadPayload(t *testing.T) {
	dir := t.TempDir()
	bad := payload("mlx.metallib", "kernels")
	bad.SHA256 = strings.Repeat("0", 64)
	if _, err := Extract(dir, "v1", []Payload{bad}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want a checksum error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "v1")); err == nil {
		t.Fatal("a broken runtime was put into place")
	}
}

func TestExtractConcurrent(t *testing.T) {
	dir := t.TempDir()
	files := []Payload{payload("a", strings.Repeat("x", 1<<20)), payload("b", "y")}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = Extract(dir, "v2", files) }()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := verify(filepath.Join(dir, "v2"), files); err != nil {
		t.Fatal(err)
	}
}

func TestParseSums(t *testing.T) {
	m, err := ParseSums("# comment\n" + strings.Repeat("a", 64) + "  libmlxc.dylib\n" + strings.Repeat("b", 64) + " *mlx.metallib\n")
	if err != nil || m["libmlxc.dylib"] != strings.Repeat("a", 64) || m["mlx.metallib"] != strings.Repeat("b", 64) {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := ParseSums("nothex  x\n"); err == nil {
		t.Fatal("bad line accepted")
	}
}
