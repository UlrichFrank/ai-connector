package web

import (
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/philipparndt/ai-connector/internal/jobs"
)

// Specs is what this Mac is (GET /api/info), for the hub to pick a Mac for a job.
type Specs struct {
	Chip     string `json:"chip"`
	RAMGB    int    `json:"ramGB"`
	GPUCores int    `json:"gpuCores,omitempty"`
	MacOS    string `json:"macOS"`
	Version  string `json:"version"` // mlxlab's
}

// WorkerState is what this Mac is doing (GET /api/info).
type WorkerState struct {
	Running *string           `json:"running"` // the running job's id (null: idle)
	Queued  int               `json:"queued"`
	Loaded  map[string]string `json:"loaded"` // backend → model in memory
}

// machine asks the system once (sysctl, ioreg, sw_vers: a few ms, but not on every request).
var machine = sync.OnceValue(func() Specs {
	run := func(name string, args ...string) string {
		out, err := exec.Command(name, args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	sp := Specs{Chip: run("sysctl", "-n", "machdep.cpu.brand_string"), MacOS: run("sw_vers", "-productVersion")}
	if b, err := strconv.ParseInt(run("sysctl", "-n", "hw.memsize"), 10, 64); err == nil {
		sp.RAMGB = int((b + 1<<29) >> 30)
	}
	if m := gpuCores.FindStringSubmatch(run("ioreg", "-rc", "AGXAccelerator", "-d1")); m != nil {
		sp.GPUCores, _ = strconv.Atoi(m[1])
	}
	return sp
})

var gpuCores = regexp.MustCompile(`"gpu-core-count"\s*=\s*(\d+)`)

// specs is this Mac's; MLXLAB_FAKE_RAM_GB and MLXLAB_FAKE_CHIP pretend another one (for trying the hub's
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
