package llm

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
)

// Local copies: a Mac that runs LM Studio (or mlx_lm) already has MLX models — they are used where they are (read
// only, nothing is copied or changed), a catalogue model once its files match the pinned SHA-256 sums (checked once,
// remembered by size and modification time), any other model after a consistency check of its files.

// LocalRoots are the folders searched for models: LM Studio's (~/.lmstudio/models, ~/.cache/lm-studio/models,
// $LMSTUDIO_MODELS_DIR) with <publisher>/<model> inside, and the Hugging Face cache.
func LocalRoots() (lmstudio []string, hfCache string) {
	home, _ := os.UserHomeDir()
	if d := os.Getenv("LMSTUDIO_MODELS_DIR"); d != "" {
		lmstudio = append(lmstudio, d)
	}
	if home != "" {
		lmstudio = append(lmstudio, filepath.Join(home, ".lmstudio", "models"), filepath.Join(home, ".cache", "lm-studio", "models"))
	}
	hfCache = os.Getenv("HF_HUB_CACHE")
	if hfCache == "" {
		if hf := os.Getenv("HF_HOME"); hf != "" {
			hfCache = filepath.Join(hf, "hub")
		} else if home != "" {
			hfCache = filepath.Join(home, ".cache", "huggingface", "hub")
		}
	}
	return lmstudio, hfCache
}

// LocalModel is a model folder found on this machine.
type LocalModel struct {
	Dir   string
	Name  string // publisher/model (LM Studio) or the repository (Hugging Face cache)
	Where string // "LM Studio", "the Hugging Face cache"
}

// ScanLocal lists the MLX model folders (config.json with a quantization, and safetensors) in LM Studio's folders
// and the Hugging Face cache (every snapshot).
func ScanLocal(lmstudio []string, hfCache string) []LocalModel {
	var out []LocalModel
	seen := map[string]bool{}
	add := func(dir, name, where string) {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil || seen[real] {
			return
		}
		raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil || !strings.Contains(string(raw), "\"quantization") {
			return
		}
		if m, _ := filepath.Glob(filepath.Join(dir, "*.safetensors")); len(m) == 0 {
			return
		}
		seen[real] = true
		out = append(out, LocalModel{Dir: dir, Name: name, Where: where})
	}
	for _, root := range lmstudio {
		pubs, _ := os.ReadDir(root)
		for _, p := range pubs {
			if !p.IsDir() || strings.HasPrefix(p.Name(), ".") {
				continue
			}
			ms, _ := os.ReadDir(filepath.Join(root, p.Name()))
			for _, m := range ms {
				if m.IsDir() && !strings.HasPrefix(m.Name(), ".") {
					add(filepath.Join(root, p.Name(), m.Name()), p.Name()+"/"+m.Name(), "LM Studio")
				}
			}
		}
	}
	if hfCache != "" {
		repos, _ := filepath.Glob(filepath.Join(hfCache, "models--*"))
		for _, r := range repos {
			name := strings.Replace(strings.TrimPrefix(filepath.Base(r), "models--"), "--", "/", 1)
			snaps, _ := os.ReadDir(filepath.Join(r, "snapshots"))
			for _, s := range snaps {
				add(filepath.Join(r, "snapshots", s.Name()), name, "the Hugging Face cache")
			}
		}
	}
	return out
}

// sameSizes: dir has every file of m with its pinned size (a quick test before the checksums).
func sameSizes(dir string, m models.Model) bool {
	for _, f := range m.Files {
		st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil || st.Size() != f.Size {
			return false
		}
	}
	return true
}

// Verifier checks local files against pinned sums and remembers the result (by path, size and modification time)
// in a small JSON file, so a 20 GB model is hashed once, not on every start.
type Verifier struct {
	Path     string // e.g. ~/.config/mlxlab/models/verified.json
	Progress func(format string, args ...any)
	mu       sync.Mutex
}

type verified struct {
	Size   int64     `json:"size"`
	Mtime  time.Time `json:"mtime"`
	SHA256 string    `json:"sha256"`
}

func (v *Verifier) load() map[string]verified {
	m := map[string]verified{}
	if raw, err := os.ReadFile(v.Path); err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}

func (v *Verifier) save(m map[string]verified) {
	raw, _ := json.MarshalIndent(m, "", " ")
	if err := os.MkdirAll(filepath.Dir(v.Path), 0o755); err != nil {
		return
	}
	tmp := v.Path + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, v.Path)
	}
}

