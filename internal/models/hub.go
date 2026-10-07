package models

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrGated: Hugging Face refuses the repository (gated: the licence is not accepted, or no token).
var ErrGated = errors.New("gated")

// HFToken is the Hugging Face token: $HF_TOKEN, else the one `hf auth login` saved ($HF_TOKEN_PATH,
// $HF_HOME/token, ~/.cache/huggingface/token).
func HFToken() string {
	for _, k := range []string{"HF_TOKEN", "HUGGING_FACE_HUB_TOKEN"} {
		if t := strings.TrimSpace(os.Getenv(k)); t != "" {
			return t
		}
	}
	p := os.Getenv("HF_TOKEN_PATH")
	if p == "" {
		if hf := os.Getenv("HF_HOME"); hf != "" {
			p = filepath.Join(hf, "token")
		} else if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, ".cache", "huggingface", "token")
		}
	}
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

// FromHub lists the files of repo at rev from the Hugging Face API, pinned like the built-in models: large (LFS)
// files by SHA-256, small ones by their git blob hash. keep picks the files (nil: all). The list is kept in the store
// (<repo>/<rev>/.files.json), so later runs need no network.
func (s *Store) FromHub(ctx context.Context, name, repo, rev string, keep func(path string) bool) (Model, error) {
	m := Model{Name: name, Repo: repo, Rev: rev}
	cache := filepath.Join(s.Dir, filepath.FromSlash(repo), rev, ".files.json")
	var all []File
	if b, err := os.ReadFile(cache); err == nil && json.Unmarshal(b, &all) == nil && len(all) > 0 {
		m.Files = filter(all, keep)
		return m, nil
	}
	all, err := hubTree(ctx, s.Client, repo, rev)
	if err != nil {
		return m, err
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err == nil {
		if b, err := json.Marshal(all); err == nil {
			_ = os.WriteFile(cache, b, 0o644)
		}
	}
	m.Files = filter(all, keep)
	return m, nil
}

// HubRepo is what the Hugging Face API says about a repository at a revision.
type HubRepo struct {
	Repo    string
	SHA     string // the commit the revision names
	Gated   bool   // the licence must be accepted on its page first
	License string // the model card's licence ("" none stated)
	Author  string
}

// HubInfo asks the Hugging Face API about repo at rev ("" the main branch): its commit, gating and licence.
func (s *Store) HubInfo(ctx context.Context, repo, rev string) (HubRepo, error) {
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	u := "https://huggingface.co/api/models/" + repo
	if rev != "" {
		u += "/revision/" + rev
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return HubRepo{}, err
	}
	if t := HFToken(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	res, err := client.Do(req)
	if err != nil {
		return HubRepo{}, fmt.Errorf("asking Hugging Face about %s: %w", repo, err)
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return HubRepo{}, ErrGated
	case http.StatusNotFound:
		return HubRepo{}, fmt.Errorf("%w: no repository %s on Hugging Face (or not at %q)", ErrNotFound, repo, rev)
	default:
		return HubRepo{}, fmt.Errorf("asking Hugging Face about %s: HTTP %d", repo, res.StatusCode)
	}
	var v struct {
		ID       string          `json:"id"`
		SHA      string          `json:"sha"`
		Gated    json.RawMessage `json:"gated"` // false, or "auto" / "manual"
		Author   string          `json:"author"`
		CardData struct {
			License json.RawMessage `json:"license"` // a string or a list
		} `json:"cardData"`
	}
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		return HubRepo{}, fmt.Errorf("asking Hugging Face about %s: %w", repo, err)
	}
	h := HubRepo{Repo: repo, SHA: v.SHA, Author: v.Author}
	if v.ID != "" {
		h.Repo = v.ID
	}
	g := strings.TrimSpace(string(v.Gated))
	h.Gated = g != "" && g != "false" && g != "null"
	var one string
	var many []string
	if json.Unmarshal(v.CardData.License, &one) == nil {
		h.License = one
	} else if json.Unmarshal(v.CardData.License, &many) == nil {
		h.License = strings.Join(many, ", ")
	}
	if h.SHA == "" {
		return h, fmt.Errorf("Hugging Face named no commit for %s", repo)
	}
	return h, nil
}

func filter(fs []File, keep func(string) bool) []File {
	var out []File
	for _, f := range fs {
		if keep == nil || keep(f.Path) {
			out = append(out, f)
		}
	}
	return out
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func hubTree(ctx context.Context, client *http.Client, repo, rev string) ([]File, error) {
	if client == nil {
		client = http.DefaultClient
	}
	u := "https://huggingface.co/api/models/" + repo + "/tree/" + rev + "?recursive=true"
	var out []File
	for u != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if t := HFToken(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", repo, err)
		}
		var entries []struct {
			Type, Path, Oid string
			Size            int64
			LFS             *struct {
				Oid  string
				Size int64
			}
		}
		switch res.StatusCode {
		case http.StatusOK:
			err = json.NewDecoder(res.Body).Decode(&entries)
		case http.StatusUnauthorized, http.StatusForbidden:
			err = ErrGated
		default:
			err = fmt.Errorf("listing %s: HTTP %d", repo, res.StatusCode)
		}
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			switch {
			case e.Type != "file":
			case e.LFS != nil:
				out = append(out, File{Path: e.Path, Size: e.LFS.Size, SHA256: e.LFS.Oid})
			default:
				out = append(out, File{Path: e.Path, Size: e.Size, GitSHA1: e.Oid})
			}
		}
		u = ""
		if sm := nextLink.FindStringSubmatch(res.Header.Get("Link")); sm != nil {
			u = sm[1]
		}
	}
	return out, nil
}

