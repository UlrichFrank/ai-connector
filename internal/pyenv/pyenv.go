// Package pyenv keeps the Python runtimes of the backends that are not ported to Go (mflux, ltx-2-mlx): one virtual
// environment per runtime under ~/.config/mlxlab/envs/<name>-<hash of the spec>, made by uv on first use. The
// packages are pinned in the spec, so a new spec is a new directory and the old one can be deleted.
package pyenv

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Env is a Python runtime.
type Env struct {
	Name     string
	Python   string   // e.g. "3.12"
	Packages []string // pip requirement specs, pinned
}

// ErrNoUV: uv is not installed.
var ErrNoUV = errors.New("uv is needed for this model (brew install uv)")

func (e Env) hash() string {
	h := sha256.Sum256([]byte(e.Python + "\n" + strings.Join(e.Packages, "\n")))
	return hex.EncodeToString(h[:4])
}

// Dir is where the environment lives.
func (e Env) Dir(root string) string { return filepath.Join(root, "envs", e.Name+"-"+e.hash()) }

// Bin is a program of the environment.
func (e Env) Bin(root, name string) string { return filepath.Join(e.Dir(root), "bin", name) }

// Ready: the environment is installed.
func (e Env) Ready(root string) bool {
	_, err := os.Stat(filepath.Join(e.Dir(root), ".mlxlab-ready"))
	return err == nil
}

// Ensure installs the environment if it is not there yet; log gets uv's output line by line.
func (e Env) Ensure(ctx context.Context, root string, log func(string)) error {
	if e.Ready(root) {
		return nil
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		return ErrNoUV
	}
	dir := e.Dir(root)
	_ = os.RemoveAll(dir) // a half-made one
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	log(fmt.Sprintf("installing the %s runtime into %s (once) …", e.Name, dir))
	if err := Stream(ctx, exec.CommandContext(ctx, uv, "venv", "--python", e.Python, dir), log); err != nil {
		return fmt.Errorf("uv venv: %w", err)
	}
	if len(e.Packages) > 0 { // a worker that needs only the standard library has none
		args := append([]string{"pip", "install", "--python", filepath.Join(dir, "bin", "python")}, e.Packages...)
		if err := Stream(ctx, exec.CommandContext(ctx, uv, args...), log); err != nil {
			return fmt.Errorf("uv pip install: %w", err)
		}
	}
	return os.WriteFile(filepath.Join(dir, ".mlxlab-ready"), []byte(strings.Join(e.Packages, "\n")+"\n"), 0o644)
}

// Stream runs cmd and passes every line it writes (stdout and stderr; a carriage return ends a line too, so
// progress bars come through) to log. The last lines are part of the error when it fails.
func Stream(ctx context.Context, cmd *exec.Cmd, log func(string)) error {
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	var tail []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		Lines(pr, func(s string) {
			tail = append(tail, s)
			if len(tail) > 8 {
				tail = tail[1:]
			}
			log(s)
		})
	}()
	err := cmd.Wait()
	pw.Close()
	<-done
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w\n%s", err, strings.Join(tail, "\n"))
	}
	return nil
}

// Lines splits r at \n and \r and passes the non-empty lines on.
func Lines(r io.Reader, f func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		for i, c := range data {
			if c == '\n' || c == '\r' {
				return i + 1, data[:i], nil
			}
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			f(s)
		}
	}
	_, _ = io.Copy(io.Discard, r)
}
