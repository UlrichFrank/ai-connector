package hub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/web"
)

type cutoutMac struct {
	cut, webp *fakeGen
}

// startCutoutMac connects a worker with FLUX.2 klein and the cutout models: BiRefNet (its fake answers with a PNG),
// BiRefNet HR matting (a WebP: a cutout too large as PNG), BiRefNet lite not on disk.
func startCutoutMac(t *testing.T, base, token string) *cutoutMac {
	t.Helper()
	cut := []string{engine.TaskCutout}
	cat := &engine.Catalog{Models: []*engine.Model{
		{ID: "flux2-klein-4b", Name: "FLUX.2 [klein] 4B", Kind: engine.Image, Backend: "mflux", Size: gib, Here: true,
			Defaults: engine.Request{Width: 1024, Height: 1024, Steps: 4}},
		{ID: "birefnet", Name: "BiRefNet", Kind: engine.Image, Backend: "cutout", Size: gib, Here: true, Tasks: cut, Licence: "MIT"},
		{ID: "birefnet-hr-matting", Name: "BiRefNet HR matting", Kind: engine.Image, Backend: "cutout-webp", Size: gib, Here: true, Tasks: cut},
		{ID: "birefnet-lite", Name: "BiRefNet lite", Kind: engine.Image, Backend: "cutout", Size: gib, Tasks: cut},
	}}
	m := &cutoutMac{cut: &fakeGen{name: "cutout", ext: ".png", size: 3 << 20}, webp: &fakeGen{name: "cutout-webp", ext: ".webp", size: 2 << 20}}
	dir := t.TempDir()
	mgr := jobs.New(cat, []engine.Backend{&fakeGen{name: "mflux", ext: ".png", size: 1000}, m.cut, m.webp}, dir)
	api := (&web.Server{Jobs: mgr, Prompts: prompts.Open(dir), Version: "test"}).Handler()
	stop, _ := connectWorkerWith(t, base, token, withRAM(api, 64))
	t.Cleanup(func() {
		stop()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !mgr.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	return m
}

// cutoutFile is a generation's file as sent: its pieces put together, and their mime.
func cutoutFile(t *testing.T, evs []vbGenEv) ([]byte, string) {
	t.Helper()
	var file bytes.Buffer
	mime := ""
	for _, ev := range evs {
		if ev.Type == "generateFile" {
			data, _ := base64.StdEncoding.DecodeString(ev.M["data"].(string))
			file.Write(data)
			mime, _ = ev.M["mime"].(string)
		}
	}
	return file.Bytes(), mime
}

// The cutout models: advertised with their own kind (id, name, kind, worker, max, nonCommercial); one image, the
// result of its size, the image to the worker without a prompt, seed or steps; the PNG as it is — also when it is
// larger than vboard takes (never a JPEG: it would lose the alpha); a WebP passes as image/webp; errors.
func TestVboardCutout(t *testing.T) {
	fastGenerate(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	mac := startCutoutMac(t, base, wtok)
	v := newFakeVboard(t, stageGenerators)
	addConnector(t, base, device, `{"name":"Studio AI","url":"`+v.srv.URL+`","token":"vbp_secret",
		"generators":["flux2-klein-4b","birefnet","birefnet-hr-matting","birefnet-lite"]}`)
	b := v.next(t)
	want := []vbGenerator{wantFlux, {ID: "birefnet", Name: "BiRefNet", Kind: "cutout", Worker: "Mac", Max: 4096},
		{ID: "birefnet-hr-matting", Name: "BiRefNet HR matting", Kind: "cutout", Worker: "Mac", Max: 4096}}
	if got := b.helloGenerators(); !reflect.DeepEqual(got, want) {
		t.Fatalf("hello %s", b.raw) // lite is not on disk
	}
	var h struct{ Generators []map[string]any }
	json.Unmarshal(b.raw, &h)
	if g := h.Generators[1]; len(g) != 6 || g["nonCommercial"] != false {
		t.Fatalf("a cutout model is advertised with id, name, kind, worker, max, nonCommercial: %v", g)
	}

	cut := pngOf(t, 640, 360, true)
	mac.cut.mu.Lock()
	mac.cut.data = cut
	mac.cut.mu.Unlock()
	src := pngOf(t, 1000, 600, false)
	for _, fields := range []string{
		`"kind":"cutout"`,
		`"kind":"cutout","prompt":"ignored","seed":7,"steps":9,"width":64,"height":64`,
		`"generator":"birefnet","kind":"image"`,
	} {
		_, ch := b.generate(fields + "," + imagesField(src))
		first := genUntilState(t, ch, "")
		if first.M["width"] != 1000.0 || first.M["height"] != 600.0 || first.M["worker"] != "Mac" {
			t.Fatalf("%s: first status %+v", fields, first)
		}
		evs := genUntil(t, ch)
		done := evs[len(evs)-1]
		if done.Type != "generateDone" || done.M["width"] != 1000.0 || done.M["height"] != 600.0 || done.M["generator"] != "birefnet" {
			t.Fatalf("%s: done %+v", fields, done)
		}
		if file, mime := cutoutFile(t, evs); mime != "image/png" || !bytes.Equal(file, cut) {
			t.Fatalf("%s: %s of %d bytes", fields, mime, len(file))
		}
		r, images := mac.cut.last()
		if r.Model != "birefnet" || r.Prompt != "" || r.Seed != 0 || r.Steps != 0 || r.Width != 0 || r.Client != "vboard: Studio AI" ||
			len(images) != 1 || !bytes.Equal(images[0], src) {
			t.Fatalf("%s: job %+v (%d images)", fields, r, len(images))
		}
	}

	// a cutout PNG larger than vboard takes is not made a JPEG: an error (the worker writes a WebP that large)
	old := genMaxImageFile
	t.Cleanup(func() { genMaxImageFile = old })
	genMaxImageFile = 200 << 10
	if msg := genFailed(t, b, `"kind":"cutout",`+imagesField(src)); !strings.Contains(msg, "more than vboard takes") {
		t.Fatalf("%q", msg)
	}
	genMaxImageFile = old

	// a WebP goes as image/webp
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8L"), genBytes(5000)...)
	mac.webp.mu.Lock()
	mac.webp.data = webp
	mac.webp.mu.Unlock()
	_, ch := b.generate(`"generator":"birefnet-hr-matting","kind":"cutout",` + imagesField(src))
	evs := genUntil(t, ch)
	if file, mime := cutoutFile(t, evs); evs[len(evs)-1].Type != "generateDone" || mime != "image/webp" || !bytes.Equal(file, webp) {
		t.Fatalf("webp: %s %+v", mime, evs[len(evs)-1])
	}

	// errors
	for fields, want := range map[string]string{
		`"kind":"cutout"`:                                                        "send exactly one (not 0)",
		`"kind":"cutout",` + imagesField(src, src):                               "send exactly one (not 2)",
		`"kind":"cutout",` + imagesField(pngOf(t, 4097, 8, false)):               "larger than BiRefNet cuts out",
		`"kind":"cutout",` + imagesField([]byte("not a picture")):                "size cannot be read",
		`"kind":"cutout","images":[{"id":"x","mime":"image/gif","data":"R0lG"}]`: "not taken",
		`"generator":"birefnet-lite","kind":"cutout",` + imagesField(src):        `"birefnet-lite" is not available now`,
	} {
		if msg := genError(t, b, fields); !strings.Contains(msg, want) {
			t.Fatalf("%.80s: %q", fields, msg)
		}
	}
	// a text prompt to a cutout model needs the image all the same
	if msg := genError(t, b, `"generator":"birefnet","prompt":"a cat"`); !strings.Contains(msg, "exactly one") {
		t.Fatalf("text to a cutout model: %q", msg)
	}
}

// Without a cutout model: kind cutout is refused.
func TestVboardCutoutNone(t *testing.T) {
	fastGenerate(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	startGenMac(t, base, wtok)
	v := newFakeVboard(t, stageGenerators)
	addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret","generators":["*"]}`)
	b := v.next(t)
	for _, g := range b.helloGenerators() {
		if g.Kind == "cutout" {
			t.Fatalf("hello %s", b.raw)
		}
	}
	if msg := genError(t, b, `"kind":"cutout",`+imagesField(pngOf(t, 10, 10, false))); !strings.Contains(msg, "no cutout generator is offered") {
		t.Fatalf("%q", msg)
	}
}

// A cutout model's setup role is image/cutout: it is recommended beside FLUX, not against it.
func TestSetupRoleCutout(t *testing.T) {
	if r := setupRole("image", []string{"cutout"}); r != "image/cutout" {
		t.Fatal(r)
	}
	if r := engine.SetupRole(engine.Image, []string{engine.TaskCutout}); r != "image/cutout" {
		t.Fatal(r)
	}
}
