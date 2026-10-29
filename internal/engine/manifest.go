package engine

import (
	"bytes"
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
	Tasks         []string `json:"tasks,omitempty"`
	Source        struct {
		Repo   string   `json:"repo"`
		Rev    string   `json:"rev"`
		Files  []string `json:"files,omitempty"`
		Ignore []string `json:"ignore,omitempty"`
	} `json:"source"`
	Here         []string `json:"here"`
	SharedFolder bool     `json:"sharedFolder,omitempty"`
	Defaults     Request  `json:"defaults"`
	RuntimeArgs  struct {
		Mflux string `json:"mflux,omitempty"`
	} `json:"runtimeArgs"`
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

var (
	manifestID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	pinnedRev  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	imageTasks = []string{"generate", "edit", TaskUpscale, TaskCutout}
)

// manifestBackends are the backends a manifest may name (the families moved out of Go so far).
var manifestBackends = map[string]Kind{"mflux": Image}

// ParseManifest reads one manifest and checks it.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	switch kind, ok := manifestBackends[m.Backend]; {
	case !manifestID.MatchString(m.ID):
		return nil, fmt.Errorf("id %q: lower case letters, digits, '.', '_' and '-'", m.ID)
	case m.Name == "" || m.About == "":
		return nil, fmt.Errorf("%s: name and about are needed", m.ID)
	case !ok:
		return nil, fmt.Errorf("%s: backend %q is not one a manifest can name (%s)", m.ID, m.Backend, strings.Join(backendNames(), ", "))
	case m.Kind != kind:
		return nil, fmt.Errorf("%s: backend %s runs %s models, not %q", m.ID, m.Backend, kind, m.Kind)
	case m.Licence == "":
		return nil, fmt.Errorf("%s: the licence is needed", m.ID)
	case m.Size <= 0:
		return nil, fmt.Errorf("%s: the size is needed", m.ID)
	case m.Source.Repo == "" || !pinnedRev.MatchString(m.Source.Rev):
		return nil, fmt.Errorf("%s: source.repo and source.rev (a commit, 40 hex digits) are needed", m.ID)
	case len(m.Here) == 0:
		return nil, fmt.Errorf("%s: here (the files that make it downloaded) is needed", m.ID)
	case m.Backend == "mflux" && m.RuntimeArgs.Mflux == "":
		return nil, fmt.Errorf("%s: runtimeArgs.mflux (mflux's name for the model) is needed", m.ID)
	}
	for _, t := range m.Tasks {
		if !slices.Contains(imageTasks, t) {
			return nil, fmt.Errorf("%s: task %q is none of %s", m.ID, t, strings.Join(imageTasks, ", "))
		}
	}
	return &m, nil
}

func backendNames() []string {
	var out []string
	for b := range manifestBackends {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// model makes the catalogue's model of a manifest, here when the store has the files of Here (verified) or the
// Hugging Face cache has them.
func (mf *Manifest) model(store *models.Store, hub string) *Model {
	m := &Model{ID: mf.ID, Name: mf.Name, Kind: mf.Kind, Backend: mf.Backend, About: mf.About, Size: int64(mf.Size),
		Gated: mf.Gated, Licence: mf.Licence, NonCommercial: mf.NonCommercial, Tasks: mf.Tasks, Defaults: mf.Defaults,
		repo: mf.Source.Repo, rev: mf.Source.Rev, files: mf.Source.Files, ignore: mf.Source.Ignore, mflux: mf.RuntimeArgs.Mflux}
	verified := make([]string, len(mf.Here))
	for i, p := range mf.Here {
		verified[i] = p + ".sha256"
	}
	if d := filepath.Join(store.Dir, filepath.FromSlash(mf.Source.Repo), mf.Source.Rev); hasAll(d, verified...) {
		m.Here, m.Where = true, d
		if !mf.SharedFolder {
			m.Size = dirSize(d)
		}
	} else if d := hfSnapshot(hub, mf.Source.Repo, mf.Source.Rev); d != "" && hasAll(d, mf.Here...) {
		m.Here, m.Where = true, "the Hugging Face cache (linked on first use)"
	}
	return m
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

// byOrder sorts manifests as the pickers list them: by order, then by id.
func byOrder(ms []*Manifest) {
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].Order != ms[j].Order {
			return ms[i].Order < ms[j].Order
		}
		return ms[i].ID < ms[j].ID
	})
}

// builtinManifests are the catalogue's manifests, in order. They are checked by the tests, so an error here is a
// broken build.
func builtinManifests() []*Manifest {
	ms, errs := readManifests(catalogFiles, "catalog")
	if len(errs) > 0 {
		panic(fmt.Sprintf("the embedded catalogue: %v", errs))
	}
	byOrder(ms)
	return ms
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
