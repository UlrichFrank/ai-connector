// Package models fetches the model files on first use into a local directory and verifies them: every file is pinned
// by size and SHA-256 here, so whichever source serves it — Hugging Face, the vboard server's blob store, an
// Artifactory or any HTTP mirror — the bytes are the ones this build was tested with. Afterwards nothing is fetched
// again; the models run from the directory.
package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// File is one file of a model, pinned by size and SHA-256.
type File struct {
	Path    string // relative to the repository root, e.g. "typed-decisions/model.safetensors"
	Size    int64
	SHA256  string
	GitSHA1 string `json:",omitempty"` // instead of SHA256: git's blob hash (the small files of a Hugging Face listing)
}

// Model is a set of files at one revision of a Hugging Face repository.
type Model struct {
	Name   string // how it is reported (hello.model / hello.embed)
	Repo   string // e.g. "convaiinnovations/laya"
	Rev    string // the commit
	Subdir string // the directory the model loads from, relative to the repository root ("" = the root)
	Files  []File
}

// Size is the total size of the files.
func (m Model) Size() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// Laya is the classifier: Laya's typed-decisions checkpoint (docs/plans/laya.md — the best on board content, English
// and German).
var Laya = Model{
	Name: "typed-decisions", Repo: "convaiinnovations/laya", Rev: "55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851", Subdir: "typed-decisions",
	Files: []File{
		{Path: "typed-decisions/rl_agent_config.json", Size: 847, SHA256: "ebf0cd524d92342a6be5e48e9fca3d7c2babfb5a56ccd79d2171ef5d8c7f7be8"},
		{Path: "typed-decisions/encoder/config.json", Size: 2084, SHA256: "5268d24ad3b77c8151de5dcb0762ba4391619aad9ab0bda33e36fb083cfeae6d"},
		{Path: "typed-decisions/tokenizer/tokenizer.json", Size: 3583228, SHA256: "6c8aaa9a542084f2457eab775d4eeb51f92a70c0fd9de28d5edb0ddec3c08d30"},
		{Path: "typed-decisions/model.safetensors", Size: 842609220, SHA256: "4fa56de72383a9d3efa9cfa78955733c81b9fc8067a587ca4beb82c78107a24e"},
	},
}

// Qwen3Embedding is the embedder ("Group by theme", "Find duplicates"): Qwen3-Embedding-0.6B.
var Qwen3Embedding = Model{
	Name: "Qwen/Qwen3-Embedding-0.6B", Repo: "Qwen/Qwen3-Embedding-0.6B", Rev: "97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3",
	Files: []File{
		{Path: "config.json", Size: 727, SHA256: "b5bf1f51fc45be473a54718cef92448d90a1be001bf9b9a44b8c7f10a19feaa9"},
		{Path: "tokenizer.json", Size: 11423705, SHA256: "def76fb086971c7867b829c23a26261e38d9d74e02139253b38aeb9df8b4b50a"},
		{Path: "model.safetensors", Size: 1191586416, SHA256: "0437e45c94563b09e13cb7a64478fc406947a93cb34a7e05870fc8dcd48e23fd"},
	},
}

// Source is where files come from.
type Source struct {
	Name   string
	URL    func(m Model, f File) string
	Header http.Header
}

// HuggingFace is the public source.
func HuggingFace() Source {
	h := http.Header{}
	if tok := HFToken(); tok != "" {
		h.Set("Authorization", "Bearer "+tok) // gated models; higher rate limits
	}
	return Source{Name: "huggingface.co", Header: h, URL: func(m Model, f File) string {
		return "https://huggingface.co/" + m.Repo + "/resolve/" + m.Rev + "/" + escapePath(f.Path)
	}}
}

// HFCache is the local Hugging Face cache (~/.cache/huggingface/hub, $HF_HOME/hub, $HF_HUB_CACHE): a machine that ran
// the Python bridge already has the files — they are copied (and checked) instead of downloaded again.
func HFCache() Source {
	dir := os.Getenv("HF_HUB_CACHE")
	if dir == "" {
		if hf := os.Getenv("HF_HOME"); hf != "" {
			dir = filepath.Join(hf, "hub")
		} else if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".cache", "huggingface", "hub")
		}
	}
	return Source{Name: "the local Hugging Face cache", URL: func(m Model, f File) string {
		return "file://" + filepath.Join(dir, "models--"+strings.ReplaceAll(m.Repo, "/", "--"), "snapshots", m.Rev, filepath.FromSlash(f.Path))
	}}
}

