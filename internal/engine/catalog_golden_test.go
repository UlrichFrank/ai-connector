package engine

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/llm"
	"github.com/philipparndt/ai-connector/internal/models"
)

// The catalogue as a user meets it, pinned: what /api/models says of each model, where its files come from (the
// store's folders: a changed repository, revision or file list downloads again), what its runtime gets, and what the
// setup derives from it. Moving the catalogue out of Go must leave testdata/catalog.golden.json as it is.
// MLXLAB_UPDATE_GOLDEN=1 writes it anew (only when the catalogue is meant to change).

const goldenCatalog = "testdata/catalog.golden.json"

type goldenFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

type goldenRepo struct {
	Key     string       `json:"key,omitempty"`
	Repo    string       `json:"repo"`
	Rev     string       `json:"rev"`
	Subdir  string       `json:"subdir,omitempty"`
	Include []string     `json:"include,omitempty"` // file patterns fetched ("" all)
	Ignore  []string     `json:"ignore,omitempty"`  // file patterns not fetched
	Files   []goldenFile `json:"files,omitempty"`   // pinned files (chat, embedder, classifier)
}

type goldenEnv struct {
	Name     string   `json:"name"`
	Python   string   `json:"python"`
	Packages []string `json:"packages,omitempty"`
	Index    string   `json:"index,omitempty"`
	IndexPkg []string `json:"indexPackages,omitempty"`
	Build    bool     `json:"build,omitempty"`
}

type goldenModel struct {
	API json.RawMessage `json:"api"` // the model as /api/models lists it

	Sources []goldenRepo `json:"sources,omitempty"`
	Runtime struct {
		Mflux       string               `json:"mflux,omitempty"`
		Loader      string               `json:"loader,omitempty"`
		EditLoader  string               `json:"editLoader,omitempty"`
		HQ          bool                 `json:"hq,omitempty"`
		Side        int                  `json:"side,omitempty"`
		Runtime     string               `json:"runtime,omitempty"`
		WorkerModel string               `json:"workerModel,omitempty"`
		MaxSeconds  float64              `json:"maxSeconds,omitempty"`
		LoRAFile    string               `json:"loraFile,omitempty"`
		LoRATrigger string               `json:"loraTrigger,omitempty"`
		LoRAScale   float64              `json:"loraScale,omitempty"`
		Env         *goldenEnv           `json:"env,omitempty"`  // on Apple silicon
		Envs        map[string]goldenEnv `json:"envs,omitempty"` // on the other platforms
	} `json:"runtime"`

	RepoURL       string  `json:"repoURL,omitempty"`
	Downloadable  bool    `json:"downloadable"`
	Licence       string  `json:"licence,omitempty"`
	NonCommercial bool    `json:"nonCommercial,omitempty"`
	FitsGB        []int64 `json:"fitsGB"` // the memory sizes FitsMemory says yes to
	Role          string  `json:"role"`
}

type goldenCatalogFile struct {
	Models []goldenModel `json:"models"`
	// Recommended: per role (SetupRole), the model proposed first on a machine with that much memory
	Recommended map[string]map[string]string `json:"recommended"`
}

var goldenMems = []int64{8, 16, 24, 32, 48, 64, 96, 128, 192, 512}

func pinnedRepo(m models.Model) goldenRepo {
	r := goldenRepo{Repo: m.Repo, Rev: m.Rev, Subdir: m.Subdir}
	for _, f := range m.Files {
		r.Files = append(r.Files, goldenFile{Path: f.Path, Size: f.Size, SHA256: f.SHA256})
	}
	return r
}

func goldenOf(t *testing.T, m *Model) goldenModel {
	var g goldenModel
	api, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	g.API = api
	switch {
	case m.chat != nil:
		g.Sources = append(g.Sources, pinnedRepo(m.chat.Model))
	case m.pinned != nil:
		g.Sources = append(g.Sources, pinnedRepo(*m.pinned))
	}
	if m.repo != "" {
		g.Sources = append(g.Sources, goldenRepo{Repo: m.repo, Rev: m.rev, Include: m.files, Ignore: m.ignore})
	}
	for _, hr := range m.repos {
		g.Sources = append(g.Sources, goldenRepo{Key: hr.key, Repo: hr.repo, Rev: hr.rev, Include: hr.include})
	}
	rt := &g.Runtime
	rt.Loader, rt.EditLoader = m.loader, m.editLoader
	rt.Mflux, rt.HQ, rt.Side, rt.Runtime, rt.WorkerModel, rt.MaxSeconds = m.mflux, m.hq, m.side, m.runtime, m.workerModel, m.maxSeconds
	if m.lora != nil {
		rt.LoRAFile, rt.LoRATrigger, rt.LoRAScale = m.lora.file, m.lora.trigger, m.lora.scale
	}
	if n := runtimeName(m); n != "" {
		build := runtimeManifest(n).Build != ""
		for _, p := range Platforms {
			env, ok := envFor(n, p)
			if !ok {
				continue
			}
			g := goldenEnv{Name: env.Name, Python: env.Python, Packages: env.Packages, Index: env.Index, IndexPkg: env.IndexPackages, Build: build}
			if p == "darwin-arm64" {
				rt.Env = &g
			} else {
				if rt.Envs == nil {
					rt.Envs = map[string]goldenEnv{}
				}
				rt.Envs[p] = g
			}
		}
	}
	g.RepoURL = RepoURL(m)
	g.Downloadable = Downloadable(m)
	g.Licence, g.NonCommercial = licenceOf(m)
	g.FitsGB = []int64{}
	for _, mem := range goldenMems {
		if FitsMemory(m, mem*gb) {
			g.FitsGB = append(g.FitsGB, mem)
		}
	}
	g.Role = SetupRole(m.Kind, m.Tasks)
	return g
}

