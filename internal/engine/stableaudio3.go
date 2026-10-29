package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Stable Audio 3 (Stability AI, github.com/Stability-AI/stable-audio-3) runs on its official pure-MLX runtime, the
// optimized/mlx folder of that repository: plain Python modules (no package to install), so mlxlab fetches that
// folder once at a pinned commit and the worker (workers/stableaudio3.py) imports it. The weights come from the
// Hugging Face repository stabilityai/stable-audio-3-optimized (MLX/*.npz, not gated), pinned in musicModels.

// sa3Commit is the pinned commit of github.com/Stability-AI/stable-audio-3 (2026-09-29).
const sa3Commit = "3a82c807b69cf4b7c5c05270011a5d5e47abac18"

// sa3Source is where the code is fetched from (tests point it elsewhere).
var sa3Source = "https://codeload.github.com/Stability-AI/stable-audio-3/tar.gz/" + sa3Commit

// fetchSA3 makes ~/.config/mlxlab/tools/stable-audio-3-<commit>/optimized/mlx (once) and returns it as the
// worker's argument.
func fetchSA3(ctx context.Context, root string, emit Emit) ([]string, error) {
	dir := filepath.Join(root, "tools", "stable-audio-3-"+sa3Commit[:7])
	code := filepath.Join(dir, "optimized", "mlx")
	done := filepath.Join(dir, ".fetched-"+sa3Commit)
	if _, err := os.Stat(done); err == nil {
		return []string{code}, nil
	}
	emit.Status("fetching the Stable Audio 3 MLX runtime (once) …")
	_ = os.RemoveAll(dir) // a half-made one
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sa3Source, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching Stable Audio 3: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching Stable Audio 3: %s", resp.Status)
	}
	n, err := untarSubdir(resp.Body, "optimized/mlx/", dir)
	if err != nil {
		return nil, fmt.Errorf("unpacking Stable Audio 3: %w", err)
	}
	if _, err := os.Stat(filepath.Join(code, "scripts", "sa3_mlx.py")); err != nil {
		return nil, fmt.Errorf("the Stable Audio 3 archive has no optimized/mlx/scripts/sa3_mlx.py (%d files)", n)
	}
	if err := os.WriteFile(done, nil, 0o644); err != nil {
		return nil, err
	}
	return []string{code}, nil
}

// untarSubdir unpacks the regular files below sub (inside the archive's one top folder) of a .tar.gz into dir,
// keeping their paths (sub included); it returns how many it wrote.
func untarSubdir(r io.Reader, sub, dir string) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		_, rel, ok := strings.Cut(h.Name, "/") // drop "stable-audio-3-<commit>/"
		if !ok || !strings.HasPrefix(rel, sub) {
			continue
		}
		rel = path.Clean(rel)
		if strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
			return n, fmt.Errorf("bad path %q in the archive", h.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return n, err
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o755|0o644)
		if err != nil {
			return n, err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return n, err
		}
		n++
	}
}