// Matches: every file of m in dir has its pinned SHA-256.
func (v *Verifier) Matches(dir string, m models.Model) error {
	if !sameSizes(dir, m) {
		return errors.New("the files differ in size")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	cache := v.load()
	changed := false
	defer func() {
		if changed {
			v.save(cache)
		}
	}()
	told := false
	for _, f := range m.Files {
		p, err := filepath.Abs(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return err
		}
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		c, ok := cache[p]
		if !ok || c.Size != st.Size() || !c.Mtime.Equal(st.ModTime()) {
			if !told && v.Progress != nil && m.Size() > 1<<30 {
				v.Progress("checking the copy of %s in %s against its SHA-256 sums (%s, once) …", m.Repo, dir, models.Human(m.Size()))
				told = true
			}
			sum, err := hashFile(p)
			if err != nil {
				return err
			}
			c = verified{Size: st.Size(), Mtime: st.ModTime(), SHA256: sum}
			cache[p] = c
			changed = true
		}
		if c.SHA256 != f.SHA256 {
			return fmt.Errorf("%s: sha256 %s, expected %s", f.Path, c.SHA256, f.SHA256)
		}
	}
	return nil
}

func hashFile(p string) (string, error) {
	fh, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CheckDir checks a model folder that is not pinned (a model you chose): config.json of a supported model,
// tokenizer.json, and complete safetensors files — each as long as its header says, and every file the index names
// present. It does not prove the weights are the ones published (no pinned checksums for your own models).
func CheckDir(dir string) (*Config, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, err
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, "tokenizer.json")); err != nil {
		return nil, fmt.Errorf("no tokenizer.json in %s", dir)
	}
	files, err := WeightFiles(dir)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := checkSafetensors(f); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// WeightFiles are the model's safetensors files: the ones model.safetensors.index.json names, else every
// *.safetensors in dir.
func WeightFiles(dir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "model.safetensors.index.json"))
	if err == nil {
		var idx struct {
			WeightMap map[string]string `json:"weight_map"`
		}
		if err := json.Unmarshal(raw, &idx); err != nil {
			return nil, fmt.Errorf("model.safetensors.index.json: %w", err)
		}
		set := map[string]bool{}
		for _, f := range idx.WeightMap {
			set[f] = true
		}
		var out []string
		for f := range set {
			if strings.Contains(f, "/") || strings.Contains(f, "\\") || strings.HasPrefix(f, ".") {
				return nil, fmt.Errorf("model.safetensors.index.json names %q", f)
			}
			p := filepath.Join(dir, f)
			if _, err := os.Stat(p); err != nil {
				return nil, fmt.Errorf("%s is missing (named in model.safetensors.index.json)", f)
			}
			out = append(out, p)
		}
		sort.Strings(out)
		if len(out) > 0 {
			return out, nil
		}
	}
	out, _ := filepath.Glob(filepath.Join(dir, "*.safetensors"))
	if len(out) == 0 {
		return nil, fmt.Errorf("no *.safetensors in %s", dir)
	}
	sort.Strings(out)
	return out, nil
}

// checkSafetensors: the file is as long as its header says (8 bytes of header length, the JSON header, the data up to
// the largest end offset) — a download that stopped half-way fails here.
func checkSafetensors(path string) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	var n uint64
	if err := binary.Read(fh, binary.LittleEndian, &n); err != nil || n == 0 || n > 100<<20 {
		return fmt.Errorf("%s: not a safetensors file", filepath.Base(path))
	}
	hdr := make([]byte, n)
	if _, err := io.ReadFull(fh, hdr); err != nil {
		return fmt.Errorf("%s: header cut off", filepath.Base(path))
	}
	var tensors map[string]json.RawMessage
	if err := json.Unmarshal(hdr, &tensors); err != nil {
		return fmt.Errorf("%s: header: %w", filepath.Base(path), err)
	}
	var end uint64
	for k, v := range tensors {
		if k == "__metadata__" {
			continue
		}
		var t struct {
			Offsets [2]uint64 `json:"data_offsets"`
		}
		if json.Unmarshal(v, &t) != nil || t.Offsets[1] < t.Offsets[0] {
			return fmt.Errorf("%s: tensor %s: bad offsets", filepath.Base(path), k)
		}
		end = max(end, t.Offsets[1])
	}
	st, err := fh.Stat()
	if err != nil {
		return err
	}
	if want := int64(8 + n + end); st.Size() != want {
		return fmt.Errorf("%s: %d bytes, the header describes %d (incomplete?)", filepath.Base(path), st.Size(), want)
	}
	return nil
}
