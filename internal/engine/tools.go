package engine

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// A server runtime's program from its release archives (RuntimeManifest.Tools): fetched once into
// <config dir>/tools/<runtime>-<hash of the archives>, checked against its SHA-256, unpacked.

// toolDir is where a runtime's tools of this platform live (ok false: it has none here).
func toolDir(root, name string) (string, []ToolArchive, bool) {
	rt := runtimeManifest(name)
	if rt == nil || len(rt.Tools[Platform()]) == 0 {
		return "", nil, false
	}
	as := rt.Tools[Platform()]
	h := sha256.New()
	for _, a := range as {
		io.WriteString(h, a.SHA256)
	}
	return filepath.Join(root, "tools", name+"-"+hex.EncodeToString(h.Sum(nil))[:8]), as, true
}

// ensureTools fetches and unpacks the runtime's archives if they are not there yet.
func ensureTools(ctx context.Context, root, name string, log func(string)) (string, error) {
	dir, as, ok := toolDir(root, name)
	if !ok {
		return "", fmt.Errorf("%s has no program for %s", name, Platform())
	}
	ready := filepath.Join(dir, ".mlxlab-ready")
	if _, err := os.Stat(ready); err == nil {
		return dir, nil
	}
	_ = os.RemoveAll(dir) // a half-made one
	// the worker's user only: a server's app writes its outputs there (ComfyUI)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	for _, a := range as {
		log(fmt.Sprintf("fetching %s (once) …", a.URL[strings.LastIndex(a.URL, "/")+1:]))
		f, err := os.CreateTemp(filepath.Dir(dir), ".tool-*")
		if err != nil {
			return "", err
		}
		err = downloadTool(ctx, a, f)
		f.Close()
		if err == nil {
			err = unpack(f.Name(), a.URL, dir)
		}
		os.Remove(f.Name())
		if err != nil {
			return "", fmt.Errorf("%s: %w", a.URL, err)
		}
	}
	return dir, os.WriteFile(ready, nil, 0o644)
}

func downloadTool(ctx context.Context, a ToolArchive, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), resp.Body); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		return fmt.Errorf("sha256 %s, want %s", got, a.SHA256)
	}
	return nil
}

// unpack extracts a .tar.gz or .zip into dir; a path that would leave dir is refused.
func unpack(file, url, dir string) error {
	inside := func(name string) (string, error) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if p != dir && !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			return "", fmt.Errorf("%s leaves the folder", name)
		}
		return p, nil
	}
	if strings.HasSuffix(url, ".zip") {
		z, err := zip.OpenReader(file)
		if err != nil {
			return err
		}
		defer z.Close()
		for _, f := range z.File {
			p, err := inside(f.Name)
			if err != nil {
				return err
			}
			if f.FileInfo().IsDir() {
				os.MkdirAll(p, 0o755)
				continue
			}
			if err := writeFrom(p, f.Mode()|0o600, func() (io.ReadCloser, error) { return f.Open() }); err != nil {
				return err
			}
		}
		return nil
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p, err := inside(h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(p, 0o755)
		case tar.TypeSymlink:
			if t, err := inside(filepath.Join(filepath.Dir(h.Name), h.Linkname)); err != nil || filepath.IsAbs(h.Linkname) {
				return fmt.Errorf("%s links out of the folder (%s)", h.Name, t)
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFrom(p, os.FileMode(h.Mode)|0o600, func() (io.ReadCloser, error) { return io.NopCloser(tr), nil }); err != nil {
				return err
			}
		}
	}
}

func writeFrom(p string, mode os.FileMode, open func() (io.ReadCloser, error)) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	r, err := open()
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// findFile is the file name in dir: the one least deep (ComfyUI's main.py, not one of a folder inside it).
func findFile(dir, name string) (string, error) {
	var found string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name && (found == "" || strings.Count(p, string(filepath.Separator)) < strings.Count(found, string(filepath.Separator))) {
			found = p
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("no %s in %s", name, dir)
	}
	return found, nil
}

// findTool is the program name in dir (its folder in the archive does not matter; Windows: name.exe).
func findTool(dir, name string) (string, error) {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var found string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && found == "" && !d.IsDir() && d.Name() == name {
			found = p
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("no %s in %s", name, dir)
	}
	return found, nil
}
