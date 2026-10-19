package engine

import (
	"context"
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
	Size    int64  `json:"size"`  // bytes to download (or on disk, when here)
	Here    bool   `json:"here"`  // the files are on this machine
	Where   string `json:"where"` // where they are ("" while not here)
	Gated   bool   `json:"gated"` // Hugging Face wants a token and an accepted licence
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

	// FLUX.2 klein through mflux (quantised when loaded)
	for _, f := range []struct {
		id, mflux, repo, rev, about string
		steps                       int
		guidance                    float64
		size                        int64
		gated                       bool
	}{
		{"flux2-klein-4b", "flux2-klein-4b", "black-forest-labs/FLUX.2-klein-4B", "e7b7dc27f91deacad38e78976d1f2b499d76a294", "FLUX.2 [klein] 4B, distilled (4 steps), Apache 2.0: text to image and editing with reference images", 4, 1, 16 * gb, false},
		{"flux2-klein-9b", "flux2-klein-9b", "black-forest-labs/FLUX.2-klein-9B", "92196c8e11f7b6cf2b7493e037d8c5345c559216", "FLUX.2 [klein] 9B, distilled (4 steps), non-commercial licence (gated: HF_TOKEN)", 4, 1, 35 * gb, true},
		{"flux2-klein-base-4b", "flux2-klein-base-4b", "black-forest-labs/FLUX.2-klein-base-4B", "a3b4f4849157f664bdbc776fd7453c2783562f4d", "FLUX.2 [klein] 4B base, undistilled (50 steps, guidance 4): slower, more varied", 50, 4, 16 * gb, false},
	} {
		m := &Model{ID: f.id, Name: "FLUX.2 [klein] " + strings.TrimPrefix(f.id, "flux2-klein-"), Kind: Image, Backend: "mflux", About: f.about,
			Size: f.size, Gated: f.gated, repo: f.repo, rev: f.rev, mflux: f.mflux,
			Defaults: Request{Width: 1024, Height: 1024, Steps: f.steps, Guidance: f.guidance, Quantize: 8}, Licence: "Apache-2.0"}
		if f.id == "flux2-klein-9b" {
			m.Licence, m.NonCommercial = "FLUX Non-Commercial License", true
		}
		if d := filepath.Join(store.Dir, filepath.FromSlash(f.repo), f.rev); hasAll(d, "transformer/*.safetensors.sha256", "vae/*.safetensors.sha256", "text_encoder/*.safetensors.sha256") {
			m.Here, m.Where, m.Size = true, d, dirSize(d)
		} else if d := hfSnapshot(hub, f.repo, f.rev); d != "" && hasAll(d, "transformer/*.safetensors", "vae/*.safetensors", "text_encoder/*.safetensors") {
			m.Here, m.Where = true, "the Hugging Face cache (linked on first use)"
		}
		add(m)
	}

	// Z-Image Turbo through mflux: mflux's 4-bit pack, the image generator for 16 GB Macs (zimage.go)
	add(zImageTurbo(store, hub))

	// Qwen-Image through mflux: the 20B MMDiT 2512 (text to image) and Edit 2511 (editing with reference images) with
	// the Qwen2.5-VL 7B text encoder, and the second generation 2.1 (7B transformer, Qwen3-VL encoder). mflux keeps
	// the text encoder in bf16 (~16 GB) and quantises only the transformer and the VAE.
	for _, q := range qwenImages {
		add(q.model(store, hub))
	}

	// SeedVR2 through mflux: upscales an image (upscale.go)
	for _, m := range seedvr2Catalog(store, hub) {
		add(m)
	}

	// BiRefNet through PyTorch: cuts an image's background away (cutout.go)
	for _, m := range cutoutCatalog(store, hub) {
		add(m)
	}

	// LTX-2.5 through ltx-2-mlx: the distilled pipeline, and the HQ pipeline of the dev transformer (ltxModels)
	for _, m := range ltxModels(store) {
		add(m)
	}

	// talking avatars (avatar.go)
	for _, m := range avatarModels(store) {
		add(m)
	}

	// songs: here when the store has every file of every repository
	for _, m := range musicModels() {
		here := true
		for _, hr := range m.repos {
			if _, err := os.Stat(filepath.Join(store.Dir, filepath.FromSlash(hr.repo), hr.rev, ".files.json")); err != nil {
				here = false
				break
			}
			var keep func(string) bool
			if len(hr.include) > 0 {
				inc := hr.include
				keep = func(p string) bool { return models.Matches(p, inc...) }
			}
			hm, err := store.FromHub(context.Background(), m.Name, hr.repo, hr.rev, keep) // from the cached list
			if err != nil || store.Missing(hm) > 0 {
				here = false
				break
			}
		}
		if here {
			m.Here, m.Where = true, store.Dir
		}
		add(m)
	}

	// speech (mlx-audio): here when the store has the repositories it needs first
	for _, m := range speechModels() {
		if reposHere(store, coreRepos(m)) {
			m.Here, m.Where = true, store.Dir
		}
		add(m)
	}

	// sound effects: here when the store has every file of every repository
	for _, m := range soundModels() {
		if soundReposHere(store, m.repos) {
			m.Here, m.Where = true, store.Dir
		}
		add(m)
	}

	order := map[Kind]int{}
	for i, k := range Kinds {
		order[k] = i
	}
	sort.SliceStable(c.Models, func(i, j int) bool { return order[c.Models[i].Kind] < order[c.Models[j].Kind] })
	return c
}