// Mirror is any HTTP server with the files: a base URL (files at <base>/<repo>/<path>), or a template with {repo},
// {rev} and {path} (e.g. an Artifactory remote repository of Hugging Face). header: "Name: value" lines.
func Mirror(spec string, header []string) (Source, error) {
	u, err := url.Parse(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(spec, "{repo}", "r"), "{rev}", "v"), "{path}", "p"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Source{}, fmt.Errorf("model source %q: an http(s) URL, \"auto\", \"huggingface\" or \"none\"", spec)
	}
	h := http.Header{}
	for _, line := range header {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) == "" {
			return Source{}, fmt.Errorf("model source header %q: \"Name: value\"", line)
		}
		h.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	tmpl := strings.Contains(spec, "{")
	return Source{Name: u.Host, Header: h, URL: func(m Model, f File) string {
		if tmpl {
			r := strings.NewReplacer("{repo}", escapePath(m.Repo), "{rev}", m.Rev, "{path}", escapePath(f.Path))
			return r.Replace(spec)
		}
		return strings.TrimRight(spec, "/") + "/" + escapePath(m.Repo) + "/" + escapePath(f.Path)
	}}, nil
}

// Sources reads the --models-from setting: auto (the local Hugging Face cache, then Hugging Face), huggingface, none
// (only files already there) or a mirror URL.
func Sources(spec string, header []string) ([]Source, error) {
	switch strings.ToLower(strings.TrimSpace(spec)) {
	case "", "auto":
		return []Source{HFCache(), HuggingFace()}, nil
	case "huggingface", "hf":
		return []Source{HuggingFace()}, nil
	case "none", "offline":
		return nil, nil
	}
	m, err := Mirror(spec, header)
	if err != nil {
		return nil, err
	}
	return []Source{m}, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// DefaultDir is ~/.config/mlxlab/models ($XDG_CONFIG_HOME/vboard-ai/models; on Windows the user's config directory).
func DefaultDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "mlxlab", "models")
	}
	if home, err := os.UserHomeDir(); err == nil && os.PathSeparator == '/' {
		return filepath.Join(home, ".config", "mlxlab", "models")
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "mlxlab", "models")
	}
	return filepath.Join(".", "mlxlab-models")
}

// Store keeps models in Dir, laid out as <repo>/<rev>/<path>.
type Store struct {
	Dir     string
	Sources []Source
	Client  *http.Client
	// Progress reports downloads (nil: quiet).
	Progress func(format string, args ...any)
	// Bytes is told about every chunk of a file that is now here (downloaded, resumed or linked).
	Bytes func(n int64)
}

// ErrNotFound: no source has the file.
var ErrNotFound = errors.New("not found")

// Path is where the model loads from.
func (s *Store) Path(m Model) string {
	return filepath.Join(s.Dir, filepath.FromSlash(m.Repo), m.Rev, filepath.FromSlash(m.Subdir))
}

func (s *Store) file(m Model, f File) string {
	return filepath.Join(s.Dir, filepath.FromSlash(m.Repo), m.Rev, filepath.FromSlash(f.Path))
}

// Missing is the number of bytes still to fetch.
func (s *Store) Missing(m Model) int64 {
	var n int64
	for _, f := range m.Files {
		if !s.have(m, f) {
			n += f.Size
		}
	}
	return n
}

// have: the file is there with its size and was verified (the marker next to it names its hash).
func (s *Store) have(m Model, f File) bool {
	p := s.file(m, f)
	st, err := os.Stat(p)
	if err != nil || st.Size() != f.Size {
		return false
	}
	mark, err := os.ReadFile(p + ".sha256")
	if err != nil {
		return false
	}
	// "<sum>  <name>": a publisher's checksum file that overwrote the marker (before filter left those out) — the
	// file only got here through its hash check, so the same sum still says verified
	sum, _, _ := strings.Cut(strings.TrimSpace(string(mark)), " ")
	return sum == f.sum()
}

// Ensure fetches the files that are not there yet and returns the directory the model loads from.
func (s *Store) Ensure(ctx context.Context, m Model) (string, error) {
	for _, f := range m.Files {
		if s.have(m, f) {
			continue
		}
		if err := s.fetch(ctx, m, f); err != nil {
			return "", err
		}
	}
	return s.Path(m), nil
}

