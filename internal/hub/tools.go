package hub

import (
	"crypto/subtle"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Platforms the worker program is built for (Dockerfile), as GOOS-GOARCH: its file under <Tools>/mlxlab/.
var Platforms = []string{"darwin-arm64", "linux-amd64", "linux-arm64", "windows-amd64"}

// platform maps what a machine calls itself — `uname -s`-`uname -m` ("Darwin-arm64", "Linux-x86_64",
// "Linux-aarch64") or Windows' names ("Windows-AMD64", "windows-amd64") — to a GOOS-GOARCH ("" when unknown).
func platform(p string) string {
	osName, arch, ok := strings.Cut(strings.ToLower(p), "-")
	if !ok {
		return ""
	}
	switch arch {
	case "x86_64", "x64", "amd64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	}
	for _, q := range Platforms {
		if q == osName+"-"+arch {
			return q
		}
	}
	return ""
}

// tool serves GET /tools/mlxlab/{platform}: the worker program of the same build as the server, for the asking
// machine's platform, from <Tools>/mlxlab/<GOOS-GOARCH>. It needs a worker token (enrolled or the legacy shared one)
// as a bearer, or a signed-in device.
func (s *Server) tool(w http.ResponseWriter, r *http.Request) {
	p := platform(r.PathValue("platform"))
	if p == "" {
		http.Error(w, "workers run on "+strings.Join(Platforms, ", ")+" (an Apple silicon Mac, Linux or Windows on x86-64 or arm64)", http.StatusNotFound)
		return
	}
	if !s.mayDownload(r) {
		http.Error(w, "unknown token: use the token from Connect a worker (or sign in)", http.StatusUnauthorized)
		return
	}
	f, err := os.Open(filepath.Join(s.cfg.Tools, "mlxlab", p))
	if errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "this server has no worker program for "+p+" (it is built into the server image)", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.Error(w, "this server has no worker program for "+p, http.StatusNotFound)
		return
	}
	name := "ai-connector"
	if strings.HasPrefix(p, "windows-") {
		name += ".exe"
	}
	w.Header().Set("Content-Type", "application/octet-stream") // set, so ServeContent does not sniff
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, fi.ModTime(), f) // Content-Length (and ranges) from the file
}

// mayDownload: a worker token as the bearer, or a device token (bearer, cookie or ?token=).
func (s *Server) mayDownload(r *http.Request) bool {
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		bearer = strings.TrimSpace(bearer)
		if _, ok := s.workers.lookup(bearer); ok {
			return true
		}
		if s.cfg.WorkerToken != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(s.cfg.WorkerToken)) == 1 {
			return true
		}
	}
	_, ok := s.devices.lookup(tokenOf(r))
	return ok
}
