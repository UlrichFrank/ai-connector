// Package websandbox holds the sources of the web sandbox images (the egress proxy, the tools service, SearXNG
// with the lockdown entrypoint) so the mlxlab binary can build them with `container build` on first use.
// The Go programs use only the standard library; the build context gets its own small go.mod.
package websandbox

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

//go:embed Containerfile.proxy Containerfile.tools Containerfile.searxng searxng/settings.yml
//go:embed proxy/main.go proxy/policy.go tools/main.go tools/html.go lockdown/main_linux.go lockdown/main_other.go
var files embed.FS

const goMod = "module mlxlab-websandbox\n\ngo 1.24\n"

// Image names one image of the sandbox: its Containerfile in the build context.
type Image struct {
	Name string // proxy | tools | searxng
	File string
}

var Images = []Image{
	{"proxy", "Containerfile.proxy"},
	{"tools", "Containerfile.tools"},
	{"searxng", "Containerfile.searxng"},
}

// Hash identifies this version of the sources (images are tagged with it, so a change rebuilds them).
func Hash() string {
	h := sha256.New()
	var names []string
	_ = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	sort.Strings(names)
	for _, n := range names {
		b, _ := files.ReadFile(n)
		h.Write([]byte(n))
		h.Write([]byte{0})
		h.Write(b)
		h.Write([]byte{0})
	}
	h.Write([]byte(goMod))
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// WriteContext writes the build context (Containerfiles, sources, go.mod) into dir.
func WriteContext(dir string) error {
	err := fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644)
}
