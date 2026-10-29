package engine

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/philipparndt/ai-connector/internal/models"
)

// Models as data (docs/models.md): a model of a runtime the worker has is a JSON manifest — the catalogue's are
// embedded (catalog/*.json), a machine's own are read from <config dir>/models.d when the worker starts.

//go:embed catalog/*.json
var catalogFiles embed.FS

// Manifest is one model's description.
type Manifest struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Kind          Kind     `json:"kind"`
	Backend       string   `json:"backend"`
	Order         int      `json:"order,omitempty"`
	About         string   `json:"about"`
	Licence       string   `json:"licence"`
	NonCommercial bool     `json:"nonCommercial,omitempty"`
	Gated         bool     `json:"gated,omitempty"`
	Size          ByteSize `json:"size"`
	// Memory: the GB the runtime takes at its peak, where the size misleads (the router's need; docs/models.md)
	Memory float64 `json:"memory,omitempty"`
	// Images: a chat model that takes images with the messages
	Images bool     `json:"images,omitempty"`
	Tasks  []string `json:"tasks,omitempty"`
	// Source: one repository, downloaded when the store has Here of it verified (or the Hugging Face cache has
	// HereCache); Sources: several (music, sound, speech), downloaded when the store has every file of each but the
	// optional ones
	Source *struct {
		Repo   string   `json:"repo"`
		Rev    string   `json:"rev"`
		Files  []string `json:"files,omitempty"`
		Ignore []string `json:"ignore,omitempty"`
	} `json:"source,omitempty"`
	Sources      []ManifestSource `json:"sources,omitempty"`
	Here         []string         `json:"here,omitempty"`
	HereCache    *[]string        `json:"hereCache,omitempty"`
	DeclaredSize bool             `json:"declaredSize,omitempty"`
	Defaults     Request          `json:"defaults"`
	// Params: what a model of a generic runtime takes besides the request's fields (checked before the queue)
	Params      map[string]ParamSpec `json:"params,omitempty"`
	RuntimeArgs struct {
		Mflux       string  `json:"mflux,omitempty"`
		Loader      string  `json:"loader,omitempty"`     // mflux: "module:Class" for text to image
		EditLoader  string  `json:"editLoader,omitempty"` // mflux: "module:Class" for editing with reference images
		HQ          bool    `json:"hq,omitempty"`
		Side        int     `json:"side,omitempty"`
		Runtime     string  `json:"runtime,omitempty"`
		WorkerModel string  `json:"workerModel,omitempty"`
		MaxSeconds  float64 `json:"maxSeconds,omitempty"`
		// Speech: a speech family mlx-audio knows but the catalogue does not (workerModel names it): speech.py runs it
		// on its general path
		Speech *struct {
			ModelType    string   `json:"modelType"`              // mlx-audio's model_type
			Voices       []string `json:"voices,omitempty"`       // its preset voices
			DefaultVoice string   `json:"defaultVoice,omitempty"` // "" the first
			Languages    []string `json:"languages,omitempty"`    // the languages it must be told (lang_code)
			Speaks       []string `json:"speaks,omitempty"`       // the languages it speaks when not told them
			Clone        bool     `json:"clone,omitempty"`        // speaks with a voice made from a sample (ref_audio)
			Instruct     bool     `json:"instruct,omitempty"`     // takes an instruction (instruct)
			Speed        bool     `json:"speed,omitempty"`        // honours speed itself
		} `json:"speech,omitempty"`
		// ComfyUI: the workflow in API format, where the request's fields go in it, the model's files by models folder
		Workflow json.RawMessage     `json:"workflow,omitempty"`
		Inputs   map[string]string   `json:"inputs,omitempty"`
		Folders  map[string][]string `json:"folders,omitempty"`
		LoRA     *struct {
			File    string  `json:"file"`
			Trigger string  `json:"trigger"`
			Scale   float64 `json:"scale"`
		} `json:"lora,omitempty"`
	} `json:"runtimeArgs"`
}

