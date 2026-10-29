package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/models"
)

// manifestModels are the catalogue's manifest models whose id starts with prefix, in order.
func manifestModels(store *models.Store, hub, prefix string) []*Model {
	var out []*Model
	for _, mf := range builtinManifests() {
		if strings.HasPrefix(mf.ID, prefix) {
			out = append(out, mf.model(store, hub))
		}
	}
	return out
}

// manifestModel is the catalogue's manifest model id.
func manifestModel(t *testing.T, store *models.Store, hub, id string) *Model {
	t.Helper()
	for _, m := range manifestModels(store, hub, id) {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("no manifest %s in the catalogue", id)
	return nil
}

func TestBuiltinManifests(t *testing.T) {
	ms := builtinManifests()
	if len(ms) == 0 {
		t.Fatal("no manifests embedded")
	}
	seen := map[string]bool{}
	for _, mf := range ms {
		if seen[mf.ID] {
			t.Errorf("%s twice", mf.ID)
		}
		seen[mf.ID] = true
	}
}

const zImageManifest = `{
  "id": "z-image-test", "name": "Z", "kind": "image", "backend": "mflux", "about": "a test",
  "licence": "Apache-2.0", "size": "5.5 GB",
  "source": {"repo": "a/b", "rev": "b3a8f31115a11f2f9e2fa0bfbc8d78dcc3e6568b", "files": ["vae/"]},
  "here": ["vae/*.safetensors"], "defaults": {"steps": 9}, "runtimeArgs": {"mflux": "z-image-turbo"}
}`

func TestParseManifest(t *testing.T) {
	mf, err := ParseManifest([]byte(zImageManifest))
	if err != nil {
		t.Fatal(err)
	}
	if mf.Size != 5905580032 || mf.Defaults.Steps != 9 || mf.RuntimeArgs.Mflux != "z-image-turbo" {
		t.Errorf("%+v", mf)
	}
	for _, c := range []struct{ from, to, want string }{
		{`"id": "z-image-test"`, `"id": "Z Image"`, "id"},
		{`"rev": "b3a8f31115a11f2f9e2fa0bfbc8d78dcc3e6568b"`, `"rev": "main"`, "40 hex"},
		{`"backend": "mflux"`, `"backend": "comfyui"`, "backend"},
		{`"kind": "image"`, `"kind": "video"`, "runs image models"},
		{`"licence": "Apache-2.0", `, ``, "licence"},
		{`"size": "5.5 GB"`, `"size": "5.5 GiB"`, "unit"},
		{`"here": ["vae/*.safetensors"], `, ``, "here"},
		{`"runtimeArgs": {"mflux": "z-image-turbo"}`, `"runtimeArgs": {}`, "runtimeArgs.mflux"},
		{`"defaults": {"steps": 9}`, `"defaults": {"stepz": 9}`, "unknown field"},
		{`"about": "a test",`, `"about": "a test", "tasks": ["paint"],`, "task"},
	} {
		_, err := ParseManifest([]byte(strings.Replace(zImageManifest, c.from, c.to, 1)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s → %s: %v, want an error about %q", c.from, c.to, err, c.want)
		}
	}
	for s, want := range map[string]int64{`7284343622`: 7284343622, `"16 GB"`: 16 << 30, `"501 MB"`: 501 << 20} {
		var b ByteSize
		if err := b.UnmarshalJSON([]byte(s)); err != nil || int64(b) != want {
			t.Errorf("%s: %d %v", s, b, err)
		}
	}
}

// A machine's own manifests (models.d): added after the catalogue's models of their kind; one whose id the catalogue
// has, and one that does not parse, are left out with the reason.
func TestAddLocal(t *testing.T) {
	c := goldenCatalogue(t)
	n := len(c.List())
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "z-image-test.json"), []byte(zImageManifest), 0o644)
	os.WriteFile(filepath.Join(dir, "dup.json"), []byte(strings.Replace(zImageManifest, `"z-image-test"`, `"flux2-klein-4b"`, 1)), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"id": `), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(`not a manifest`), 0o644)
	errs := c.AddLocal(dir, &models.Store{Dir: t.TempDir()})
	if len(errs) != 2 || !strings.Contains(errs[0].Error(), "broken.json") || !strings.Contains(errs[1].Error(), "has flux2-klein-4b already") {
		t.Errorf("errors %v", errs)
	}
	ms := c.List()
	if len(ms) != n+1 {
		t.Fatalf("%d models, want %d", len(ms), n+1)
	}
	at := -1
	for i, m := range ms {
		if m.ID == "z-image-test" {
			at = i
		}
	}
	if at < 1 || ms[at-1].Kind != Image || (at+1 < len(ms) && ms[at+1].Kind == Image) {
		t.Errorf("z-image-test at %d: not the last image model", at)
	}
	if m := c.Find("z-image-test"); m == nil || m.Here || m.mflux != "z-image-turbo" || m.repo != "a/b" {
		t.Errorf("%+v", m)
	}
	if errs := c.AddLocal(filepath.Join(dir, "missing"), &models.Store{Dir: t.TempDir()}); errs != nil {
		t.Errorf("a missing models.d: %v", errs)
	}
}

// A speech family the catalogue does not know, described by the model's manifest: its tasks and voices come from the
// description; a model naming an unknown family without one is refused.
func TestManifestSpeechFamily(t *testing.T) {
	kitten := `{"id": "kitten-test", "name": "Kitten", "kind": "speech", "backend": "speech", "about": "a test",
	  "licence": "Apache-2.0", "size": "130 MB",
	  "sources": [{"key": "model", "repo": "mlx-community/kitten-tts-micro-0.8", "rev": "fd4e8e8677eafd21c5e92433f68532fcf156bd9c"}],
	  "defaults": {"speed": 1},
	  "runtimeArgs": {"workerModel": "kitten-test", "speech": {"modelType": "kitten_tts", "voices": ["Bella", "Leo"],
	    "defaultVoice": "Leo", "speaks": ["en"], "speed": true}}}`
	mf, err := ParseManifest([]byte(kitten))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { delete(speechFamilies, "kitten-test") })
	m := mf.model(nil, "")
	if strings.Join(m.Tasks, ",") != "presets,speed" || m.Audio == nil || len(m.Audio.Voices) != 2 || m.Audio.Instruct {
		t.Errorf("tasks %v audio %+v", m.Tasks, m.Audio)
	}
	if f := speechFamilies["kitten-test"]; f == nil || f.modelType != "kitten_tts" || f.preset("de") != "Leo" {
		t.Errorf("family %+v", f)
	}
	bad := strings.Replace(kitten, `"workerModel": "kitten-test", "speech": {"modelType": "kitten_tts", "voices": ["Bella", "Leo"],
	    "defaultVoice": "Leo", "speaks": ["en"], "speed": true}`, `"workerModel": "nobody"`, 1)
	if _, err := ParseManifest([]byte(strings.Replace(bad, `"kitten-test"`, `"nobody-test"`, 1))); err == nil || !strings.Contains(err.Error(), "runtimeArgs.speech") {
		t.Errorf("unknown family: %v", err)
	}
}
