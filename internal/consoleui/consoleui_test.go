package consoleui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// a build as vite writes it
var built = fstest.MapFS{
	"dist/.keep":                {Data: nil},
	"dist/index.html":           {Data: []byte(`<!doctype html><script src="/console/assets/index-abc.js"></script>`)},
	"dist/favicon.svg":          {Data: []byte("<svg/>")},
	"dist/assets/index-abc.js":  {Data: []byte("console.log(1)")},
	"dist/assets/index-abc.css": {Data: []byte("body{}")},
}

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestServesTheBuild(t *testing.T) {
	h := handler(built)
	for _, tc := range []struct {
		path, want, cache, ctype string
		code                     int
	}{
		{"/console/", "/console/assets/index-abc.js", "no-cache", "text/html", 200},
		{"/console/index.html", "/console/assets/index-abc.js", "no-cache", "text/html", 200},
		// the app routes itself: any other path below /console/ is the app
		{"/console/workers/abc", "/console/assets/index-abc.js", "no-cache", "text/html", 200},
		{"/console/favicon.svg", "<svg/>", "no-cache", "image/svg+xml", 200},
		{"/console/assets/index-abc.js", "console.log(1)", "immutable", "javascript", 200},
		{"/console/assets/index-abc.css", "body{}", "immutable", "text/css", 200},
		// a missing asset is a 404, not the app
		{"/console/assets/gone.js", "", "", "", 404},
	} {
		w := get(t, h, "GET", tc.path)
		if w.Code != tc.code {
			t.Errorf("%s: %d, want %d", tc.path, w.Code, tc.code)
			continue
		}
		if tc.code != 200 {
			continue
		}
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: body %q, want %q", tc.path, w.Body.String(), tc.want)
		}
		if !strings.Contains(w.Header().Get("Cache-Control"), tc.cache) {
			t.Errorf("%s: Cache-Control %q, want %q", tc.path, w.Header().Get("Cache-Control"), tc.cache)
		}
		if !strings.Contains(w.Header().Get("Content-Type"), tc.ctype) {
			t.Errorf("%s: Content-Type %q, want %q", tc.path, w.Header().Get("Content-Type"), tc.ctype)
		}
	}
}

func TestRedirectsAndMethods(t *testing.T) {
	h := handler(built)
	w := get(t, h, "GET", "/console?x=1")
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/console/?x=1" {
		t.Errorf("/console: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get(t, h, "POST", "/console/"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
	if w := get(t, h, "HEAD", "/console/"); w.Code != 200 {
		t.Errorf("HEAD: %d", w.Code)
	}
	if w := get(t, h, "GET", "/other"); w.Code != 404 {
		t.Errorf("/other: %d", w.Code)
	}
}

func TestNotBuilt(t *testing.T) {
	h := handler(fstest.MapFS{"dist/.keep": {}})
	w := get(t, h, "GET", "/console/")
	b, _ := io.ReadAll(w.Body)
	if w.Code != 404 || !strings.Contains(string(b), "make console") {
		t.Errorf("not built: %d %q", w.Code, b)
	}
}
