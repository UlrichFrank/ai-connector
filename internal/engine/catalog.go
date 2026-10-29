package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/philipparndt/ai-connector/internal/llm"
	"github.com/philipparndt/ai-connector/internal/models"
)

// Model is one entry of the catalogue.
type Model struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    Kind   `json:"kind"`
	Backend string `json:"backend"`
	About   string `json:"about"`
	Size    int64  `json:"size"` // bytes to download (or on disk, when here)
	// Memory: the GB its runtime takes at its peak, where that is known better than from the size (the server's
	// router reckons with it; 0: from the size)
	Memory float64 `json:"memory,omitempty"`
	Here   bool    `json:"here"`  // the files are on this machine
	Where  string  `json:"where"` // where they are ("" while not here)
	Gated  bool    `json:"gated"` // Hugging Face wants a token and an accepted licence
	// Licence is the weights' licence ("Apache-2.0", "CC BY-NC 4.0", …); NonCommercial: not for commercial use
	// (the pickers show it).
	Licence       string `json:"licence,omitempty"`
	NonCommercial bool   `json:"nonCommercial,omitempty"`
	// Tasks are the studio tasks a song model can do besides making a new song (cover, repaint, extend, lego, extract);
	// an image model with tasks does only those: "generate" (text to image, no reference images), "edit" (needs
	// reference images), "upscale" (one image made larger, no prompt: SeedVR2) or "cutout" (one image's background made
	// transparent, no prompt: BiRefNet). An image model without tasks does
	// the first two (FLUX.2 klein).
	Tasks []string `json:"tasks,omitempty"`
	// Styles: a song model that can learn a style from the user's tracks (a LoRA, see styles.go); its requests
	// take a style.
	Styles bool `json:"styles,omitempty"`
	// Images: a chat model that takes images with the messages (llm.Message.Images): Claude Code, and the models a
	// local runtime says can see (LM Studio type "vlm", Ollama capability "vision"). The built-in MLX chat cannot.
	Images bool `json:"images,omitempty"`
	// Audio: what a music, sound or speech model takes (audiocaps.go; vboard's generators)
	Audio *AudioCaps `json:"audio,omitempty"`
	// Defaults are the request fields the UIs start with.
	Defaults Request `json:"defaults"`
	// Sampling: a chat model's settings with thinking on and off (generation_config.json and Qwen's
	// recommendations) — what a chat request gets for the fields it leaves unset (the chat settings show them).
	Sampling *llm.Defaults `json:"sampling,omitempty"`

	chat   *llm.Chat     // a catalogue chat model
	dir    string        // a chat model of your own (a folder)
	pinned *models.Model // the classifier, the embedder
	repo   string        // Hugging Face repository (Python backends)
	rev    string        // pinned revision ("" = the newest)
	files  []string      // the files a backend needs of repo ("" = all but ignore)
	ignore []string      // file patterns not to download
	mflux  string        // mflux's model name
	hq     bool          // video: LTX's two-stage HQ pipeline (the dev transformer with CFG, then the distilled LoRA)
	side   int           // cutout: the side the model sees (BiRefNet 1024, the HR ones 2048)

	served      *servedModel // a model of a local OpenAI-compatible server (OpenAIBackend)
	runtime     string       // music: the runtime (MusicBackend.Runtimes)
	workerModel string       // music: the model id the worker knows
	repos       []hubRepo    // music: the repositories it needs
	maxSeconds  float64      // music: the longest song it makes (0: no limit of its own)
	lora        *musicLoRA
}

// Catalog is the list of models: Models, built when the worker starts, and the models of local runtimes (Ollama,
// LM Studio, …), which come and go while it runs (SetRuntime).
type Catalog struct {
	Models []*Model
	HFHub  string

	mu      sync.RWMutex
	runtime map[string][]*Model // backend → its models now
}

// List is every model: the built-in ones, then those of the local runtimes.
func (c *Catalog) List() []*Model {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := append([]*Model(nil), c.Models...)
	names := make([]string, 0, len(c.runtime))
	for b := range c.runtime {
		names = append(names, b)
	}
	sort.Strings(names)
	for _, b := range names {
		out = append(out, c.runtime[b]...)
	}
	return out
}

// SetRuntime replaces the models a runtime backend offers now.
func (c *Catalog) SetRuntime(backend string, ms []*Model) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runtime == nil {
		c.runtime = map[string][]*Model{}
	}
	c.runtime[backend] = ms
}

// AddRuntime adds the models of ms a runtime backend does not offer yet (by id) and keeps the rest; it returns how
// many were added.
func (c *Catalog) AddRuntime(backend string, ms []*Model) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runtime == nil {
		c.runtime = map[string][]*Model{}
	}
	have := map[string]bool{}
	for _, m := range c.runtime[backend] {
		have[m.ID] = true
	}
	cur := append([]*Model(nil), c.runtime[backend]...)
	n := 0
	for _, m := range ms {
		if !have[m.ID] {
			cur = append(cur, m)
			have[m.ID] = true
			n++
		}
	}
	c.runtime[backend] = cur
	return n
}

// Keep drops the built-in models keep says no to (those of backends this machine cannot run). Call it before the
// catalogue is used.
func (c *Catalog) Keep(keep func(*Model) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ms := c.Models[:0]
	for _, m := range c.Models {
		if keep(m) {
			ms = append(ms, m)
		}
	}
	c.Models = ms
}

// Find looks a model up by its id (or a unique prefix of it).
func (c *Catalog) Find(id string) *Model {
	id = strings.ToLower(strings.TrimSpace(id))
	var hit *Model
	for _, m := range c.List() {
		if m.ID == id {
			return m
		}
		if strings.HasPrefix(m.ID, id) {
			if hit != nil {
				return nil
			}
			hit = m
		}
	}
	return hit
}