// ManifestSource is one repository of a model with several.
type ManifestSource struct {
	Key      string   `json:"key"`
	Repo     string   `json:"repo"`
	Rev      string   `json:"rev"`
	Include  []string `json:"include,omitempty"`
	Optional bool     `json:"optional,omitempty"` // fetched when first used: not needed for the model to be here
}

// ByteSize is a size in a manifest: bytes, or "5.5 GB" / "501 MB" (binary units).
type ByteSize int64

func (b *ByteSize) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*b = ByteSize(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("size: bytes or a string like \"5.5 GB\"")
	}
	num, unit, _ := strings.Cut(strings.TrimSpace(s), " ")
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return fmt.Errorf("size %q: %v", s, err)
	}
	switch unit {
	case "GB":
		*b = ByteSize(f * float64(gb))
	case "MB":
		*b = ByteSize(f * float64(1<<20))
	default:
		return fmt.Errorf("size %q: the unit is GB or MB", s)
	}
	return nil
}

// manifestBackend is what a manifest of a backend must say, and what the backend adds to its models.
type manifestBackend struct {
	kind  Kind
	tasks []string                 // the tasks a manifest may name
	check func(mf *Manifest) error // the runtime arguments it needs
	add   func(m *Model)           // what the backend derives from the model (tasks, audio capabilities, styles)
}

var manifestBackends = map[string]manifestBackend{
	"mflux": {kind: Image, tasks: []string{"generate", "edit", TaskUpscale},
		check: needArg("mflux", func(mf *Manifest) string { return mf.RuntimeArgs.Mflux })},
	"cutout": {kind: Image, tasks: []string{TaskCutout},
		check: func(mf *Manifest) error {
			if mf.RuntimeArgs.Side <= 0 {
				return fmt.Errorf("runtimeArgs.side (the side the model sees) is needed")
			}
			return nil
		}},
	"ltx":    {kind: Video},
	"avatar": {kind: Avatar, check: needArg("runtime", func(mf *Manifest) string { return mf.RuntimeArgs.Runtime })},
	"music": {kind: Music, tasks: []string{"cover", "repaint", "extend", "lego", "extract", "restyle"},
		check: func(mf *Manifest) error {
			if MusicRuntimes()[mf.RuntimeArgs.Runtime] == nil || mf.RuntimeArgs.WorkerModel == "" {
				return fmt.Errorf("runtimeArgs.runtime (one of the song runtimes) and runtimeArgs.workerModel are needed")
			}
			return nil
		},
		add: func(m *Model) { withStyles([]*Model{m}) }},
	"speech": {kind: Speech, check: func(mf *Manifest) error {
		wm, sp := mf.RuntimeArgs.WorkerModel, mf.RuntimeArgs.Speech
		switch {
		case wm == "":
			return fmt.Errorf("runtimeArgs.workerModel is needed")
		case speechFamilies[wm] == nil && (sp == nil || sp.ModelType == ""):
			return fmt.Errorf("%s is no family the catalogue knows: describe it in runtimeArgs.speech (modelType, voices, …)", wm)
		case sp != nil && speechFamilies[wm] != nil && speechFamilies[wm].modelType == "":
			return fmt.Errorf("%s is a family of the catalogue: no runtimeArgs.speech", wm)
		}
		return nil
	},
		add: func(m *Model) {
			if len(m.Tasks) == 0 {
				m.Tasks = speechTasks(m)
			}
			m.Audio = speechCaps(m)
		}},
	"sound": {kind: Sound, check: needArg("workerModel", func(mf *Manifest) string { return mf.RuntimeArgs.WorkerModel }),
		add: func(m *Model) { m.Audio = &AudioCaps{MaxSeconds: soundMaxSeconds} }},
}

func needArg(name string, get func(*Manifest) string) func(*Manifest) error {
	return func(mf *Manifest) error {
		if get(mf) == "" {
			return fmt.Errorf("runtimeArgs.%s is needed", name)
		}
		return nil
	}
}

