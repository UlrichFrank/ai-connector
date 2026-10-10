package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/web"
)

// The fake vboard's generators stage (docs/api.md, "vboard: generating images and videos on the board"): it reads
// hello.generators and {type:"generators"}, sends {generate} and {generateCancel}, and collects the answers.

// vbGenerator is a generator as vboard reads it.
type vbGenerator struct {
	ID, Name, Kind, Worker string
	Edit                   bool
	TextToImage            *bool
	FirstFrame, Audio      bool
	Multiple, Max          int
	MaxSeconds             int
	Default                struct{ Width, Height, FPS, Seconds int }
	NonCommercial          bool
}

// vbGenEv is an answer to a generate: generateStatus, generateFile, generateDone or error, as sent.
type vbGenEv struct {
	Type string
	M    map[string]any
}

func vbGenerators(raw json.RawMessage) []vbGenerator {
	var out []vbGenerator
	_ = json.Unmarshal(raw, &out)
	return out
}

// genMessage takes the generators' messages (false: not one of them).
func (b *fakeBridge) genMessage(typ, id string, raw []byte) bool {
	switch typ {
	case "generators":
		var m struct{ Generators json.RawMessage }
		_ = json.Unmarshal(raw, &m)
		b.genList <- vbGenerators(m.Generators)
		return true
	case "generateStatus", "generateFile", "generateDone":
	case "error":
		if _, ok := b.gens.Load(id); !ok {
			return false
		}
	default:
		return false
	}
	ch, ok := b.gens.Load(id)
	if !ok {
		return true
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if typ == "generateDone" || typ == "error" {
		b.gens.Delete(id)
	}
	ch.(chan vbGenEv) <- vbGenEv{typ, m}
	return true
}

// helloGenerators is hello.generators as sent (nil: the field is not there).
func (b *fakeBridge) helloGenerators() []vbGenerator {
	var h map[string]json.RawMessage
	_ = json.Unmarshal(b.raw, &h)
	if h["generators"] == nil {
		return nil
	}
	return vbGenerators(h["generators"])
}

// generate sends a {generate} with these fields; its answers arrive on the channel.
func (b *fakeBridge) generate(fields string) (string, chan vbGenEv) {
	id := "g" + b.id()
	ch := make(chan vbGenEv, 1024)
	b.gens.Store(id, ch)
	msg := `{"type":"generate","id":"` + id + `",` + fields + `}`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		b.v.t.Fatal(err)
	}
	return id, ch
}

// genUntil reads a generation's answers up to its end (done or error).
func genUntil(t *testing.T, ch chan vbGenEv) []vbGenEv {
	t.Helper()
	var evs []vbGenEv
	for {
		select {
		case ev := <-ch:
			evs = append(evs, ev)
			if ev.Type == "generateDone" || ev.Type == "error" {
				return evs
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the generation did not end: %+v", evs)
		}
	}
}

func genError(t *testing.T, b *fakeBridge, fields string) string {
	t.Helper()
	_, ch := b.generate(fields)
	evs := genUntil(t, ch)
	if len(evs) != 1 || evs[0].Type != "error" {
		t.Fatalf("%s: %+v", fields, evs)
	}
	msg, _ := evs[0].M["message"].(string)
	return msg
}

func nextGenerators(t *testing.T, b *fakeBridge) []vbGenerator {
	t.Helper()
	select {
	case g := <-b.genList:
		return g
	case <-time.After(10 * time.Second):
		t.Fatal("no generators update")
	}
	return nil
}

// ---------- the worker side ----------

// fakeGen makes a picture (mflux) or a video (ltx): progress in four steps, then a file of size bytes. While hold is
// set, it waits for it (the model "loads").
type fakeGen struct {
	name, ext string
	size      int

	mu     sync.Mutex
	got    []engine.Request
	images [][]byte // the reference images of the last job, as read from their files
	hold   chan struct{}
	loaded string
}

func (b *fakeGen) Name() string { return b.name }
func (b *fakeGen) Heavy() bool  { return true }
func (b *fakeGen) Loaded() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loaded
}
func (b *fakeGen) Unload() {
	b.mu.Lock()
	b.loaded = ""
	b.mu.Unlock()
}

