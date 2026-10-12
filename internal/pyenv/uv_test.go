package pyenv

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fakeUVRelease(t *testing.T, target string, content []byte, badSum bool) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	dir := strings.TrimSuffix(target, ".tar.gz") + "/"
	for _, f := range []struct {
		name string
		body []byte
	}{{dir + "uvx", []byte("uvx")}, {dir + "uv", content}} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		tw.Write(f.body)
	}
	tw.Close()
	gz.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	hexSum := hex.EncodeToString(sum[:])
	if badSum {
		hexSum = strings.Repeat("0", 64)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /"+UVVersion+"/sha256.sum", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("1111111111111111111111111111111111111111111111111111111111111111 *uv-other.tar.gz\n" + hexSum + " *" + target + "\n"))
	})
	mux.HandleFunc("GET /"+UVVersion+"/"+target, func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Without uv on the PATH the standalone one is fetched into <root>/bin once, checked against the release's
// sha256.sum; a wrong sum keeps it out.
func TestUVFetchedWhenMissing(t *testing.T) {
	target := uvTarget(runtime.GOOS, runtime.GOARCH)
	if target == "" || strings.HasSuffix(target, ".zip") {
		t.Skip("no tar.gz release for this machine")
	}
	t.Setenv("PATH", t.TempDir()) // no uv
	old := uvReleases
	t.Cleanup(func() { uvReleases = old })

	srv := fakeUVRelease(t, target, []byte("#!/bin/sh\necho uv\n"), true)
	uvReleases = srv.URL
	root := t.TempDir()
	if _, err := UV(context.Background(), root, func(string) {}); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("a wrong sum: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "uv")); err == nil {
		t.Fatal("kept a uv with a wrong sum")
	}

	srv = fakeUVRelease(t, target, []byte("#!/bin/sh\necho uv\n"), false)
	uvReleases = srv.URL
	var said []string
	p, err := UV(context.Background(), root, func(s string) { said = append(said, s) })
	if err != nil || p != filepath.Join(root, "bin", "uv") || len(said) != 1 || !strings.Contains(said[0], UVVersion) {
		t.Fatalf("uv %q %v %q", p, err, said)
	}
	if b, _ := os.ReadFile(p); string(b) != "#!/bin/sh\necho uv\n" {
		t.Fatalf("uv is %q", b)
	}
	if st, _ := os.Stat(p); st.Mode()&0o100 == 0 {
		t.Fatalf("not executable: %v", st.Mode())
	}
	srv.Close() // the next call needs no network
	if p2, err := UV(context.Background(), root, func(s string) { t.Errorf("fetched again: %s", s) }); err != nil || p2 != p {
		t.Fatalf("again: %q %v", p2, err)
	}
}

func TestUVTargets(t *testing.T) {
	for _, x := range []struct{ goos, arch, want string }{
		{"darwin", "arm64", "uv-aarch64-apple-darwin.tar.gz"},
		{"linux", "amd64", "uv-x86_64-unknown-linux-gnu.tar.gz"},
		{"windows", "amd64", "uv-x86_64-pc-windows-msvc.zip"},
		{"plan9", "amd64", ""},
		{"linux", "riscv64", ""},
	} {
		if got := uvTarget(x.goos, x.arch); got != x.want {
			t.Errorf("%s/%s: %q", x.goos, x.arch, got)
		}
	}
}

func TestUVEnvSystemCerts(t *testing.T) {
	got := strings.Join(uvEnv([]string{"PATH=/bin"}), " ")
	if !strings.Contains(got, "UV_SYSTEM_CERTS=1") || !strings.Contains(got, "UV_NATIVE_TLS=1") {
		t.Fatalf("uv without the system certificates: %s", got)
	}
	for _, own := range []string{"UV_SYSTEM_CERTS=0", "UV_NATIVE_TLS=false"} {
		if got := uvEnv([]string{own}); len(got) != 1 {
			t.Fatalf("%s overridden: %v", own, got)
		}
	}
}
