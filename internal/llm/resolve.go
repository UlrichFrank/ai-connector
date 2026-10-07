package llm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/philipparndt/ai-connector/internal/models"
)

// Choice is the chat model vboard-ai will run and where it comes from.
type Choice struct {
	Chat  *Chat  // a catalogue model (nil: a model of your own)
	Dir   string // the folder it loads from ("" while it still has to be fetched)
	Where string // "LM Studio", "the Hugging Face cache", "the models directory", "your folder"
	Name  string // how it is shown
}

// Missing is what still has to be fetched (0: it is here).
func (c Choice) Missing(s *models.Store) int64 {
	if c.Dir != "" || c.Chat == nil {
		return 0
	}
	return s.Missing(c.Chat.Model)
}

// Finder picks the chat model: --llm-model, else the catalogue's model for this machine's memory — a copy already on
// the machine first (the models directory, LM Studio, the Hugging Face cache), then another catalogue model that is
// here and fits, else the default is fetched on first use.
type Finder struct {
	Store    *models.Store
	Verifier *Verifier
	LMStudio []string
	HFCache  string
	Mem      int64 // bytes of memory (0: unknown)
	local    []LocalModel
	scanned  bool
}

func (f *Finder) scan() []LocalModel {
	if !f.scanned {
		f.local = ScanLocal(f.LMStudio, f.HFCache)
		f.scanned = true
	}
	return f.local
}

// here finds a copy of a catalogue model on this machine; verify also checks the checksums (else sizes only).
func (f *Finder) here(c Chat, verify bool) (dir, where string, ok bool) {
	if f.Store != nil && f.Store.Missing(c.Model) == 0 {
		return f.Store.Path(c.Model), "the models directory", true
	}
	var cands []LocalModel
	if f.HFCache != "" {
		snap := filepath.Join(f.HFCache, "models--"+strings.ReplaceAll(c.Repo, "/", "--"), "snapshots", c.Rev)
		cands = append(cands, LocalModel{Dir: snap, Name: c.Repo, Where: "the Hugging Face cache"})
	}
	cands = append(cands, f.scan()...)
	for _, l := range cands {
		if !sameSizes(l.Dir, c.Model) {
			continue
		}
		if verify && f.Verifier != nil {
			if err := f.Verifier.Matches(l.Dir, c.Model); err != nil {
				continue
			}
		}
		return l.Dir, l.Where, true
	}
	return "", "", false
}

// Resolve picks the model for want (--llm-model; "" or "auto": by memory). verify checks the checksums of a local
// copy (slow the first time for a large model); without it, sizes decide (for the start-up message).
func (f *Finder) Resolve(want string, verify bool) (Choice, error) {
	want = strings.TrimSpace(want)
	if want == "" || strings.EqualFold(want, "auto") {
		def := Default(f.Mem)
		if dir, where, ok := f.here(def, verify); ok {
			return Choice{Chat: &def, Dir: dir, Where: where, Name: def.Name}, nil
		}
		// a smaller or larger catalogue model that is already here (and meant for this machine) rather than a download
		cs := append([]Chat(nil), Catalogue...)
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].Size() > cs[j].Size() })
		for _, c := range cs {
			if c.Repo == def.Repo || c.MinRAM > f.Mem || !c.Fits(f.Mem) {
				continue
			}
			if dir, where, ok := f.here(c, verify); ok {
				c := c
				return Choice{Chat: &c, Dir: dir, Where: where, Name: c.Name}, nil
			}
		}
		return Choice{Chat: &def, Name: def.Name}, nil
	}
	if c, ok := Find(want); ok {
		if dir, where, ok := f.here(c, verify); ok {
			return Choice{Chat: &c, Dir: dir, Where: where, Name: c.Name}, nil
		}
		return Choice{Chat: &c, Name: c.Name}, nil
	}
	if st, err := os.Stat(want); err == nil && st.IsDir() {
		return f.own(want, "your folder", filepath.Base(want), verify)
	}
	lw := strings.ToLower(want)
	for _, l := range f.scan() {
		ln := strings.ToLower(l.Name)
		if ln == lw || strings.HasSuffix(ln, "/"+lw) {
			return f.own(l.Dir, l.Where, l.Name, verify)
		}
	}
	var names []string
	for _, l := range f.scan() {
		names = append(names, l.Name)
	}
	msg := fmt.Sprintf("--llm-model %s: not in the catalogue, no such folder, and no local model of that name.\nThe catalogue:\n%s", want, CatalogueHelp())
	if len(names) > 0 {
		msg += "Models on this machine: " + strings.Join(names, ", ")
	}
	return Choice{}, fmt.Errorf("%s", strings.TrimRight(msg, "\n"))
}

// own is a model folder you named: a catalogue model if its files are the pinned ones, else checked for consistency.
func (f *Finder) own(dir, where, name string, verify bool) (Choice, error) {
	for _, c := range Catalogue {
		if sameSizes(dir, c.Model) {
			if !verify || f.Verifier == nil || f.Verifier.Matches(dir, c.Model) == nil {
				c := c
				return Choice{Chat: &c, Dir: dir, Where: where, Name: c.Name}, nil
			}
		}
	}
	if verify {
		if _, err := CheckDir(dir); err != nil {
			return Choice{}, fmt.Errorf("%s (%s): %w", name, dir, err)
		}
	}
	return Choice{Dir: dir, Where: where, Name: name}, nil
}

// Ensure makes the chosen model available: a catalogue model that is not here is fetched (pinned, checked); a local
// copy is checked (the checksums of a catalogue model, the files of your own).
func (f *Finder) Ensure(ctx context.Context, c Choice) (Choice, error) {
	if c.Dir != "" {
		if c.Chat != nil && f.Verifier != nil && c.Where != "the models directory" {
			if err := f.Verifier.Matches(c.Dir, c.Chat.Model); err != nil {
				return c, fmt.Errorf("the copy of %s in %s is not the pinned one (%v)", c.Chat.Repo, c.Dir, err)
			}
		} else if c.Chat == nil {
			if _, err := CheckDir(c.Dir); err != nil {
				return c, err
			}
		}
		return c, nil
	}
	if c.Chat == nil {
		return c, fmt.Errorf("no folder for %s", c.Name)
	}
	dir, err := f.Store.Ensure(ctx, c.Chat.Model)
	if err != nil {
		return c, err
	}
	c.Dir, c.Where = dir, "the models directory"
	return c, nil
}