func (b *fakeGen) Run(ctx context.Context, m *engine.Model, r engine.Request, outDir string, emit engine.Emit) error {
	var images [][]byte
	for _, p := range r.Images {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		images = append(images, data)
	}
	b.mu.Lock()
	b.got, b.images, b.loaded = append(b.got, r), images, m.ID
	hold := b.hold
	b.mu.Unlock()
	emit.Status("loading " + m.ID + " …")
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for i := 1; i <= 4; i++ {
		emit.Progress(i, 4)
		select {
		case <-time.After(40 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	seed := r.Seed
	if seed == 0 {
		seed = 42
	}
	out := filepath.Join(outDir, fmt.Sprintf("gen-%d%s", time.Now().UnixNano(), b.ext))
	if err := os.WriteFile(out, genBytes(b.size), 0o644); err != nil {
		return err
	}
	stats := map[string]any{"seed": seed, "width": r.Width, "height": r.Height}
	if r.Frames > 0 {
		stats["frames"], stats["fps"] = r.Frames, r.FPS
	}
	emit(engine.Event{Type: "stats", Data: stats})
	emit(engine.Event{Type: "file", File: out})
	return nil
}

func (b *fakeGen) setHold(h chan struct{}) {
	b.mu.Lock()
	b.hold = h
	b.mu.Unlock()
}

func (b *fakeGen) last() (engine.Request, [][]byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.got[len(b.got)-1], b.images
}

// genBytes is a file's content: every byte known (a piece out of place shows).
func genBytes(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*7 + i/1000)
	}
	return out
}

// heavyChat is the chat model of the built-in runtime: large, it stays in memory after its job.
type heavyChat struct {
	*vbChat
	mu     sync.Mutex
	loaded string
}

func (b *heavyChat) Heavy() bool { return true }
func (b *heavyChat) Loaded() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loaded
}
func (b *heavyChat) Unload() {
	b.mu.Lock()
	b.loaded = ""
	b.mu.Unlock()
}
func (b *heavyChat) Run(ctx context.Context, m *engine.Model, r engine.Request, out string, emit engine.Emit) error {
	b.mu.Lock()
	b.loaded = m.ID
	b.mu.Unlock()
	return b.vbChat.Run(ctx, m, r, out, emit)
}

type genMac struct {
	mgr        *jobs.Manager
	chat       *heavyChat
	flux, ltxv *fakeGen
}