var (
	manifestID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	pinnedRev  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ParseManifest reads one manifest and checks it.
func ParseManifest(data []byte) (*Manifest, error) {
	var mf Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&mf); err != nil {
		return nil, err
	}
	if !manifestID.MatchString(mf.ID) {
		return nil, fmt.Errorf("id %q: lower case letters, digits, '.', '_' and '-'", mf.ID)
	}
	if err := mf.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", mf.ID, err)
	}
	return &mf, nil
}

func (mf *Manifest) check() error {
	be, ok := manifestBackends[mf.Backend]
	generic := false
	if rt := runtimeManifest(mf.Backend); !ok && rt != nil && (rt.Generic || rt.Server != nil) {
		ok, generic = true, true
		be = manifestBackend{kind: mf.Kind, tasks: mf.Tasks}
		if !slices.Contains(rt.Kinds, mf.Kind) {
			return fmt.Errorf("runtime %s makes %v models, not %q", rt.Name, rt.Kinds, mf.Kind)
		}
		if rt.Server != nil && rt.Server.Protocol == "comfyui" {
			ra := mf.RuntimeArgs
			var wf map[string]any
			if len(ra.Workflow) == 0 || json.Unmarshal(ra.Workflow, &wf) != nil || len(ra.Inputs) == 0 || len(ra.Folders) == 0 {
				return fmt.Errorf("a ComfyUI model needs runtimeArgs.workflow (API format), inputs and folders")
			}
			for field, path := range ra.Inputs {
				node, _, _ := strings.Cut(path, ".")
				if _, ok := wf[node]; !ok {
					return fmt.Errorf("input %s: the workflow has no node %q", field, node)
				}
			}
		}
	}
	switch {
	case mf.Name == "" || mf.About == "":
		return fmt.Errorf("name and about are needed")
	case !ok:
		return fmt.Errorf("backend %q is neither one of %s nor a generic runtime", mf.Backend, strings.Join(backendNames(), ", "))
	case mf.Kind != be.kind:
		return fmt.Errorf("backend %s runs %s models, not %q", mf.Backend, be.kind, mf.Kind)
	case mf.Images && mf.Kind != Chat:
		return fmt.Errorf("images is for chat models (that take images with the messages)")
	case len(mf.Params) > 0 && !generic:
		return fmt.Errorf("params are for the models of a generic runtime")
	case mf.Licence == "":
		return fmt.Errorf("the licence is needed")
	case mf.Size <= 0:
		return fmt.Errorf("the size is needed")
	case mf.Source != nil && len(mf.Sources) > 0:
		return fmt.Errorf("source (one repository) or sources (several), not both")
	case mf.Source == nil && len(mf.Sources) == 0 && !generic:
		return fmt.Errorf("source (one repository) or sources (several) is needed")
	case mf.Source != nil && (mf.Source.Repo == "" || !pinnedRev.MatchString(mf.Source.Rev)):
		return fmt.Errorf("source.repo and source.rev (a commit, 40 hex digits) are needed")
	case mf.Source != nil && len(mf.Here) == 0:
		return fmt.Errorf("here (the files that make it downloaded) is needed")
	case len(mf.Sources) > 0 && (len(mf.Here) > 0 || mf.HereCache != nil):
		return fmt.Errorf("a model of several sources is here when each has all its files: no here")
	}
	keys := map[string]bool{}
	for _, s := range mf.Sources {
		if s.Key == "" || keys[s.Key] || s.Repo == "" || !pinnedRev.MatchString(s.Rev) {
			return fmt.Errorf("each of sources needs a key of its own, a repo and a rev (a commit, 40 hex digits)")
		}
		keys[s.Key] = true
	}
	for _, t := range mf.Tasks {
		if !slices.Contains(be.tasks, t) {
			return fmt.Errorf("task %q is none of %s's (%s)", t, mf.Backend, strings.Join(be.tasks, ", "))
		}
	}
	for n, p := range mf.Params {
		if err := p.check(); err != nil {
			return fmt.Errorf("param %s: %w", n, err)
		}
	}
	if be.check != nil {
		return be.check(mf)
	}
	return nil
}

