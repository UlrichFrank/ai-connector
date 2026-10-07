package web

import (
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/philipparndt/ai-connector/internal/jobs"
)

// Specs is what this machine is (GET /api/info), for the server to pick a worker for a job.
type Specs struct {
	Chip     string `json:"chip"`
	RAMGB    int    `json:"ramGB"`
	GPUCores int    `json:"gpuCores,omitempty"` // Apple silicon
	MacOS    string `json:"macOS"`              // macOS only (older UIs read it)
	// OS is "macOS", "Linux" or "Windows", OSVersion its version or distribution, Arch the Go architecture.
	OS        string `json:"os"`
	OSVersion string `json:"osVersion,omitempty"`
	Arch      string `json:"arch"`
	// GPU is a discrete GPU (NVIDIA, through nvidia-smi) and its memory; "" on a Mac (unified memory: RAMGB).
	GPU     string `json:"gpu,omitempty"`
	VRAMGB  int    `json:"vramGB,omitempty"`
	Version string `json:"version"` // the worker's build
}

// WorkerState is what this Mac is doing (GET /api/info).
type WorkerState struct {
	Running *string           `json:"running"` // the running job's id (null: idle)
	Queued  int               `json:"queued"`
	Loaded  map[string]string `json:"loaded"` // backend → model in memory
}

// machine asks the system once (a few ms, but not on every request).
var machine = sync.OnceValue(func() Specs {
	sp := probe()
	sp.Arch = runtime.GOARCH
	if sp.GPU == "" {
		sp.GPU, sp.VRAMGB = nvidia()
	}
	return sp
})

// run is a command's trimmed output ("" when it fails).
func run(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// nvidia is the first NVIDIA GPU and its memory in GB (nvidia-smi comes with the driver, on Linux and Windows).
func nvidia() (string, int) {
	line, _, _ := strings.Cut(run("nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits"), "\n")
	name, mib, ok := strings.Cut(line, ",")
	if !ok {
		return "", 0
	}
	m, _ := strconv.Atoi(strings.TrimSpace(mib))
	return strings.TrimSpace(name), (m + 512) / 1024
}

// gb rounds bytes to whole GB.
func gb(b int64) int { return int((b + 1<<29) >> 30) }

// specs is this machine's; MLXLAB_FAKE_RAM_GB and MLXLAB_FAKE_CHIP pretend another one (for trying the hub's
// routing with several workers on one Mac).
func (s *Server) specs() Specs {
	sp := machine()
	if v, err := strconv.Atoi(os.Getenv("MLXLAB_FAKE_RAM_GB")); err == nil && v > 0 {
		sp.RAMGB = v
	}
	if v := os.Getenv("MLXLAB_FAKE_CHIP"); v != "" {
		sp.Chip = v
	}
	sp.Version = s.Version
	if sp.Version == "" {
		sp.Version = "dev"
	}
	return sp
}

func (s *Server) state(rt *route) WorkerState {
	st := WorkerState{Loaded: map[string]string{}}
	for _, j := range s.Jobs.Jobs() {
		switch j.State {
		case jobs.Running:
			id := rt.outID(j.ID)
			st.Running = &id
		case jobs.Queued:
			st.Queued++
		}
	}
	for b, m := range s.Jobs.Loaded() {
		if m != "" {
			st.Loaded[b] = m
		}
	}
	return st
}

// info: GET /api/info — who this is, what it can do (routes: it understands the hub's route header) and what it
// is doing.
func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"app": "mlxlab", "role": "worker", "name": HostName(), "routes": true,
		"specs": s.specs(), "state": s.state(routeOf(r))})
}
