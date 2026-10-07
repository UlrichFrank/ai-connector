// Package approvals keeps the tools the user allowed always (docs/chat-tools.md, "Always allow" for tools): home
// tools that change something but may run without an approval card. The list lives on the worker in
// <config dir>/approvals.json ({"always": ["smarthome__set_light", …]}), so it holds for every client.
package approvals

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// File is the name of the list in the config dir.
const File = "approvals.json"

// Store is the list on disk. It is read again whenever the file changed (another process, a hand edit), so a
// running worker sees every change at its next call.
type Store struct {
	Path string

	mu    sync.Mutex
	list  []string
	mtime time.Time
	size  int64
	read  bool
}

// Open is the store in dir (the worker's config dir).
func Open(dir string) *Store { return &Store{Path: filepath.Join(dir, File)} }

type file struct {
	Always []string `json:"always"`
}

// load refreshes the cache if the file changed (called with mu held). A missing file is an empty list.
func (s *Store) load() ([]string, error) {
	info, err := os.Stat(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		s.list, s.read, s.mtime, s.size = nil, true, time.Time{}, 0
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if s.read && info.ModTime().Equal(s.mtime) && info.Size() == s.size {
		return s.list, nil
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, err
	}
	var f file
	if len(b) > 0 {
		if err := json.Unmarshal(b, &f); err != nil {
			s.read = false
			return nil, err
		}
	}
	s.list, s.read, s.mtime, s.size = normalize(f.Always), true, info.ModTime(), info.Size()
	return s.list, nil
}

func normalize(l []string) []string {
	out := []string{}
	for _, n := range l {
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// List is the tools allowed always, sorted.
func (s *Store) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	return append([]string{}, l...), err
}

// Has says the tool is allowed always (an unreadable list allows nothing).
func (s *Store) Has(tool string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	return err == nil && slices.Contains(l, tool)
}

// Add allows a tool always.
func (s *Store) Add(tool string) error {
	return s.change(func(l []string) []string { return append(l, tool) })
}

// Remove asks again for a tool.
func (s *Store) Remove(tool string) error {
	return s.change(func(l []string) []string {
		return slices.DeleteFunc(l, func(n string) bool { return n == tool })
	})
}

func (s *Store) change(f func([]string) []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(file{Always: normalize(f(slices.Clone(l)))}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	// atomic: a reader sees the old list or the new one, never half of it
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".approvals-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // gone after the rename; left over only on failure
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		return err
	}
	s.read = false // read back at the next call
	return nil
}