func backendNames() []string {
	var out []string
	for b := range manifestBackends {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// model makes the catalogue's model of a manifest and finds out whether its files are here (store nil: not looked
// for).
func (mf *Manifest) model(store *models.Store, hub string) *Model {
	ra := mf.RuntimeArgs
	m := &Model{ID: mf.ID, Name: mf.Name, Kind: mf.Kind, Backend: mf.Backend, About: mf.About, Size: int64(mf.Size), Memory: mf.Memory,
		Gated: mf.Gated, Licence: mf.Licence, NonCommercial: mf.NonCommercial, Tasks: mf.Tasks, Defaults: mf.Defaults, Params: mf.Params, Images: mf.Images,
		mflux: ra.Mflux, loader: ra.Loader, editLoader: ra.EditLoader, hq: ra.HQ, side: ra.Side, runtime: ra.Runtime, workerModel: ra.WorkerModel, maxSeconds: ra.MaxSeconds}
	m.workflow, m.inputs, m.folders = ra.Workflow, ra.Inputs, ra.Folders
	if ra.LoRA != nil {
		m.lora = &musicLoRA{file: ra.LoRA.File, trigger: ra.LoRA.Trigger, scale: ra.LoRA.Scale}
	}
	if s := mf.Source; s != nil {
		m.repo, m.rev, m.files, m.ignore = s.Repo, s.Rev, s.Files, s.Ignore
	}
	for _, s := range mf.Sources {
		m.repos = append(m.repos, hubRepo{key: s.Key, repo: s.Repo, rev: s.Rev, include: s.Include})
	}
	if sp := ra.Speech; sp != nil && speechFamilies[ra.WorkerModel] == nil {
		speechFamilies[ra.WorkerModel] = manifestFamily(ra.WorkerModel, sp.ModelType, sp.Voices, sp.DefaultVoice, sp.Languages, sp.Speaks,
			sp.Clone, sp.Instruct, sp.Speed)
	}
	if add := manifestBackends[mf.Backend].add; add != nil {
		add(m)
	}
	if store != nil {
		mf.findHere(m, store, hub)
	}
	return m
}

func (mf *Manifest) findHere(m *Model, store *models.Store, hub string) {
	if mf.Source == nil && len(mf.Sources) == 0 { // a generic runtime's model that brings its weights along
		m.Here = true
		return
	}
	if len(mf.Sources) > 0 {
		var need []hubRepo
		for i, s := range mf.Sources {
			if !s.Optional {
				need = append(need, m.repos[i])
			}
		}
		if reposHere(store, need) {
			m.Here, m.Where = true, store.Dir
		}
		return
	}
	verified := make([]string, len(mf.Here))
	for i, p := range mf.Here {
		verified[i] = p + ".sha256"
	}
	cache := mf.Here
	if mf.HereCache != nil {
		cache = *mf.HereCache
	}
	if d := filepath.Join(store.Dir, filepath.FromSlash(m.repo), m.rev); hasAll(d, verified...) {
		m.Here, m.Where = true, d
		if !mf.DeclaredSize {
			m.Size = dirSize(d)
		}
	} else if d := hfSnapshot(hub, m.repo, m.rev); len(cache) > 0 && d != "" && hasAll(d, cache...) {
		m.Here, m.Where = true, "the Hugging Face cache (linked on first use)"
	}
}

// reposHere: the store has every file of every repository (from the cached file lists, no network).
func reposHere(store *models.Store, repos []hubRepo) bool {
	for _, hr := range repos {
		if _, err := os.Stat(filepath.Join(store.Dir, filepath.FromSlash(hr.repo), hr.rev, ".files.json")); err != nil {
			return false
		}
		hm, err := store.FromHub(context.Background(), hr.repo, hr.repo, hr.rev, hr.keep())
		if err != nil || store.Missing(hm) > 0 {
			return false
		}
	}
	return true
}

// readManifests reads every *.json of a directory, by name; a file that does not parse or pass the checks is left
// out with its error.
func readManifests(fsys fs.FS, dir string) ([]*Manifest, []error) {
	names, err := fs.Glob(fsys, filepath.ToSlash(filepath.Join(dir, "*.json")))
	if err != nil {
		return nil, []error{err}
	}
	sort.Strings(names)
	var out []*Manifest
	var errs []error
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err == nil {
			var mf *Manifest
			if mf, err = ParseManifest(b); err == nil {
				out = append(out, mf)
				continue
			}
		}
		errs = append(errs, fmt.Errorf("%s: %w", n, err))
	}
	return out, errs
}

// byOrder sorts manifests as the pickers list them: by kind (Kinds), then order, then id.
func byOrder(ms []*Manifest) {
	kind := map[Kind]int{}
	for i, k := range Kinds {
		kind[k] = i
	}
	sort.SliceStable(ms, func(i, j int) bool {
		a, b := ms[i], ms[j]
		switch {
		case a.Kind != b.Kind:
			return kind[a.Kind] < kind[b.Kind]
		case a.Order != b.Order:
			return a.Order < b.Order
		}
		return a.ID < b.ID
	})
}

var builtin = sync.OnceValue(func() []*Manifest {
	ms, errs := readManifests(catalogFiles, "catalog")
	if len(errs) > 0 {
		panic(fmt.Sprintf("the embedded catalogue: %v", errs))
	}
	byOrder(ms)
	return ms
})

// builtinManifests are the catalogue's manifests, in order. The tests check them, so an error here is a broken build.
func builtinManifests() []*Manifest { return builtin() }

// builtinModels are the catalogue's models of a backend, in order (store nil: none is here).
func builtinModels(backend string, store *models.Store, hub string) []*Model {
	var out []*Model
	for _, mf := range builtinManifests() {
		if mf.Backend == backend {
			out = append(out, mf.model(store, hub))
		}
	}
	return out
}

// builtinRepo is the repository key of the catalogue's model id (a tool the code fetches itself: the transcriber,
// the aligner).
func builtinRepo(id, key string) hubRepo {
	for _, mf := range builtinManifests() {
		if mf.ID != id {
			continue
		}
		for _, s := range mf.Sources {
			if s.Key == key {
				return hubRepo{key: s.Key, repo: s.Repo, rev: s.Rev, include: s.Include}
			}
		}
	}
	panic("the catalogue has no " + id + " with a source " + key)
}

// AddLocal adds the models of the manifests in dir (<config dir>/models.d) the catalogue does not have yet. It
// returns what it left out and why; a missing dir is no error.
func (c *Catalog) AddLocal(dir string, store *models.Store) []error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	ms, errs := readManifests(os.DirFS(dir), ".")
	byOrder(ms)
	c.mu.Lock()
	defer c.mu.Unlock()
	have := map[string]bool{}
	for _, m := range c.Models {
		have[m.ID] = true
	}
	for _, mf := range ms {
		if have[mf.ID] {
			errs = append(errs, fmt.Errorf("%s.json: the catalogue has %s already; left out", mf.ID, mf.ID))
			continue
		}
		have[mf.ID] = true
		c.Models = append(c.Models, mf.model(store, c.HFHub))
	}
	sortByKind(c.Models)
	return errs
}

// sortByKind orders models by kind (Kinds), keeping the order within a kind.
func sortByKind(ms []*Model) {
	order := map[Kind]int{}
	for i, k := range Kinds {
		order[k] = i
	}
	sort.SliceStable(ms, func(i, j int) bool { return order[ms[i].Kind] < order[ms[j].Kind] })
}
