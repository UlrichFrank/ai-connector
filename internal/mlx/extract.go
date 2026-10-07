package mlx

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Payload is one file of the runtime: its name, the SHA-256 of its content and how to read that content (unpacked).
type Payload struct {
	Name   string
	SHA256 string
	Open   func() (io.ReadCloser, error)
}

// ParseSums reads a SHA256SUMS file ("<hex>  <name>" lines, # comments).
func ParseSums(s string) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 || len(f[0]) != 64 {
			return nil, fmt.Errorf("SHA256SUMS: bad line %q", line)
		}
		if _, err := hex.DecodeString(f[0]); err != nil {
			return nil, fmt.Errorf("SHA256SUMS: bad line %q", line)
		}
		out[strings.TrimPrefix(f[1], "*")] = f[0]
	}
	return out, nil
}

// Extract makes sure dir/<version>/ holds the files with their checksums and returns that directory. A complete,
// verified directory is used as it is; otherwise the files are unpacked into a temporary directory next to it, checked
// and renamed into place in one step — a second vboard-ai starting at the same time, or one killed half-way, never
// sees half a runtime.
func Extract(dir, version string, files []Payload) (string, error) {
	final := filepath.Join(dir, version)
	if verify(final, files) == nil {
		return final, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(dir, ".tmp-"+version+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp) // gone after the rename; cleans up after a failure
	for _, f := range files {
		if err := unpack(filepath.Join(tmp, f.Name), f); err != nil {
			return "", err
		}
	}
	if err := verify(tmp, files); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		// another process got there first (or an old broken copy is in the way)
		if verify(final, files) == nil {
			return final, nil
		}
		old := final + ".old"
		_ = os.RemoveAll(old)
		if err := os.Rename(final, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		_ = os.RemoveAll(old)
		if err := os.Rename(tmp, final); err != nil {
			return "", err
		}
	}
	return final, verify(final, files)
}

func unpack(dst string, f Payload) error {
	r, err := f.Open()
	if err != nil {
		return fmt.Errorf("%s: %w", f.Name, err)
	}
	defer r.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return fmt.Errorf("%s: %w", f.Name, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// verify checks every file in dir against its SHA-256.
func verify(dir string, files []Payload) error {
	for _, f := range files {
		sum, err := fileSHA256(filepath.Join(dir, f.Name))
		if err != nil {
			return err
		}
		if sum != f.SHA256 {
			return fmt.Errorf("%s: checksum mismatch (sha256 %s, expected %s)", f.Name, sum, f.SHA256)
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