// startGenMac connects a worker with a chat model and the image and video models (FLUX.2 klein 9B not on disk).
func startGenMac(t *testing.T, base, token string) *genMac {
	t.Helper()
	img := engine.Request{Width: 1024, Height: 1024, Steps: 4}
	cat := &engine.Catalog{Models: []*engine.Model{
		{ID: "qwen3.6-35b-a3b", Name: "Qwen3.6", Kind: engine.Chat, Backend: "vbchat", Size: gib, Here: true},
		{ID: "flux2-klein-4b", Name: "FLUX.2 [klein] 4B", Kind: engine.Image, Backend: "mflux", Size: gib, Here: true, Defaults: img},
		{ID: "flux2-klein-9b", Name: "FLUX.2 [klein] 9B", Kind: engine.Image, Backend: "mflux", Size: gib, Defaults: img},
		{ID: "qwen-image-2512", Name: "Qwen-Image 2512", Kind: engine.Image, Backend: "mflux", Size: gib, Here: true, Defaults: img,
			Tasks: []string{"generate"}, NonCommercial: true},
		{ID: "qwen-image-edit-2511", Name: "Qwen-Image-Edit 2511", Kind: engine.Image, Backend: "mflux", Size: gib, Here: true,
			Defaults: img, Tasks: []string{"edit"}},
		{ID: "ltx-2.5-q4", Name: "LTX-2.5 (q4)", Kind: engine.Video, Backend: "ltx", Size: gib, Here: true,
			Defaults: engine.Request{Width: 704, Height: 448, FPS: 24, Frames: 97}},
	}}
	m := &genMac{chat: &heavyChat{vbChat: &vbChat{}}, flux: &fakeGen{name: "mflux", ext: ".png", size: 2<<20 + 512<<10}, ltxv: &fakeGen{name: "ltx", ext: ".mp4", size: 3000}}
	dir := t.TempDir()
	m.mgr = jobs.New(cat, []engine.Backend{m.chat, m.flux, m.ltxv}, dir)
	api := (&web.Server{Jobs: m.mgr, Prompts: prompts.Open(dir), Version: "test"}).Handler()
	stop, _ := connectWorkerWith(t, base, token, withRAM(api, 64))
	t.Cleanup(func() {
		stop()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !m.mgr.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	return m
}

func fastGenerate(t *testing.T) {
	t.Helper()
	fastBridges(t)
	p := genPoll
	genPoll = 20 * time.Millisecond
	t.Cleanup(func() { genPoll = p })
}

var (
	wantFlux = vbGenerator{ID: "flux2-klein-4b", Name: "FLUX.2 [klein] 4B", Kind: "image", Worker: "Mac", Edit: true, Multiple: 16,
		Max: 2048, Default: struct{ Width, Height, FPS, Seconds int }{1024, 1024, 0, 0}}
	wantLTX = vbGenerator{ID: "ltx-2.5-q4", Name: "LTX-2.5 (q4)", Kind: "video", Worker: "Mac", FirstFrame: true, Audio: true,
		Multiple: 64, Max: 1280, MaxSeconds: 10, Default: struct{ Width, Height, FPS, Seconds int }{704, 448, 24, 4}}
)

// ---------- tests ----------

// The generators: none without the setting (the hello as before), those on disk in the setting's order, an edit sends
// {type:"generators"} on the open connection (no new connection), a handover's hello has them.
func TestVboardGenerators(t *testing.T) {
	fastGenerate(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	startGenMac(t, base, wtok)
	v := newFakeVboard(t, stageGenerators)
	c := addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret"}`)
	if c.Generators == nil || len(c.Generators) != 0 {
		t.Fatalf("generators %#v", c.Generators)
	}
	b := v.next(t)
	if strings.Contains(string(b.raw), "generators") || b.hello.Chat != "qwen3.6-35b-a3b" {
		t.Fatalf("hello without the setting: %s", b.raw)
	}

	// the setting: an image model, one not on disk, a chat model (not a generator), the video model, a text-only one
	code, out := patchConnector(t, base, device, c.ID, `{"generators":["flux2-klein-4b","flux2-klein-9b","qwen3.6-35b-a3b","ltx-2.5-q4","qwen-image-2512"]}`)
	if code != 200 || len(out["generators"].([]any)) != 5 {
		t.Fatalf("patch %d %v", code, out)
	}
	no := false
	qwen := vbGenerator{ID: "qwen-image-2512", Name: "Qwen-Image 2512", Kind: "image", Worker: "Mac", Multiple: 16, Max: 2048,
		Default: struct{ Width, Height, FPS, Seconds int }{1024, 1024, 0, 0}, NonCommercial: true}
	if got := nextGenerators(t, b); !reflect.DeepEqual(got, []vbGenerator{wantFlux, wantLTX, qwen}) {
		t.Fatalf("update %+v", got)
	}
	// an editing model says it needs an image
	patchConnector(t, base, device, c.ID, `{"generators":["qwen-image-edit-2511"]}`)
	edit := vbGenerator{ID: "qwen-image-edit-2511", Name: "Qwen-Image-Edit 2511", Kind: "image", Worker: "Mac", Edit: true,
		TextToImage: &no, Multiple: 16, Max: 2048, Default: struct{ Width, Height, FPS, Seconds int }{1024, 1024, 0, 0}}
	if got := nextGenerators(t, b); !reflect.DeepEqual(got, []vbGenerator{edit}) {
		t.Fatalf("update %+v", got)
	}
	patchConnector(t, base, device, c.ID, `{"generators":["ltx-2.5-q4","flux2-klein-4b"]}`)
	if got := nextGenerators(t, b); !reflect.DeepEqual(got, []vbGenerator{wantLTX, wantFlux}) {
		t.Fatalf("update %+v", got)
	}
	if _, dials := v.count(); dials != 1 {
		t.Fatalf("%d connections: generators must not force a handover", dials)
	}
	// a handover: the new hello has them
	b.write(map[string]string{"type": "reconnect"})
	b2 := v.next(t)
	if got := b2.helloGenerators(); !reflect.DeepEqual(got, []vbGenerator{wantLTX, wantFlux}) {
		t.Fatalf("hello after the handover %s", b2.raw)
	}
	// off again: an empty list
	patchConnector(t, base, device, c.ID, `{"generators":[]}`)
	if got := nextGenerators(t, b2); len(got) != 0 {
		t.Fatalf("update %+v", got)
	}
	// checked: unique, no blanks
	for _, body := range []string{`{"generators":["a","A"]}`, `{"generators":[" "]}`} {
		if code, out := patchConnector(t, base, device, c.ID, body); code != 400 || out["error"] != "generators" {
			t.Fatalf("%s: %d %v", body, code, out)
		}
	}
}

// Older vboards: a connector with generators says the same to them as before, besides a hello field they drop.
func TestVboardGeneratorsOlder(t *testing.T) {
	fastGenerate(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	startGenMac(t, base, wtok)
	v := newFakeVboard(t, stageImages)
	addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret","generators":["flux2-klein-4b"]}`)
	b := v.next(t)
	want := vbHello{Name: "ai-connector", Device: "ai-connector", Chat: "qwen3.6-35b-a3b", Instance: b.hello.Instance, Handover: true}
	if b.hello != want || !reflect.DeepEqual(b.agents, []vbAgent{{ID: "default", Name: "Qwen3.6", Kind: "model", Worker: "Mac"}}) {
		t.Fatalf("hello %+v agents %+v", b.hello, b.agents)
	}
	_, ch := b.chat(`[{"role":"user","content":"hi"}]`, "")
	if evs := until(t, ch); evs[len(evs)-1].Type != "done" {
		t.Fatalf("chat %+v", evs)
	}
}

// A picture: the status at once with the rounded size, loading while the model is read, the steps, the file in
// pieces of at most 1 MiB, done. A video from a first frame. Errors.
func TestVboardGenerate(t *testing.T) {
	fastGenerate(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	mac := startGenMac(t, base, wtok)
	v := newFakeVboard(t, stageGenerators)
	addConnector(t, base, device, `{"name":"Studio AI","url":"`+v.srv.URL+`","token":"vbp_secret",
		"generators":["flux2-klein-4b","ltx-2.5-q4","qwen-image-2512","qwen-image-edit-2511"]}`)
	b := v.next(t)
	if got := b.helloGenerators(); len(got) != 4 || got[0] != wantFlux || got[1] != wantLTX {
		t.Fatalf("hello %s", b.raw)
	}

	hold := make(chan struct{})
	mac.flux.setHold(hold)
	_, ch := b.generate(`"generator":"flux2-klein-4b","kind":"image","prompt":"  a lighthouse at dusk ","width":1000,"height":600,"seed":7,
		"context":{"boardId":"b_1","userId":"u_1","userName":"Anna","placeholderId":"i_9"}`)
	first := genUntilState(t, ch, "")
	if first.Type != "generateStatus" || first.M["width"] != 992.0 || first.M["height"] != 592.0 || first.M["worker"] != "Mac" {
		t.Fatalf("first status %+v", first)
	}
	if s := first.M["state"]; s != "queued" && s != "loading" {
		t.Fatalf("first state %v", s)
	}
	genUntilState(t, ch, "loading")
	close(hold)
	running := genUntilState(t, ch, "running")
	if running.M["total"] != 4.0 {
		t.Fatalf("running %+v", running)
	}
	evs := genUntil(t, ch)
	var file bytes.Buffer
	seq := 0
	for _, ev := range evs {
		if ev.Type != "generateFile" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(ev.M["data"].(string))
		if err != nil || len(data) > 1<<20 || ev.M["seq"] != float64(seq) || ev.M["mime"] != "image/png" || ev.M["size"] != float64(mac.flux.size) ||
			ev.M["last"] != (file.Len()+len(data) == mac.flux.size) {
			t.Fatalf("piece %d: %v %d bytes %+v", seq, err, len(data), ev.M["last"])
		}
		file.Write(data)
		seq++
	}
	if seq != 3 || !bytes.Equal(file.Bytes(), genBytes(mac.flux.size)) {
		t.Fatalf("%d pieces, %d bytes", seq, file.Len())
	}
	done := evs[len(evs)-1]
	if done.Type != "generateDone" || done.M["width"] != 992.0 || done.M["height"] != 592.0 || done.M["seed"] != 7.0 ||
		done.M["generator"] != "flux2-klein-4b" || done.M["took"].(float64) <= 0 || done.M["seconds"] != nil {
		t.Fatalf("done %+v", done)
	}
	r, _ := mac.flux.last()
	if r.Prompt != "a lighthouse at dusk" || r.Width != 992 || r.Height != 592 || r.Seed != 7 || r.Client != "vboard: Studio AI" || len(r.Images) != 0 {
		t.Fatalf("job %+v", r)
	}

	// a video from a first frame: the size from the default's aspect ratio, 2 s at 24 fps = 49 frames, no sound
	mac.flux.setHold(nil)
	frame := genBytes(5000)
	_, ch = b.generate(`"kind":"video","prompt":"waves","width":1000,"seconds":2,"audio":false,
		"images":[{"id":"image-1","mime":"image/jpeg","data":"` + base64.StdEncoding.EncodeToString(frame) + `"}]`)
	first = genUntilState(t, ch, "")
	if first.M["width"] != 960.0 || first.M["height"] != 576.0 || first.M["frames"] != 49.0 || first.M["fps"] != 24.0 {
		t.Fatalf("video status %+v", first)
	}
	evs = genUntil(t, ch)
	done = evs[len(evs)-1]
	if done.Type != "generateDone" || done.M["seconds"] != 2.0 || done.M["seed"] != 42.0 || done.M["generator"] != "ltx-2.5-q4" {
		t.Fatalf("video done %+v", evs)
	}
	if p := evs[len(evs)-2]; p.Type != "generateFile" || p.M["mime"] != "video/mp4" || p.M["last"] != true || p.M["seq"] != 0.0 {
		t.Fatalf("video file %+v", p.M["mime"])
	}
	r, images := mac.ltxv.last()
	if r.Frames != 49 || r.FPS != 24 || !r.NoAudio || r.Width != 960 || r.Height != 576 || len(images) != 1 || !bytes.Equal(images[0], frame) {
		t.Fatalf("video job %+v (%d images)", r, len(images))
	}

	// a reference image to FLUX (it edits)
	_, ch = b.generate(`"prompt":"make it a photo","images":[{"id":"image-1","mime":"image/png","data":"` + base64.StdEncoding.EncodeToString(frame) + `"}]`)
	if evs := genUntil(t, ch); evs[len(evs)-1].Type != "generateDone" {
		t.Fatalf("edit %+v", evs)
	}
	if _, images := mac.flux.last(); len(images) != 1 || !bytes.Equal(images[0], frame) {
		t.Fatalf("edit images %d", len(images))
	}

	// errors
	png := `"images":[{"id":"image-1","mime":"image/png","data":"` + base64.StdEncoding.EncodeToString(frame) + `"}]`
	for fields, want := range map[string]string{
		`"generator":"sdxl","kind":"image","prompt":"x"`:    `"sdxl" is not available now`,
		`"generator":"flux2-klein-9b","prompt":"x"`:         `"flux2-klein-9b" is not available now`,
		`"generator":"qwen-image-2512","prompt":"x",` + png: "takes no reference images",
		`"generator":"qwen-image-edit-2511","prompt":"x"`:   "needs at least one reference image",
		`"prompt":" "`:            "a prompt is needed",
		`"prompt":"x","width":-1`: "0 (the default) or more",
		`"prompt":"x","images":[{"id":"image-1","mime":"image/gif","data":"R0lG"}]`: "not taken",
		`"prompt":"` + strings.Repeat("x", 4001) + `"`:                              "longer than 4000",
	} {
		if msg := genError(t, b, fields); !strings.Contains(msg, want) {
			t.Fatalf("%.80s: %q", fields, msg)
		}
	}
}

// genUntilState reads a generation's answers up to a status of that state ("" the next status).
func genUntilState(t *testing.T, ch chan vbGenEv, state string) vbGenEv {
	t.Helper()
	for {
		select {
		case ev := <-ch:
			if ev.Type == "error" || ev.Type == "generateDone" {
				t.Fatalf("waiting for %q: %+v", state, ev)
			}
			if ev.Type == "generateStatus" && (state == "" || ev.M["state"] == state) {
				return ev
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no status %q", state)
		}
	}
}

// A second picture waits behind the first (its position), a cancel stops the job on the worker and nothing more is
// sent for it; the one ahead goes on.
func TestVboardGenerateQueueAndCancel(t *testing.T) {
	fastGenerate(t)
	s, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	mac := startGenMac(t, base, wtok)
	v := newFakeVboard(t, stageGenerators)
	addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret","generators":["flux2-klein-4b"]}`)
	b := v.next(t)

	hold := make(chan struct{})
	mac.flux.setHold(hold)
	_, first := b.generate(`"prompt":"one"`)
	genUntilState(t, first, "loading")
	waitFor(t, "the worker says it is busy", func() bool {
		for _, n := range s.fleet.list() {
			if _, _, st, _ := n.snapshot(); st.Running != nil {
				return true
			}
		}
		return false
	})
	id, second := b.generate(`"prompt":"two"`)
	if st := genUntilState(t, second, ""); st.M["state"] != "queued" || st.M["position"] != 1.0 {
		t.Fatalf("second %+v", st)
	}
	b.write(map[string]string{"type": "generateCancel", "id": id})
	waitFor(t, "the second job canceled on the worker", func() bool {
		for _, j := range mac.mgr.Jobs() {
			if j.Request.Prompt == "two" {
				return j.State == jobs.Canceled
			}
		}
		return false
	})
	close(hold)
	if evs := genUntil(t, first); evs[len(evs)-1].Type != "generateDone" {
		t.Fatalf("first %+v", evs)
	}
	select {
	case ev := <-second:
		t.Fatalf("after the cancel: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}

	// a running one is canceled too
	hold = make(chan struct{})
	mac.flux.setHold(hold)
	id, third := b.generate(`"prompt":"three"`)
	genUntilState(t, third, "loading")
	b.write(map[string]string{"type": "generateCancel", "id": id})
	waitFor(t, "the third job canceled on the worker", func() bool {
		for _, j := range mac.mgr.Jobs() {
			if j.Request.Prompt == "three" {
				return j.State == jobs.Canceled
			}
		}
		return false
	})
	select {
	case ev := <-third:
		if ev.Type != "generateStatus" {
			t.Fatalf("after the cancel: %+v", ev)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

// One large model in memory at a time: a picture's job unloads the chat model the chat before left loaded.
func TestVboardGenerateUnloadsChat(t *testing.T) {
	fastGenerate(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	mac := startGenMac(t, base, wtok)
	v := newFakeVboard(t, stageGenerators)
	addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret","generators":["flux2-klein-4b"]}`)
	b := v.next(t)
	_, chat := b.chat(`[{"role":"user","content":"hi"}]`, "")
	if evs := until(t, chat); evs[len(evs)-1].Type != "done" || mac.chat.Loaded() != "qwen3.6-35b-a3b" {
		t.Fatalf("chat %+v, loaded %q", evs, mac.chat.Loaded())
	}
	_, ch := b.generate(`"prompt":"x"`)
	if evs := genUntil(t, ch); evs[len(evs)-1].Type != "generateDone" {
		t.Fatalf("%+v", evs)
	}
	if mac.chat.Loaded() != "" || mac.flux.Loaded() != "flux2-klein-4b" {
		t.Fatalf("loaded: chat %q, flux %q", mac.chat.Loaded(), mac.flux.Loaded())
	}
}