// OfKind lists the models of one kind.
func (c *Catalog) OfKind(k Kind) []*Model {
	var out []*Model
	for _, m := range c.List() {
		if m.Kind == k {
			out = append(out, m)
		}
	}
	return out
}

const gb = int64(1) << 30

// NewCatalog builds the catalogue: the vboard models (chat, embedder, classifier), local MLX chat models (LM
// Studio, the Hugging Face cache), FLUX.2 klein, Z-Image Turbo, Qwen-Image, the SeedVR2 upscalers, the BiRefNet cutouts and LTX-2.5.
func NewCatalog(store *models.Store, finder *llm.Finder) *Catalog {
	_, hub := llm.LocalRoots()
	c := &Catalog{HFHub: hub}
	add := func(m *Model) { c.Models = append(c.Models, m) }

	// chat: the catalogue of vboard-ai
	known := map[string]bool{} // repositories and folders already listed
	for i := range llm.Catalogue {
		ch := llm.Catalogue[i]
		known[strings.ToLower(ch.Repo)] = true
		m := &Model{ID: ch.Short, Name: ch.Name, Kind: Chat, Backend: "mlx-go", About: ch.About, Size: ch.Size(), chat: &ch,
			Sampling: ptr(llm.DefaultsFor(""))}
		if ch, err := finder.Resolve(ch.Short, false); err == nil && ch.Dir != "" {
			m.Here, m.Where = true, ch.Where+" ("+ch.Dir+")"
			m.Sampling = ptr(llm.DefaultsFor(ch.Dir))
			known[realPath(ch.Dir)] = true
		}
		add(m)
	}
	// chat: other MLX models on this machine the engine can run
	lms, _ := llm.LocalRoots()
	for _, l := range llm.ScanLocal(lms, hub) {
		if known[strings.ToLower(l.Name)] || known[realPath(l.Dir)] {
			continue
		}
		if _, err := llm.CheckDir(l.Dir); err != nil {
			continue
		}
		known[strings.ToLower(l.Name)] = true
		short := l.Name[strings.LastIndex(l.Name, "/")+1:]
		add(&Model{ID: strings.ToLower(short), Name: short, Kind: Chat, Backend: "mlx-go", About: l.Name + ", found in " + l.Where, Size: dirSize(l.Dir),
			Here: true, Where: l.Where + " (" + l.Dir + ")", dir: l.Dir, Sampling: ptr(llm.DefaultsFor(l.Dir))})
	}

	// the embedder and the classifier of vboard (pure Go, Accelerate / Metal)
	for _, p := range []struct {
		m     models.Model
		id    string
		kind  Kind
		about string
	}{
		{models.Qwen3Embedding, "qwen3-embedding-0.6b", Embed, "Qwen3-Embedding-0.6B: compares texts (cosine similarity), as vboard's \"Group by theme\""},
		{models.Laya, "laya-typed-decisions", Classify, "Laya typed-decisions: answers a choice question about each text, as vboard's \"Sort into frames\""},
	} {
		pm := p.m
		m := &Model{ID: p.id, Name: pm.Name, Kind: p.kind, Backend: "go", About: p.about, Size: pm.Size(), pinned: &pm}
		if store.Missing(pm) == 0 {
			m.Here, m.Where = true, store.Path(pm)
		} else if hfHas(hub, pm.Repo, pm.Rev) {
			m.Here, m.Where = true, "the Hugging Face cache (copied on first use)"
		}
		add(m)
	}

	// every other model: manifests, catalog/*.json (docs/models.md) — images (mflux, BiRefNet), video (LTX-2.5),
	// avatars, songs, speech, sound effects; here when the store (or the Hugging Face cache) has their files
	for _, mf := range builtinManifests() {
		add(mf.model(store, hub))
	}

	sortByKind(c.Models)
	return c
}

// hfSnapshot is a snapshot directory of repo in the Hugging Face cache (rev "" = the one refs/main names).
func hfSnapshot(hub, repo, rev string) string {
	if hub == "" {
		return ""
	}
	base := filepath.Join(hub, "models--"+strings.ReplaceAll(repo, "/", "--"))
	if rev == "" {
		b, err := os.ReadFile(filepath.Join(base, "refs", "main"))
		if err != nil {
			return ""
		}
		rev = strings.TrimSpace(string(b))
	}
	d := filepath.Join(base, "snapshots", rev)
	if st, err := os.Stat(d); err == nil && st.IsDir() {
		return d
	}
	return ""
}

func hfHas(hub, repo, rev string) bool { return hfSnapshot(hub, repo, rev) != "" }

// GatedError explains how to get a gated model.
func GatedError(repo string) error {
	return fmt.Errorf("%s is gated on Hugging Face: accept its licence on https://huggingface.co/%s, then log in "+
		"(`uvx hf auth login`, or export HF_TOKEN) and try again", repo, repo)
}

func isGated(s string) bool {
	return strings.Contains(s, "GatedRepoError") || strings.Contains(s, "Cannot access gated repo")
}

// hasAll: dir has a file for every pattern (the Hugging Face cache links a file into the snapshot once it is complete).
func hasAll(dir string, patterns ...string) bool {
	for _, p := range patterns {
		if m, _ := filepath.Glob(filepath.Join(dir, p)); len(m) == 0 {
			return false
		}
	}
	return true
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// dirSize adds up the files under dir (following the Hugging Face cache's symlinks).
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if st, err := os.Stat(p); err == nil {
			n += st.Size()
		}
		return nil
	})
	return n
}

func ptr[T any](v T) *T { return &v }
