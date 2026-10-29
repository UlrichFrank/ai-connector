package engine

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/philipparndt/ai-connector/internal/pyenv"
)

// Python runtimes as data (docs/models.md, "Runtimes"): a runtime is a JSON manifest — its worker script, an optional
// build step, and one environment per platform (the packages differ: PyTorch's CUDA or ROCm wheels, MLX only on Apple
// silicon). A worker offers a runtime's models only where the runtime has an environment for its platform.

//go:embed runtimes/*.json
var runtimeFiles embed.FS

// RuntimeManifest is one Python runtime.
type RuntimeManifest struct {
	Name   string                `json:"name"`
	Notes  string                `json:"notes,omitempty"`
	Script string                `json:"script,omitempty"` // in workers/ (a local runtime: beside its manifest)
	Build  string                `json:"build,omitempty"`  // a build step of its own (levo2, sa3)
	Envs   map[string]RuntimeEnv `json:"envs"`             // by platform (Platform)
	// Generic: run by the generic backend (RuntimeBackend) — the job protocol with the request as it comes, no Go of
	// its own; Kinds are what its models may be, Output the extension of what it makes
	Generic bool   `json:"generic,omitempty"`
	Kinds   []Kind `json:"kinds,omitempty"`
	Output  string `json:"output,omitempty"`
	Heavy   bool   `json:"heavy,omitempty"` // one heavy model in memory at a time (the queue unloads the others first)

	dir string // a local runtime's directory (its script is there)
}

// RuntimeEnv is a runtime's environment on one platform.
type RuntimeEnv struct {
	Python   string   `json:"python"`
	Packages []string `json:"packages,omitempty"`
	Index    string   `json:"index,omitempty"`
}

// Platforms a runtime can have an environment for.
var Platforms = []string{"darwin-arm64", "linux-cuda", "linux-rocm", "linux-cpu", "windows-cuda", "windows-rocm", "windows-cpu"}

var runtimeBuilds = map[string]bool{"levo2": true, "sa3": true}

// ParseRuntime reads one runtime manifest and checks it.
func ParseRuntime(data []byte) (*RuntimeManifest, error) {
	var rt RuntimeManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rt); err != nil {
		return nil, err
	}
	switch {
	case !manifestID.MatchString(rt.Name):
		return nil, fmt.Errorf("name %q: lower case letters, digits, '.', '_' and '-'", rt.Name)
	case len(rt.Envs) == 0:
		return nil, fmt.Errorf("%s: envs (one per platform) are needed", rt.Name)
	case rt.Build != "" && !runtimeBuilds[rt.Build]:
		return nil, fmt.Errorf("%s: no build step %q", rt.Name, rt.Build)
	case rt.Generic && (rt.Script == "" || len(rt.Kinds) == 0 || rt.Output == ""):
		return nil, fmt.Errorf("%s: a generic runtime needs script, kinds and output", rt.Name)
	}
	for p, e := range rt.Envs {
		if !contains(Platforms, p) {
			return nil, fmt.Errorf("%s: platform %q is none of %s", rt.Name, p, strings.Join(Platforms, ", "))
		}
		if e.Python == "" {
			return nil, fmt.Errorf("%s: %s: python (the version) is needed", rt.Name, p)
		}
	}
	return &rt, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

var (
	runtimesMu sync.RWMutex
	runtimes   = sync.OnceValue(func() map[string]*RuntimeManifest {
		out := map[string]*RuntimeManifest{}
		names, _ := fs.Glob(runtimeFiles, "runtimes/*.json")
		for _, n := range names {
			b, _ := runtimeFiles.ReadFile(n)
			rt, err := ParseRuntime(b)
			if err != nil {
				panic(fmt.Sprintf("the embedded runtimes: %s: %v", n, err))
			}
			out[rt.Name] = rt
		}
		return out
	})
)

// runtimeManifest is the runtime name (nil: none).
func runtimeManifest(name string) *RuntimeManifest {
	runtimesMu.RLock()
	defer runtimesMu.RUnlock()
	return runtimes()[name]
}

