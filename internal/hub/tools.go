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

// macPlatform is the one platform the worker runs on (`uname -s`-`uname -m` of an Apple silicon Mac).
const macPlatform = "Darwin-arm64"

// tool serves GET /tools/mlxlab/{platform}: the Mac program of the same build as the hub, from
// <Tools>/mlxlab/<platform>. It needs a worker token (enrolled or the legacy shared one) as a bearer, or a
// signed-in device.
func (s *Server) tool(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("platform") != macPlatform {
		http.Error(w, "mlxlab workers run on Apple Silicon Macs (Darwin-arm64)", http.StatusNotFound)
		return
	}
	if !s.mayDownload(r) {
		http.Error(w, "unknown token: use the token from Connect a Mac (or sign in)", http.StatusUnauthorized)
		return
	}
	f, err := os.Open(filepath.Join(s.cfg.Tools, "mlxlab", macPlatform))
	if errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "this hub has no Mac program (it is built into the hub image)", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.Error(w, "this hub has no Mac program", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream") // set, so ServeContent does not sniff
	w.Header().Set("Content-Disposition", `attachment; filename="mlxlab"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "mlxlab", fi.ModTime(), f) // Content-Length (and ranges) from the file
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
