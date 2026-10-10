package pyenv

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// uv for the Python runtimes: the one on the PATH, else Astral's standalone uv in <root>/bin, fetched there once
// (pinned to UVVersion, checked against the release's sha256.sum) — nobody has to install Homebrew for it.

// UVVersion is the uv release fetched when none is installed.
const UVVersion = "0.13.0"

// uvReleases is where the releases are (tests point it elsewhere).
var uvReleases = "https://github.com/astral-sh/uv/releases/download"

var uvMu sync.Mutex // one fetch at a time

// uvTarget is the release asset for this machine ("" none).
func uvTarget(goos, goarch string) string {
	arch := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[goarch]
	if arch == "" {
		return ""
	}
	switch goos {
	case "darwin":
		return "uv-" + arch + "-apple-darwin.tar.gz"
	case "linux":
		return "uv-" + arch + "-unknown-linux-gnu.tar.gz"
	case "windows":
		return "uv-" + arch + "-pc-windows-msvc.zip"
	}
	return ""
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// UV is the uv to use: on the PATH, else <root>/bin/uv, fetched when it is not there yet (log says so).
func UV(ctx context.Context, root string, log func(string)) (string, error) {
	if p, err := exec.LookPath("uv"); err == nil {
		return p, nil
	}
	uvMu.Lock()
	defer uvMu.Unlock()
	p := filepath.Join(root, "bin", exeName("uv"))
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p, nil
	}
	target := uvTarget(runtime.GOOS, runtime.GOARCH)
	if target == "" {
		return "", ErrNoUV
	}
	log(fmt.Sprintf("fetching uv %s (%s) into %s (once) …", UVVersion, target, filepath.Dir(p)))
	if err := fetchUV(ctx, target, p); err != nil {
		return "", fmt.Errorf("fetching uv %s: %w (or install it: https://docs.astral.sh/uv/)", UVVersion, err)
	}
	return p, nil
}

// fetchUV downloads the release asset, checks it against the release's sha256.sum and puts its uv at dst.
func fetchUV(ctx context.Context, target, dst string) error {
	base := uvReleases + "/" + UVVersion + "/"
	sums, err := get(ctx, base+"sha256.sum", 1<<20)
	if err != nil {
		return err
	}
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		// "<hex> *<name>" (or two spaces)
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == target {
			want = strings.ToLower(f[0])
		}
	}
	if len(want) != 64 {
		return fmt.Errorf("sha256.sum names no %s", target)
	}
	archive, err := get(ctx, base+target, 256<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("%s: SHA-256 %s, the release says %s", target, got, want)
	}
	bin, err := uvFrom(archive, target)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".part"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// uvFrom is the uv program in the release archive (<target>/uv in a .tar.gz, uv.exe in a .zip).
func uvFrom(archive []byte, target string) ([]byte, error) {
	if strings.HasSuffix(target, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if path.Base(f.Name) == "uv.exe" {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, 512<<20))
			}
		}
		return nil, errors.New("no uv.exe in " + target)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("no uv in %s (%v)", target, err)
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == "uv" {
			return io.ReadAll(io.LimitReader(tr, 512<<20))
		}
	}
}

func get(ctx context.Context, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, max))
}
