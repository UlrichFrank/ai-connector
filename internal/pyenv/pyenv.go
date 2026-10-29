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
	"runtime"
	"slices"
	"strings"
)

// Env is a Python runtime.
type Env struct {
	Name     string
	Python   string   // e.g. "3.12"
	Packages []string // pip requirement specs, pinned
	// Index: a package index for the packages IndexPackages names (PyTorch's CUDA or ROCm wheels:
	// https://download.pytorch.org/whl/rocm6.4); they are installed from it alone, first, and the others from PyPI
	// alone — the indexes never compete for a name (no dependency confusion). "" PyPI only.
	Index         string
	IndexPackages []string // package names, e.g. torch, torchvision
}

// ErrNoUV: uv is not installed.
var ErrNoUV = errors.New("uv is needed for this model, and there is no standalone uv for this machine: install it (https://docs.astral.sh/uv/)")

func (e Env) hash() string {
	spec := e.Python + "\n" + strings.Join(e.Packages, "\n")
	if e.Index != "" { // an environment without one keeps the directory it always had
		spec += "\nindex " + e.Index + " " + strings.Join(e.IndexPackages, " ")
	}
	h := sha256.Sum256([]byte(spec))
	return hex.EncodeToString(h[:4])
}

// Dir is where the environment lives.
func (e Env) Dir(root string) string { return filepath.Join(root, "envs", e.Name+"-"+e.hash()) }

// Bin is a program of the environment (Windows: Scripts\<name>.exe).
func (e Env) Bin(root, name string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(e.Dir(root), "Scripts", name+".exe")
	}
	return filepath.Join(e.Dir(root), "bin", name)
}

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
	uv, err := UV(ctx, root, log) // on the PATH, else fetched into <root>/bin (uv.go)
	if err != nil {
		return err
	}
	dir := e.Dir(root)
	_ = os.RemoveAll(dir) // a half-made one
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	log(fmt.Sprintf("installing the %s runtime into %s (once) …", e.Name, dir))
	if err := Stream(ctx, uvCommand(ctx, uv, "venv", "--python", e.Python, dir), log); err != nil {
		return fmt.Errorf("uv venv: %w", err)
	}
	if len(e.Packages) > 0 { // a worker that needs only the standard library has none
		from, rest := e.split()
		install := func(index string, pkgs []string) error {
			if len(pkgs) == 0 {
				return nil
			}
			args := []string{"pip", "install", "--python", e.Bin(root, "python")}
			if index != "" {
				args = append(args, "--index-url", index)
			}
			return Stream(ctx, uvCommand(ctx, uv, append(args, pkgs...)...), log)
		}
		if err := install(e.Index, from); err != nil {
			return fmt.Errorf("uv pip install (%s): %w", e.Index, err)
		}
		if err := install("", rest); err != nil {
			return fmt.Errorf("uv pip install: %w", err)
		}
	}
	return os.WriteFile(filepath.Join(dir, ".mlxlab-ready"), []byte(strings.Join(e.Packages, "\n")+"\n"), 0o644)
}

// split parts the packages into those from Index (IndexPackages) and the others (PyPI).
func (e Env) split() (from, rest []string) {
	if e.Index == "" {
		return nil, e.Packages
	}
	want := make([]string, len(e.IndexPackages))
	for i, n := range e.IndexPackages {
		want[i] = normName(n)
	}
	for _, p := range e.Packages {
		fs := strings.FieldsFunc(p, func(r rune) bool { return strings.ContainsRune("=<>!~[@ ;", r) })
		if len(fs) == 0 {
			continue
		}
		if slices.Contains(want, normName(fs[0])) {
			from = append(from, p)
		} else {
			rest = append(rest, p)
		}
	}
	return from, rest
}

// normName is a package name as PyPI compares them (PEP 503): lower case, runs of -, _ and . as one -.
func normName(n string) string {
	n = strings.ToLower(strings.TrimSpace(n))
	return strings.Join(strings.FieldsFunc(n, func(r rune) bool { return r == '-' || r == '_' || r == '.' }), "-")
}

// uvCommand runs uv with the system's certificates (macOS keychain, Windows store, Linux CA bundle) instead of
// uv's own: behind a TLS-inspecting proxy (a company network re-signs pypi.org and github.com with its own CA) uv
// otherwise fails with "invalid peer certificate: UnknownIssuer". UV_SYSTEM_CERTS for uv 0.11 and newer,
// UV_NATIVE_TLS for older ones on the PATH; a value the user set wins.
func uvCommand(ctx context.Context, uv string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, uv, args...)
	cmd.Env = uvEnv(os.Environ())
	return cmd
}

func uvEnv(env []string) []string {
	set := func(k string) bool {
		for _, kv := range env {
			if strings.HasPrefix(kv, k+"=") {
				return true
			}
		}
		return false
	}
	if set("UV_SYSTEM_CERTS") || set("UV_NATIVE_TLS") {
		return env
	}
	return append(env, "UV_SYSTEM_CERTS=1", "UV_NATIVE_TLS=1")
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