// goldenCatalogue is the catalogue of a machine with nothing on it: an empty store, no LM Studio, no Hugging Face
// cache; every backend's models (no Keep), so the file is the same on every platform.
func goldenCatalogue(t *testing.T) *Catalog {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LMSTUDIO_MODELS_DIR", "")
	t.Setenv("HF_HOME", "")
	t.Setenv("HF_HUB_CACHE", filepath.Join(home, "hf"))
	store := &models.Store{Dir: filepath.Join(home, "models")}
	finder := &llm.Finder{Store: store, Mem: 64 * gb, Verifier: &llm.Verifier{Path: filepath.Join(store.Dir, "verified.json")}}
	return NewCatalog(store, finder)
}

func TestCatalogGolden(t *testing.T) {
	c := goldenCatalogue(t)
	var out goldenCatalogFile
	roles := map[string]bool{}
	for _, m := range c.List() {
		if m.Here {
			t.Fatalf("%s: here on an empty machine (%s)", m.ID, m.Where)
		}
		g := goldenOf(t, m)
		out.Models = append(out.Models, g)
		roles[g.Role] = true
	}
	out.Recommended = map[string]map[string]string{}
	for role := range roles {
		per := map[string]string{}
		for _, mem := range goldenMems {
			if id := recommendedOf(role, mem*gb); id != "" {
				per[jsonKey(mem)] = id
			}
		}
		out.Recommended[role] = per
	}
	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("MLXLAB_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(goldenCatalog, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenCatalog)
	if err != nil {
		t.Fatalf("%v (MLXLAB_UPDATE_GOLDEN=1 writes it)", err)
	}
	if !bytes.Equal(got, want) {
		diffGolden(t, want, got)
	}
}

// flatJSON is a JSON value as path → value ("api.defaults.steps" → "9").
func flatJSON(b []byte) map[string]string {
	var v any
	json.Unmarshal(b, &v)
	out := map[string]string{}
	var walk func(p string, v any)
	walk = func(p string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(strings.TrimPrefix(p+"."+k, "."), e)
			}
		case []any:
			for i, e := range x {
				walk(p+"["+strconv.Itoa(i)+"]", e)
			}
		default:
			s, _ := json.Marshal(x)
			out[p] = string(s)
		}
	}
	walk("", v)
	return out
}

func unionKeys(a, b map[string]string) []string {
	var ks []string
	for k := range a {
		ks = append(ks, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	return ks
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func jsonKey(mem int64) string {
	b, _ := json.Marshal(mem)
	return string(b) + "GB"
}

// diffGolden names the models that changed (or came or went), so a failure says what, not only that.
func diffGolden(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g goldenCatalogFile
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("the golden file does not parse: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	idOf := func(m goldenModel) string {
		var a struct {
			ID string `json:"id"`
		}
		json.Unmarshal(m.API, &a)
		return a.ID
	}
	wm := map[string][]byte{}
	var order []string
	for _, m := range w.Models {
		b, _ := json.Marshal(m)
		wm[idOf(m)] = b
		order = append(order, idOf(m))
	}
	gm := map[string]bool{}
	for i, m := range g.Models {
		id := idOf(m)
		gm[id] = true
		b, _ := json.Marshal(m)
		switch old, ok := wm[id]; {
		case !ok:
			t.Errorf("new model %s", id)
		case !bytes.Equal(old, b):
			was, now := flatJSON(old), flatJSON(b)
			for _, k := range unionKeys(was, now) {
				if was[k] != now[k] {
					t.Errorf("%s: %s was %s, now %s", id, k, orNone(was[k]), orNone(now[k]))
				}
			}
		case i >= len(order) || order[i] != id:
			t.Errorf("%s moved in the list", id)
		}
	}
	for _, id := range order {
		if !gm[id] {
			t.Errorf("model %s is gone", id)
		}
	}
	wr, _ := json.Marshal(w.Recommended)
	gr, _ := json.Marshal(g.Recommended)
	if !bytes.Equal(wr, gr) {
		t.Errorf("the recommendations changed:\n  was %s\n  now %s", wr, gr)
	}
	if !t.Failed() {
		t.Errorf("%s differs (formatting?); MLXLAB_UPDATE_GOLDEN=1 writes it", goldenCatalog)
	}
}
