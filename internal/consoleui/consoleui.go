// Package consoleui is the console of ai-connector (console/), built into dist/ by `make console`. The server
// serves it under /console/ (docs/api.md, "Console UI and host names"); the mlxlab app (internal/webui) stays at /.
package consoleui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// Prefix is where the console is served; its build refers to its assets below it (vite base "/console/").
const Prefix = "/console/"

//go:embed all:dist
var dist embed.FS

// Built reports whether the console was built (dist/index.html exists).
func Built() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return err == nil
}

// Handler serves the console under Prefix: hashed assets are cached for good, everything else is checked on each
// load, and any other path below Prefix gets index.html (the app routes itself).
func Handler() http.Handler {
	return handler(dist)
}

func handler(fsys fs.FS) http.Handler {
	sub, _ := fs.Sub(fsys, "dist")
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == strings.TrimSuffix(Prefix, "/") {
			target := Prefix
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		name, ok := strings.CutPrefix(r.URL.Path, Prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		index, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "the console is not built into this binary (make console)", http.StatusNotFound)
			return
		}
		// a hashed asset: there or not (no index.html for a missing script)
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			serveFile(files, w, r, name)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		if name != "" && name != "index.html" && !strings.HasSuffix(name, "/") {
			if st, err := fs.Stat(sub, name); err == nil && !st.IsDir() {
				serveFile(files, w, r, name)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
	})
}

// serveFile serves name from the file server (which wants a rooted path).
func serveFile(files http.Handler, w http.ResponseWriter, r *http.Request, name string) {
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + name
	r2.URL.RawPath = ""
	files.ServeHTTP(w, r2)
}
