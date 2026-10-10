package hub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/engine"
)

// pngOf is a PNG of w×h: noise when noisy (it does not compress), else one colour.
func pngOf(t *testing.T, w, h int, noisy bool) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(1, 2))
	for i := range im.Pix {
		if noisy {
			im.Pix[i] = byte(r.Uint32())
		} else {
			im.Pix[i] = 0xff
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func imagesField(ims ...[]byte) string {
	var parts []string
	for i, b := range ims {
		parts = append(parts, `{"id":"image-`+string(rune('1'+i))+`","mime":"image/png","data":"`+base64.StdEncoding.EncodeToString(b)+`"}`)
	}
	return `"images":[` + strings.Join(parts, ",") + `]`
}

// The upscaler: advertised with its own kind; one image and a scale (the nearest it takes below), the result's size
// at once (cut to 4096, the aspect ratio kept), the image to the worker without a prompt; errors.
func TestVboardUpscale(t *testing.T) {
	fastGenerate(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	mac := startGenMac(t, base, wtok)
	v := newFakeVboard(t, stageGenerators)
	addConnector(t, base, device, `{"name":"Studio AI","url":"`+v.srv.URL+`","token":"vbp_secret",
		"generators":["flux2-klein-4b","seedvr2-3b","seedvr2-7b"]}`)
	b := v.next(t)
	want := vbGenerator{ID: "seedvr2-3b", Name: "SeedVR2 3B", Kind: "upscale", Worker: "Mac", Scales: [2]int{2, 4}, Max: 4096}
	if got := b.helloGenerators(); len(got) != 2 || got[0] != wantFlux || !reflect.DeepEqual(got[1], want) {
		t.Fatalf("hello %s", b.raw) // the 7B is not on disk
	}
	var h struct{ Generators []map[string]any }
	json.Unmarshal(b.raw, &h)
	if g := h.Generators[1]; len(g) != 7 || g["nonCommercial"] != false {
		t.Fatalf("an upscaler is advertised with id, name, kind, worker, scales, max, nonCommercial: %v", g)
	}

	src := pngOf(t, 1000, 600, false)
	for _, c := range []struct {
		fields        string
		w, h, scale   int
		width, height int
	}{
		{`"kind":"upscale","scale":2`, 1000, 600, 2, 2000, 1200},
		{`"kind":"upscale","scale":3,"prompt":"ignored"`, 1000, 600, 2, 2000, 1200},     // 3 → 2
		{`"kind":"upscale"`, 1000, 600, 2, 2000, 1200},                                  // 0 → the smallest
		{`"generator":"seedvr2-3b","kind":"image","scale":8`, 1366, 768, 4, 4094, 2302}, // 8 → 4, cut to 4096
		{`"kind":"upscale","scale":4`, 2048, 1000, 4, 4096, 2000},
	} {
		im := src
		if c.w != 1000 {
			im = pngOf(t, c.w, c.h, false)
		}
		_, ch := b.generate(c.fields + "," + imagesField(im))
		first := genUntilState(t, ch, "")
		if first.M["width"] != float64(c.width) || first.M["height"] != float64(c.height) || first.M["worker"] != "Mac" {
			t.Fatalf("%s: first status %+v", c.fields, first)
		}
		evs := genUntil(t, ch)
		done := evs[len(evs)-1]
		if done.Type != "generateDone" || done.M["width"] != float64(c.width) || done.M["height"] != float64(c.height) || done.M["generator"] != "seedvr2-3b" {
			t.Fatalf("%s: done %+v", c.fields, done)
		}
		if p := evs[len(evs)-2]; p.Type != "generateFile" || p.M["mime"] != "image/png" {
			t.Fatalf("%s: file %+v", c.fields, p.M["mime"])
		}
		r, images := mac.flux.last()
		if r.Model != "seedvr2-3b" || r.Scale != c.scale || r.Prompt != "" || r.Width != 0 || r.Client != "vboard: Studio AI" ||
			len(images) != 1 || !bytes.Equal(images[0], im) {
			t.Fatalf("%s: job %+v (%d images)", c.fields, r, len(images))
		}
	}

	// errors
	big := pngOf(t, 2049, 10, false)
	for fields, want := range map[string]string{
		`"kind":"upscale"`:                                                        "send exactly one (not 0)",
		`"kind":"upscale",` + imagesField(src, src):                               "send exactly one (not 2)",
		`"kind":"upscale",` + imagesField(big):                                    "already as large as SeedVR2 3B can make it",
		`"kind":"upscale","scale":-2,` + imagesField(src):                         "0 (the default) or more",
		`"kind":"upscale",` + imagesField([]byte("not a picture")):                "size cannot be read",
		`"kind":"upscale","images":[{"id":"x","mime":"image/gif","data":"R0lG"}]`: "not taken",
		`"generator":"seedvr2-7b","kind":"upscale",` + imagesField(src):           `"seedvr2-7b" is not available now`,
	} {
		if msg := genError(t, b, fields); !strings.Contains(msg, want) {
			t.Fatalf("%.80s: %q", fields, msg)
		}
	}
	// an image generator is not an upscaler, nor the other way round
	if msg := genError(t, b, `"generator":"seedvr2-3b","prompt":"a cat"`); !strings.Contains(msg, "exactly one") {
		t.Fatalf("text to an upscaler: %q", msg)
	}
}

// Without an upscaler: kind upscale is refused (and an older generator list is unchanged).
func TestVboardUpscaleNone(t *testing.T) {
	fastGenerate(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	startGenMac(t, base, wtok)
	v := newFakeVboard(t, stageGenerators)
	addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret","generators":["flux2-klein-4b"]}`)
	b := v.next(t)
	if got := b.helloGenerators(); !reflect.DeepEqual(got, []vbGenerator{wantFlux}) {
		t.Fatalf("hello %s", b.raw)
	}
	if msg := genError(t, b, `"kind":"upscale",`+imagesField(pngOf(t, 10, 10, false))); !strings.Contains(msg, "no upscale generator is offered") {
		t.Fatalf("%q", msg)
	}
}

// An image larger than vboard takes as PNG goes as a JPEG (quality 92), for any kind; still too large → error.
func TestVboardGenerateJPEG(t *testing.T) {
	fastGenerate(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	mac := startGenMac(t, base, wtok)
	v := newFakeVboard(t, stageGenerators)
	addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret","generators":["flux2-klein-4b","seedvr2-3b"]}`)
	b := v.next(t)

	// a real-sized one: a 2048² PNG of noise is 16 MB
	noise := pngOf(t, 2048, 2048, true)
	if len(noise) <= 15<<20 {
		t.Fatalf("the PNG is %d bytes: not larger than 15 MB", len(noise))
	}
	mac.flux.mu.Lock()
	mac.flux.data = noise
	mac.flux.mu.Unlock()
	_, ch := b.generate(`"prompt":"static"`)
	evs := genUntil(t, ch)
	if evs[len(evs)-1].Type != "generateDone" {
		t.Fatalf("%+v", evs[len(evs)-1])
	}
	var file bytes.Buffer
	var size float64
	for _, ev := range evs {
		if ev.Type == "generateFile" {
			if ev.M["mime"] != "image/jpeg" {
				t.Fatalf("mime %v", ev.M["mime"])
			}
			data, _ := base64.StdEncoding.DecodeString(ev.M["data"].(string))
			file.Write(data)
			size = ev.M["size"].(float64)
		}
	}
	im, err := jpeg.Decode(bytes.NewReader(file.Bytes()))
	if err != nil || float64(file.Len()) != size || size > 15<<20 || im.Bounds().Dx() != 2048 || im.Bounds().Dy() != 2048 {
		t.Fatalf("the JPEG: %v, %d bytes (said %v)", err, file.Len(), size)
	}

	// an upscale too, smaller limits to be quick: a PNG of 1 MB with a limit of 600 KB goes as a JPEG
	old := genMaxImageFile
	t.Cleanup(func() { genMaxImageFile = old })
	genMaxImageFile = 600 << 10
	small := pngOf(t, 512, 512, true)
	mac.flux.mu.Lock()
	mac.flux.data = small
	mac.flux.mu.Unlock()
	_, ch = b.generate(`"kind":"upscale",` + imagesField(pngOf(t, 256, 256, false)))
	evs = genUntil(t, ch)
	if p := evs[len(evs)-2]; evs[len(evs)-1].Type != "generateDone" || p.M["mime"] != "image/jpeg" {
		t.Fatalf("upscale: %+v", evs[len(evs)-1])
	}
	// still too large as a JPEG
	genMaxImageFile = 20 << 10
	if msg := genFailed(t, b, `"prompt":"static"`); !strings.Contains(msg, "image (as JPEG) is") || !strings.Contains(msg, "more than vboard takes") {
		t.Fatalf("%q", msg)
	}
	// a PNG that is no picture cannot become one
	mac.flux.mu.Lock()
	mac.flux.data = genBytes(100 << 10)
	mac.flux.mu.Unlock()
	if msg := genFailed(t, b, `"prompt":"static"`); !strings.Contains(msg, "could not be made a JPEG") {
		t.Fatalf("%q", msg)
	}
	// small enough: the PNG as before
	mac.flux.mu.Lock()
	mac.flux.data = pngOf(t, 16, 16, false)
	mac.flux.mu.Unlock()
	_, ch = b.generate(`"prompt":"x"`)
	evs = genUntil(t, ch)
	if p := evs[len(evs)-2]; p.M["mime"] != "image/png" {
		t.Fatalf("small: %+v", p.M["mime"])
	}
}

func TestImageSize(t *testing.T) {
	var jb bytes.Buffer
	jpeg.Encode(&jb, image.NewGray(image.Rect(0, 0, 33, 17)), nil)
	webp := func(chunk string, body []byte) []byte {
		b := append([]byte("RIFF\x00\x00\x00\x00WEBP"+chunk+"\x00\x00\x00\x00"), body...)
		return append(b, make([]byte, 40)...)
	}
	vp8l := uint32(300-1) | uint32(200-1)<<14
	for name, c := range map[string]struct {
		b    []byte
		w, h int
	}{
		"png":           {pngOf(t, 40, 30, false), 40, 30},
		"jpeg":          {jb.Bytes(), 33, 17},
		"webp lossy":    {webp("VP8 ", []byte{0, 0, 0, 0x9d, 0x01, 0x2a, 0x40, 0x01, 0xf0, 0x00}), 320, 240},
		"webp lossless": {webp("VP8L", []byte{0x2f, byte(vp8l), byte(vp8l >> 8), byte(vp8l >> 16), byte(vp8l >> 24)}), 300, 200},
		"webp extended": {webp("VP8X", []byte{0, 0, 0, 0, 0xff, 0x0f, 0, 0x7f, 0x02, 0}), 4096, 640},
	} {
		w, h, err := imageSize(c.b)
		if err != nil || w != c.w || h != c.h {
			t.Errorf("%s: %d×%d %v, want %d×%d", name, w, h, err, c.w, c.h)
		}
	}
	if _, _, err := imageSize([]byte("RIFF\x00\x00\x00\x00WEBPVP8 ")); err == nil {
		t.Error("a cut WebP")
	}
}

// The hub works out an upscale's size as the worker does.
func TestUpscaleSizeAsEngine(t *testing.T) {
	for _, s := range [][3]int{{1000, 600, 2}, {1366, 768, 4}, {800, 1200, 4}, {1111, 999, 4}, {2049, 1537, 2}, {333, 2047, 2}, {1023, 767, 4}, {3, 5, 2}} {
		w, h := upscaleSize(s[0], s[1], s[2], genUpscaleMax)
		ew, eh := engine.UpscaleSize(s[0], s[1], s[2], engine.UpscaleMax)
		if w != ew || h != eh {
			t.Errorf("%v: hub %d×%d, engine %d×%d", s, w, h, ew, eh)
		}
	}
}

// genFailed is the error a generation ends with (after its statuses).
func genFailed(t *testing.T, b *fakeBridge, fields string) string {
	t.Helper()
	_, ch := b.generate(fields)
	evs := genUntil(t, ch)
	last := evs[len(evs)-1]
	if last.Type != "error" {
		t.Fatalf("%s: %+v", fields, last)
	}
	msg, _ := last.M["message"].(string)
	return msg
}