// runtimeNames are the runtimes there are, sorted.
func runtimeNames() []string {
	runtimesMu.RLock()
	defer runtimesMu.RUnlock()
	var out []string
	for n := range runtimes() {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// envFor is the runtime's environment on a platform (ok false: it has none there).
func envFor(name, platform string) (pyenv.Env, bool) {
	rt := runtimeManifest(name)
	if rt == nil {
		return pyenv.Env{}, false
	}
	e, ok := rt.Envs[platform]
	if !ok {
		return pyenv.Env{}, false
	}
	return pyenv.Env{Name: name, Python: e.Python, Packages: e.Packages, Index: e.Index}, true
}

// runtimeEnv is the runtime's environment on this machine (the zero Env where it has none: RuntimeRuns says).
func runtimeEnv(name string) pyenv.Env {
	e, _ := envFor(name, Platform())
	return e
}

// RuntimeRuns: the runtime has an environment for this machine.
func RuntimeRuns(name string) bool {
	_, ok := envFor(name, Platform())
	return ok
}

// Platform is this machine as the runtimes name it: darwin-arm64 (Apple silicon), else the system and its GPU —
// linux-cuda, linux-rocm, linux-cpu, windows-cuda, windows-rocm, windows-cpu. MLXLAB_GPU (cuda, rocm, cpu) overrides
// what is found: an NVIDIA driver (nvidia-smi), AMD's ROCm (rocminfo, /opt/rocm; Windows: the HIP SDK's HIP_PATH).
var Platform = sync.OnceValue(func() string { return platformOf(runtime.GOOS, runtime.GOARCH, os.Getenv("MLXLAB_GPU"), haveGPU) })

func platformOf(goos, goarch, gpu string, have func(string) bool) string {
	if goos == "darwin" && goarch == "arm64" {
		return "darwin-arm64"
	}
	if gpu == "" {
		switch {
		case have("cuda"):
			gpu = "cuda"
		case have("rocm"):
			gpu = "rocm"
		default:
			gpu = "cpu"
		}
	}
	return goos + "-" + gpu
}

func haveGPU(kind string) bool {
	switch kind {
	case "cuda":
		_, err := exec.LookPath("nvidia-smi")
		return err == nil
	case "rocm":
		if runtime.GOOS == "windows" {
			return os.Getenv("HIP_PATH") != ""
		}
		if _, err := exec.LookPath("rocminfo"); err == nil {
			return true
		}
		_, err := os.Stat("/opt/rocm")
		return err == nil
	}
	return false
}

// AddLocalRuntimes adds the runtimes of dir (<config dir>/runtimes.d/<name>/runtime.json with its script beside it):
// a machine's own Python runtimes, run by the generic backend. Only when the worker allows them
// (allowLocalRuntimes: they run code nobody reviewed); a runtime the catalogue has already is left out.
func AddLocalRuntimes(dir string) []error {
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return []error{err}
	}
	var errs []error
	runtimesMu.Lock()
	defer runtimesMu.Unlock()
	all := runtimes()
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		d := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(filepath.Join(d, "runtime.json"))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		rt, err := ParseRuntime(b)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
		case !rt.Generic:
			errs = append(errs, fmt.Errorf("%s: a local runtime is a generic one (generic: true)", e.Name()))
		case all[rt.Name] != nil:
			errs = append(errs, fmt.Errorf("%s: the catalogue has a runtime %s already; left out", e.Name(), rt.Name))
		default:
			if _, err := os.Stat(filepath.Join(d, rt.Script)); err != nil {
				errs = append(errs, fmt.Errorf("%s: its script: %w", e.Name(), err))
				continue
			}
			rt.dir = d
			all[rt.Name] = rt
		}
	}
	return errs
}

// script is the runtime's worker script: its source.
func (rt *RuntimeManifest) source() ([]byte, error) {
	if rt.dir != "" {
		return os.ReadFile(filepath.Join(rt.dir, rt.Script))
	}
	return workerFiles.ReadFile("workers/" + rt.Script)
}

// runtimeName is the Python runtime a model runs in ("" none: Go, a local server, Claude Code).
func runtimeName(m *Model) string {
	switch m.Backend {
	case "mflux", "cutout", "sound", "speech":
		return m.Backend
	case "ltx":
		return "ltx-2-mlx"
	case "music":
		return m.runtime
	case "avatar":
		if m.runtime == "longcat" {
			return "longcat-avatar"
		}
		return "ltx-2-mlx"
	}
	if rt := runtimeManifest(m.Backend); rt != nil && rt.Generic {
		return m.Backend
	}
	return ""
}

// ModelRuns: the model's Python runtime (if it has one) has an environment for this machine.
func ModelRuns(m *Model) bool {
	n := runtimeName(m)
	return n == "" || RuntimeRuns(n)
}

// packageCommit is the commit a runtime's package is pinned to ("name @ git+https://…@<commit>…") on Apple silicon.
func packageCommit(runtime, pkg string) string {
	e, _ := envFor(runtime, "darwin-arm64")
	for _, p := range e.Packages {
		if !strings.HasPrefix(p, pkg+" @ ") {
			continue
		}
		at := strings.LastIndex(p, "@")
		c := p[at+1:]
		if i := strings.IndexAny(c, "#&"); i >= 0 {
			c = c[:i]
		}
		return c
	}
	panic("runtime " + runtime + " pins no package " + pkg)
}

// GenericRuntimes are the generic runtimes with an environment for this machine (each gets a RuntimeBackend).
func GenericRuntimes() []string {
	var out []string
	for _, n := range runtimeNames() {
		if rt := runtimeManifest(n); rt.Generic && RuntimeRuns(n) {
			out = append(out, n)
		}
	}
	return out
}