func (s *Store) fetch(ctx context.Context, m Model, f File) error {
	if len(s.Sources) == 0 {
		return fmt.Errorf("%s of %s is missing in %s and downloads are off (--models-from none)", f.Path, m.Repo, s.Dir)
	}
	dst := s.file(m, f)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if s.linkFromHFCache(m, f, dst) {
		s.bytes(f.Size)
		return os.WriteFile(dst+".sha256", []byte(f.sum()+"\n"), 0o644)
	}
	var errs []string
	for _, src := range s.Sources {
		err := s.download(ctx, src, m, f, dst)
		if err == nil {
			return os.WriteFile(dst+".sha256", []byte(f.sum()+"\n"), 0o644)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		errs = append(errs, src.Name+": "+err.Error())
	}
	return fmt.Errorf("%s of %s could not be fetched (%s)", f.Path, m.Repo, strings.Join(errs, "; "))
}

// download fetches one file into dst (via dst.part, resumed where an earlier run stopped) and checks its hash.
func (s *Store) download(ctx context.Context, src Source, m Model, f File, dst string) error {
	part := dst + ".part"
	h := f.hasher()
	var have int64
	if pf, err := os.Open(part); err == nil {
		have, _ = io.Copy(h, pf)
		pf.Close()
		if have > f.Size {
			have = 0
			h = f.hasher()
		}
	}
	if f.Size == 0 { // an empty file (a package's __init__.py): nothing to fetch, but finish renames the part
		if err := os.WriteFile(part, nil, 0o644); err != nil {
			return err
		}
	}
	if have == f.Size { // complete, not yet checked (an earlier run stopped here)
		s.bytes(have)
		return finish(part, dst, h, f)
	}
	body, status, err := open(ctx, s.Client, src, src.URL(m, f), have)
	if err != nil {
		return err
	}
	if body == nil { // not found, or another HTTP status
		if status == http.StatusNotFound {
			return ErrNotFound
		}
		return fmt.Errorf("HTTP %d", status)
	}
	defer body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case status == http.StatusPartialContent && have > 0:
		flags |= os.O_APPEND
	case status == http.StatusOK:
		have = 0
		h = f.hasher()
		flags |= os.O_TRUNC
	default:
		return fmt.Errorf("HTTP %d", status)
	}
	out, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	if s.Progress != nil && f.Size > 1<<20 {
		s.Progress("fetching %s of %s from %s (%s) …", f.Path, m.Repo, src.Name, human(f.Size))
	}
	s.bytes(have) // resumed
	pr := &progress{r: body, done: have, total: f.Size, report: s.Progress, name: f.Path, last: time.Now(), bytes: s.Bytes}
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(pr, f.Size-have+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err // the part stays: the next attempt resumes it
	}
	if have+n != f.Size {
		if have+n > f.Size {
			os.Remove(part)
		}
		return fmt.Errorf("size %d, expected %d", have+n, f.Size)
	}
	return finish(part, dst, h, f)
}

// finish checks the hash of a complete part and moves it into place (a wrong one is removed).
func finish(part, dst string, h hash.Hash, f File) error {
	want, kind := f.SHA256, "sha256"
	if want == "" {
		want, kind = f.GitSHA1, "git sha1"
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != want {
		os.Remove(part)
		return fmt.Errorf("checksum mismatch (%s %s, expected %s)", kind, sum, want)
	}
	return os.Rename(part, dst)
}

// open starts reading a file from a source at offset from: an HTTP GET (with Range), or a local file (file://).
func open(ctx context.Context, client *http.Client, src Source, u string, from int64) (io.ReadCloser, int, error) {
	if p, ok := strings.CutPrefix(u, "file://"); ok {
		fh, err := os.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, http.StatusNotFound, nil
		}
		if err != nil {
			return nil, 0, err
		}
		if from > 0 {
			if _, err := fh.Seek(from, io.SeekStart); err != nil {
				fh.Close()
				return nil, 0, err
			}
			return fh, http.StatusPartialContent, nil
		}
		return fh, http.StatusOK, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range src.Header {
		req.Header[k] = v
	}
	if from > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
	}
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		res.Body.Close()
		return nil, res.StatusCode, nil
	}
	return res.Body, res.StatusCode, nil
}

type progress struct {
	r           io.Reader
	done, total int64
	report      func(string, ...any)
	name        string
	last        time.Time
	bytes       func(int64)
}

func (p *progress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.bytes != nil && n > 0 {
		p.bytes(int64(n))
	}
	if p.report != nil && p.total > 1<<20 && time.Since(p.last) > 5*time.Second {
		p.last = time.Now()
		p.report("  %s: %d %% of %s", path.Base(p.name), p.done*100/p.total, human(p.total))
	}
	return n, err
}

func (s *Store) bytes(n int64) {
	if s.Bytes != nil && n > 0 {
		s.Bytes(n)
	}
}

func newSHA256() hash.Hash { return sha256.New() }

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d kB", n>>10)
}

// Human formats a size for messages.
func Human(n int64) string { return human(n) }