// ltxPack is an LTX-2.5 pack of ltx-2-mlx (dgrauet/ltx-2.5-mlx-*) at a pinned revision.
type ltxPack struct{ quant, repo, rev string }

var (
	ltxQ4 = ltxPack{"q4", "dgrauet/ltx-2.5-mlx-q4", "e9a20add8c5937fcab635fb8e7a52926487a5776"}
	ltxQ8 = ltxPack{"q8", "dgrauet/ltx-2.5-mlx-q8", "746ca9aacb697d2c739f544d68b584214dedcc75"}
)

// ltxModels are the LTX-2.5 video models: the distilled two-stage pipeline (8 + 3 steps, no CFG) and the HQ one, the
// open dev model (the closest local thing to Lightricks' hosted LTX-2.5 Pro): stage 1 at half size with the dev
// transformer, the res_2s sampler and real CFG, stage 2 at full size with the distilled LoRA fused in. The two of a
// quantisation share one folder of the store: the HQ model downloads only the dev transformer and the LoRA on top.
func ltxModels(store *models.Store) []*Model {
	var out []*Model
	for _, l := range []struct {
		id    string
		pack  ltxPack
		hq    bool
		about string
		size  int64
	}{
		{"ltx-2.5-q4", ltxQ4, false, "LTX-2.5 22B audio+video, int4 (Gemma 4 text encoder): distilled two-stage, fits 32 GB", 30 * gb},
		{"ltx-2.5-q8", ltxQ8, false, "LTX-2.5 22B audio+video, int8: better quality, wants 64 GB", 45 * gb},
		{"ltx-2.5-hq-q4", ltxQ4, true, ltxHQAbout("int4", "fits 32 GB at small sizes"), 47 * gb},
		{"ltx-2.5-hq-q8", ltxQ8, true, ltxHQAbout("int8", "wants 48 GB or more"), 65 * gb},
	} {
		name := "LTX-2.5 (" + l.pack.quant + ")"
		// the distilled pipeline loads neither the dev transformer nor the distilled LoRA
		ignore := []string{"transformer-dev.safetensors", "*distilled-lora*", ".gitattributes"}
		// here: the store has the verified weights the pipeline loads
		weights := []string{"transformer-distilled.safetensors.sha256", "text_encoder.safetensors.sha256"}
		d := Request{Width: 704, Height: 448, FPS: 24, Frames: 97}
		if l.hq {
			name = "LTX-2.5 HQ (" + l.pack.quant + ")"
			// all of the pack, the distilled transformer too: --low-ram (the GPU watchdog's retry) streams it for
			// stage 2 instead of fusing the LoRA
			ignore = []string{".gitattributes"}
			weights = []string{"transformer-dev.safetensors.sha256", "*distilled-lora*.safetensors.sha256",
				"transformer-distilled.safetensors.sha256", "text_encoder.safetensors.sha256"}
			// res_2s with CFG: 15 steps (upstream's HQ default) and CFG 3; a smaller size, it is much slower
			d = Request{Width: 640, Height: 384, FPS: 24, Frames: 97, Steps: 15, Guidance: 3}
		}
		m := &Model{ID: l.id, Name: name, Kind: Video, Backend: "ltx", About: l.about, Size: l.size,
			repo: l.pack.repo, rev: l.pack.rev, hq: l.hq, ignore: ignore, Defaults: d,
			Gated: true} // the LTX-2 Community License, accepted on Hugging Face
		if dir := filepath.Join(store.Dir, filepath.FromSlash(l.pack.repo), l.pack.rev); hasAll(dir, weights...) {
			m.Here, m.Where, m.Size = true, dir, dirSize(dir)
		}
		out = append(out, m)
	}
	return out
}