// Matches reports whether p matches one of the patterns (path.Match on the whole path or its base name; a pattern
// ending in "/" matches a directory and everything in it).
func Matches(p string, patterns ...string) bool {
	for _, pat := range patterns {
		if strings.HasSuffix(pat, "/") && strings.HasPrefix(p, pat) {
			return true
		}
		if ok, _ := path.Match(pat, p); ok {
			return true
		}
		if ok, _ := path.Match(pat, path.Base(p)); ok {
			return true
		}
	}
	return false
}

// sum is what a verified file's marker holds.
func (f File) sum() string {
	if f.SHA256 == "" && f.GitSHA1 != "" {
		return "git-sha1:" + f.GitSHA1
	}
	return f.SHA256
}

// hasher hashes a file's content the way it is pinned (git's blob hash starts with a header).
func (f File) hasher() hash.Hash {
	if f.SHA256 == "" && f.GitSHA1 != "" {
		h := sha1.New()
		fmt.Fprintf(h, "blob %d\x00", f.Size)
		return h
	}
	return newSHA256()
}

// linkFromHFCache puts a file the Hugging Face cache already holds into place as a hard link (no copy, no download):
// its blob is named by its SHA-256, which pins it like the checksum would.
func (s *Store) linkFromHFCache(m Model, f File, dst string) bool {
	if f.SHA256 == "" {
		return false
	}
	hub := hfHub()
	if hub == "" {
		return false
	}
	blob := filepath.Join(hub, "models--"+strings.ReplaceAll(m.Repo, "/", "--"), "blobs", f.SHA256)
	st, err := os.Stat(blob)
	if err != nil || st.Size() != f.Size {
		return false
	}
	_ = os.Remove(dst)
	if err := os.Link(blob, dst); err != nil {
		return false
	}
	return true
}

func hfHub() string {
	if d := os.Getenv("HF_HUB_CACHE"); d != "" {
		return d
	}
	if hf := os.Getenv("HF_HOME"); hf != "" {
		return filepath.Join(hf, "hub")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cache", "huggingface", "hub")
	}
	return ""
}