func ltxHQAbout(quant, mem string) string {
	return "LTX-2.5 HQ 22B audio+video, " + quant + ": the open dev model with real CFG (res_2s sampler, 15 steps) at " +
		"half size, then the distilled LoRA refines at full size — the closest local thing to Lightricks' hosted " +
		"LTX-2.5 Pro (API only). More detail and closer prompt following than the distilled model, but about 6× " +
		"slower (q8, 512×320, 49 frames: 5.7 min against 1 min distilled, peak ~30 GB); " + mem + ". Shares the folder of LTX-2.5 (" + "q" + strings.TrimPrefix(quant, "int") +
		"): adds the dev transformer and the distilled LoRA"
}

// qwenImages are the Qwen-Image models (pinned revisions; sizes: what mflux loads of each repository).
var qwenImages = []qwenImage{
	{"qwen-image-2512", "Qwen-Image 2512", "qwen-image", "Qwen/Qwen-Image-2512", "25468b98e3276ca6700de15c6628e51b7de54a26",
		"Qwen-Image 2512, 20B, Apache 2.0: text to image with true CFG; strong at text in pictures (also Chinese). " +
			"Slow (two passes per step) and soft compared with FLUX; edits need Qwen-Image-Edit",
		[]string{"generate"}, 20, 4, 54 * gb, "Apache-2.0", false},
	{"qwen-image-edit-2511", "Qwen-Image-Edit 2511", "qwen-image-edit", "Qwen/Qwen-Image-Edit-2511", "6f3ccc0b56e431dc6a0c2b2039706d7d26f22cb9",
		"Qwen-Image-Edit 2511, 20B, Apache 2.0: edits one to a few reference images following the prompt " +
			"(needs at least one; several can be combined) and keeps what the prompt leaves alone. Slow (two passes per step)",
		[]string{"edit"}, 30, 4, 54 * gb, "Apache-2.0", false},
	{"qwen-image-2.1", "Qwen-Image 2.1", "qwen-image-2.1", "Qwen/Qwen-Image-2.1", "d26bb61231c349cf6b7896fa83353113880e1ba3",
		"Qwen-Image 2.1, 7B transformer with a Qwen3-VL text encoder: text to image, 40 steps without guidance. " +
			"Qwen Research Licence: non-commercial only",
		[]string{"generate"}, 40, 1, 31 * gb, "Qwen Research License", true},
}

// qwenImage is a Qwen-Image model of the catalogue (run by mflux like FLUX).
type qwenImage struct {
	id, name, mflux, repo, rev, about string
	tasks                             []string // "generate" (text to image) or "edit" (needs reference images)
	steps                             int
	guidance                          float64 // true CFG scale (1: off)
	size                              int64
	licence                           string
	nonCommercial                     bool
}

// qwenFiles are the files of a Qwen-Image repository mflux loads: the weights and configs of the transformer, the
// VAE and the text encoder, and the tokenizer (2.1 keeps it in processor/).
func qwenFiles(mflux string) []string {
	tok := "tokenizer/"
	if mflux == "qwen-image-2.1" {
		tok = "processor/"
	}
	return []string{"transformer/", "vae/", "text_encoder/", tok, "model_index.json"}
}

func (q qwenImage) model(store *models.Store, hub string) *Model {
	m := &Model{ID: q.id, Name: q.name, Kind: Image, Backend: "mflux", About: q.about, Size: q.size, repo: q.repo, rev: q.rev,
		mflux: q.mflux, files: qwenFiles(q.mflux), Tasks: q.tasks, Licence: q.licence, NonCommercial: q.nonCommercial,
		Defaults: Request{Width: 1024, Height: 1024, Steps: q.steps, Guidance: q.guidance, Quantize: 8}}
	if d := filepath.Join(store.Dir, filepath.FromSlash(q.repo), q.rev); hasAll(d, "transformer/*.safetensors.sha256", "vae/*.safetensors.sha256", "text_encoder/*.safetensors.sha256") {
		m.Here, m.Where, m.Size = true, d, dirSize(d)
	} else if d := hfSnapshot(hub, q.repo, q.rev); d != "" && hasAll(d, "transformer/*.safetensors", "vae/*.safetensors", "text_encoder/*.safetensors") {
		m.Here, m.Where = true, "the Hugging Face cache (linked on first use)"
	}
	return m
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
