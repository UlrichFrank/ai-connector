package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/tunnel"
	"github.com/philipparndt/ai-connector/internal/web"
)

// fakeBackend makes a small file per job; while hold is set, a job waits for it to be closed.
type fakeBackend struct {
	mu     sync.Mutex
	loaded string
	hold   chan struct{}
}

func (b *fakeBackend) Name() string { return "fake" }
func (b *fakeBackend) Heavy() bool  { return false }
func (b *fakeBackend) Loaded() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loaded
}
func (b *fakeBackend) Unload() {
	b.mu.Lock()
	b.loaded = ""
	b.mu.Unlock()
}
func (b *fakeBackend) Run(ctx context.Context, m *engine.Model, r engine.Request, outDir string, emit engine.Emit) error {
	b.mu.Lock()
	b.loaded = m.ID
	hold := b.hold
	b.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	os.MkdirAll(outDir, 0o755)
	f := filepath.Join(outDir, "out-"+time.Now().Format("150405.000000000")+".png")
	if err := os.WriteFile(f, []byte("image of "+r.Prompt), 0o644); err != nil {
		return err
	}
	emit(engine.Event{Type: "file", File: f})
	return nil
}

func (b *fakeBackend) block() (release func()) {
	ch := make(chan struct{})
	b.mu.Lock()
	b.hold = ch
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.hold = nil
			b.mu.Unlock()
			close(ch)
		})
	}
}

const gib = int64(1) << 30

// fakeMac is a real worker API (internal/web over a job manager) with a fake backend, claiming ramGB of memory.
type fakeMac struct {
	mgr     *jobs.Manager
	prompts *prompts.Library
	backend *fakeBackend
	stop    func()
}

func startMac(t *testing.T, base, token string, ramGB int) *fakeMac {
	t.Helper()
	cat := &engine.Catalog{Models: []*engine.Model{
		{ID: "small", Name: "Small", Kind: engine.Image, Backend: "fake", Size: 2 * gib, Here: true},
		{ID: "big", Name: "Big", Kind: engine.Image, Backend: "fake", Size: 40 * gib},        // ~58 GB
		{ID: "huge", Name: "Huge", Kind: engine.Image, Backend: "fake", Size: 100 * gib},     // fits nothing
		{ID: "ltx-2.5-q4", Name: "LTX", Kind: engine.Image, Backend: "fake", Size: 30 * gib}, // a known peak: ~22 GB
	}}
	be := &fakeBackend{}
	dir := t.TempDir()
	mgr := jobs.New(cat, []engine.Backend{be}, dir)
	lib := prompts.Open(dir)
	api := (&web.Server{Jobs: mgr, Prompts: lib, Version: "test"}).Handler()
	// the memory this Mac claims (MLXLAB_FAKE_RAM_GB is per process; these Macs share one)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/info" {
			api.ServeHTTP(w, r)
			return
		}
		rec := &recorder{header: http.Header{}}
		api.ServeHTTP(rec, r)
		var info map[string]any
		json.Unmarshal(rec.body.Bytes(), &info)
		info["specs"].(map[string]any)["ramGB"] = ramGB
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
	})
	stop, _ := connectWorkerWith(t, base, token, h)
	m := &fakeMac{mgr: mgr, prompts: lib, backend: be, stop: stop}
	t.Cleanup(func() {
		stop()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !mgr.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	return m
}

type recorder struct {
	header http.Header
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(int)             {}

type jobView struct {
	ID      string            `json:"id"`
	State   string            `json:"state"`
	Files   []string          `json:"files"`
	Request map[string]any    `json:"request"`
	Worker  map[string]string `json:"worker"`
}

func submitJob(t *testing.T, base, device, body string) (int, jobView, map[string]string) {
	t.Helper()
	r := do(t, "POST", base+"/api/jobs", device, strings.NewReader(body))
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	var j jobView
	var e map[string]string
	if r.StatusCode == 200 {
		json.Unmarshal(b, &j)
	} else {
		json.Unmarshal(b, &e)
	}
	return r.StatusCode, j, e
}

func getJob(t *testing.T, base, device, id string) jobView {
	t.Helper()
	var j jobView
	r := do(t, "GET", base+"/api/jobs/"+id, device, nil)
	if r.StatusCode != 200 {
		t.Fatalf("job %s: %d", id, r.StatusCode)
	}
	decode(t, r, &j)
	return j
}

func waitDone(t *testing.T, base, device, id string) jobView {
	t.Helper()
	var j jobView
	waitFor(t, "job "+id+" done", func() bool { j = getJob(t, base, device, id); return j.State == "done" })
	return j
}

// workerItems is GET /api/workers by id.
func workerItems(t *testing.T, base, device string) map[string]workerItem {
	t.Helper()
	var l []workerItem
	decode(t, do(t, "GET", base+"/api/workers", device, nil), &l)
	out := map[string]workerItem{}
	for _, w := range l {
		out[w.ID] = w
	}
	return out
}

func TestNeed(t *testing.T) {
	for _, c := range []struct {
		m    catalogModel
		want float64
	}{
		{catalogModel{ID: "qwen3.5-9b", Size: 10 * gib}, 19},
		{catalogModel{ID: "ltx-2.5-q4", Size: 30 * gib}, 22},
		{catalogModel{ID: "ltx-2.5-hq-q8", Size: 65 * gib}, 40}, // its measured peak, not 65 GB × 1.3
		{catalogModel{ID: "ltx-2.5-hq-q4", Size: 47 * gib}, 32},
		{catalogModel{ID: "longcat-avatar-1.5-q8", Size: 30 * gib}, 43},
		{catalogModel{ID: "ace-step-1.5-xl-turbo", Size: 20 * gib}, 16},
		{catalogModel{ID: "flux2-klein-4b", Size: 16 * gib}, 21},
		{catalogModel{ID: "qwen3.6-35b-a3b", Size: 20 * gib}, 25},
		{catalogModel{ID: "minimax-music3-8bit", Size: 13 * gib}, 27},
	} {
		if got := needGB(c.m); got != c.want {
			t.Errorf("%s: %v, want %v", c.m.ID, got, c.want)
		}
	}
}

// Two Macs at once: both known with their specs, jobs routed by fit, load, what is loaded, follow-ups and pins;
// one list and one event stream with ids that say the Mac; files and job requests reach the right Mac.
func TestSeveralMacs(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	base := srv.URL
	device := signIn(t, base, "laptop")
	miniID, miniTok := enroll(t, base, device, "Mac mini")
	studioID, studioTok := enroll(t, base, device, "Mac Studio")
	mini := startMac(t, base, miniTok, 16)
	studio := startMac(t, base, studioTok, 64)

	// both known, the studio (more memory) is the primary
	var ws map[string]workerItem
	waitFor(t, "both Macs known", func() bool {
		ws = workerItems(t, base, device)
		return ws[miniID].Specs != nil && ws[studioID].Specs != nil
	})
	if ws[miniID].Specs.RAMGB != 16 || ws[studioID].Specs.RAMGB != 64 || !ws[studioID].Primary || ws[miniID].Primary ||
		!ws[miniID].Routes || ws[miniID].Route == ws[studioID].Route {
		t.Fatalf("workers %+v", ws)
	}
	miniRoute, studioRoute := ws[miniID].Route, ws[studioID].Route
	var info struct {
		Worker  workerState  `json:"worker"`
		Workers []workerItem `json:"workers"`
	}
	decode(t, do(t, "GET", base+"/api/info", "", nil), &info)
	if info.Worker.ID != studioID || len(info.Workers) != 2 {
		t.Fatalf("info %+v", info)
	}

	// the event stream, from before the first job
	ev := do(t, "GET", base+"/api/events", device, nil)
	defer ev.Body.Close()
	seen := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(ev.Body)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var u struct{ JobID string }
				json.Unmarshal([]byte(d), &u)
				seen <- u.JobID
			}
		}
	}()

	// both idle, the model on both: the one with more memory
	code, j1, _ := submitJob(t, base, device, `{"model":"small","prompt":"one"}`)
	if code != 200 || !strings.HasPrefix(j1.ID, studioRoute+".") || j1.Worker["id"] != studioID || j1.Worker["name"] != "Mac Studio" {
		t.Fatalf("first job: %d %+v", code, j1)
	}
	waitDone(t, base, device, j1.ID)

	// the studio busy (with a model only it fits, so small is no longer loaded there): the idle mini gets the next
	release := studio.backend.block()
	defer release()
	_, busy, _ := submitJob(t, base, device, `{"model":"big","prompt":"busy"}`)
	if !strings.HasPrefix(busy.ID, studioRoute+".") {
		t.Fatalf("big went to %+v", busy)
	}
	waitFor(t, "the hub sees the studio busy", func() bool {
		st := workerItems(t, base, device)[studioID].State
		return st != nil && st.Running != nil
	})
	_, j2, _ := submitJob(t, base, device, `{"model":"small","prompt":"two"}`)
	if !strings.HasPrefix(j2.ID, miniRoute+".") {
		t.Fatalf("idle Mac not preferred: %+v", j2)
	}
	// a big model fits the studio only, busy or not
	_, j3, _ := submitJob(t, base, device, `{"model":"big","prompt":"three"}`)
	if !strings.HasPrefix(j3.ID, studioRoute+".") {
		t.Fatalf("big model: %+v", j3)
	}
	// a known peak: LTX q4 (~22 GB) does not fit the mini, whatever its size says
	_, jl, _ := submitJob(t, base, device, `{"model":"ltx","prompt":"l"}`)
	if !strings.HasPrefix(jl.ID, studioRoute+".") {
		t.Fatalf("ltx: %+v", jl)
	}
	// nothing fits: 409
	code, _, e := submitJob(t, base, device, `{"model":"huge","prompt":"x"}`)
	if code != 409 || e["error"] != "no-worker" || !strings.Contains(e["message"], "Huge") || !strings.Contains(e["message"], "GB") {
		t.Fatalf("huge: %d %v", code, e)
	}
	// a follow-up goes to its parent's Mac, though another is idle
	_, f, _ := submitJob(t, base, device, `{"model":"small","prompt":"again","parent":"`+j1.ID+`"}`)
	if !strings.HasPrefix(f.ID, studioRoute+".") || f.Request["parent"] != j1.ID {
		t.Fatalf("follow-up: %+v", f)
	}
	release()
	waitDone(t, base, device, j2.ID)

	// the mini has small loaded now, the studio big (the last it ran): small goes to the mini
	waitDone(t, base, device, f.ID)
	_, jb, _ := submitJob(t, base, device, `{"model":"big","prompt":"b"}`)
	waitDone(t, base, device, jb.ID)
	waitFor(t, "the hub sees what is loaded", func() bool {
		ws := workerItems(t, base, device)
		return ws[miniID].State.Loaded["fake"] == "small" && ws[studioID].State.Loaded["fake"] != "small" &&
			ws[studioID].State.Running == nil && ws[studioID].State.Queued == 0
	})
	_, j4, _ := submitJob(t, base, device, `{"model":"small","prompt":"four"}`)
	if !strings.HasPrefix(j4.ID, miniRoute+".") {
		t.Fatalf("loaded model not preferred: %+v", j4)
	}
	// a pin wins over the score
	_, j5, _ := submitJob(t, base, device, `{"model":"small","prompt":"five","worker":"`+studioID+`"}`)
	if !strings.HasPrefix(j5.ID, studioRoute+".") || j5.Request["worker"] != nil {
		t.Fatalf("pinned: %+v", j5)
	}
	j4 = waitDone(t, base, device, j4.ID)

	// files: by the route in their url
	if len(j4.Files) != 1 || !strings.HasPrefix(j4.Files[0], "/files/"+miniRoute+"/") {
		t.Fatalf("files %v", j4.Files)
	}
	r := do(t, "GET", base+j4.Files[0], device, nil)
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || string(b) != "image of four" {
		t.Fatalf("file: %d %q", r.StatusCode, b)
	}
	// a file of one Mac as a reference on another: copied over
	_, j6, _ := submitJob(t, base, device, `{"model":"small","prompt":"six","worker":"`+studioID+`","images":["`+j4.Files[0]+`"]}`)
	img, _ := j6.Request["images"].([]any)
	if !strings.HasPrefix(j6.ID, studioRoute+".") || len(img) != 1 || !strings.HasPrefix(img[0].(string), "/files/"+studioRoute+"/uploads/") {
		t.Fatalf("copied reference: %+v", j6)
	}
	waitDone(t, base, device, j6.ID)

	// job-addressed endpoints reach the Mac of the id
	r = do(t, "POST", base+"/api/jobs/"+j4.ID+"/title", device, strings.NewReader(`{"title":"Four"}`))
	var renamed map[string]any
	decode(t, r, &renamed)
	if renamed["id"] != j4.ID {
		t.Fatalf("rename: %v", renamed)
	}
	if mj, ok := mini.mgr.Job(strings.TrimPrefix(j4.ID, miniRoute+".")); !ok || mj.Request.Title != "Four" {
		t.Fatalf("not renamed on the mini: %+v", mj)
	}

	// one list of both, oldest first
	var list []jobView
	decode(t, do(t, "GET", base+"/api/jobs", device, nil), &list)
	routes := map[string]int{}
	for _, x := range list {
		routes[jobRoute(x.ID)]++
	}
	if routes[miniRoute] == 0 || routes[studioRoute] == 0 || routes[miniRoute]+routes[studioRoute] != len(list) {
		t.Fatalf("merged jobs %v", routes)
	}
	// one stream of both
	got := map[string]bool{}
	timeout := time.After(5 * time.Second)
	for !got[miniRoute] || !got[studioRoute] {
		select {
		case id := <-seen:
			got[jobRoute(id)] = true
		case <-timeout:
			t.Fatalf("events from %v", got)
		}
	}

	// models: each with the Macs that have it and fit it
	var ms struct {
		Models []struct {
			ID      string
			Workers []modelWorker
		}
	}
	decode(t, do(t, "GET", base+"/api/models", device, nil), &ms)
	for _, m := range ms.Models {
		if m.ID != "big" {
			continue
		}
		fit := map[string]bool{}
		for _, w := range m.Workers {
			fit[w.ID] = w.Fits
		}
		if len(m.Workers) != 2 || fit[miniID] || !fit[studioID] {
			t.Fatalf("big: %+v", m.Workers)
		}
	}

	// prompts: written to every Mac with the primary's id
	r = do(t, "POST", base+"/api/prompts", device, strings.NewReader(`{"name":"Mine","use":"chat","text":"be brief"}`))
	var p prompts.Prompt
	decode(t, r, &p)
	if _, ok := mini.prompts.Get(p.ID); !ok {
		t.Fatalf("prompt %q not on the mini", p.ID)
	}
	if _, ok := studio.prompts.Get(p.ID); !ok {
		t.Fatalf("prompt %q not on the studio", p.ID)
	}

	// the primary pinned to the mini
	if r := do(t, "PUT", base+"/api/workers/"+miniID+"/primary", device, nil); r.StatusCode != 200 {
		t.Fatalf("pin: %d", r.StatusCode)
	}
	decode(t, do(t, "GET", base+"/api/info", "", nil), &info)
	if info.Worker.ID != miniID {
		t.Fatalf("pinned primary %+v", info.Worker)
	}
	if r := do(t, "PUT", base+"/api/workers/nope/primary", device, nil); r.StatusCode != 404 {
		t.Fatalf("pin unknown: %d", r.StatusCode)
	}
	do(t, "DELETE", base+"/api/workers/"+miniID+"/primary", device, nil).Body.Close()
	decode(t, do(t, "GET", base+"/api/info", "", nil), &info)
	if info.Worker.ID != studioID {
		t.Fatalf("automatic primary %+v", info.Worker)
	}

	// the mini goes: the studio stays; the mini's jobs say it is offline
	mini.stop()
	waitFor(t, "the mini gone", func() bool { return !workerItems(t, base, device)[miniID].Connected })
	if !workerItems(t, base, device)[studioID].Connected {
		t.Fatal("the studio went with the mini")
	}
	r = do(t, "GET", base+"/api/jobs/"+j4.ID, device, nil)
	decode(t, r, &e)
	if r.StatusCode != 503 || !strings.Contains(e["message"], "Mac mini") {
		t.Fatalf("job of the gone Mac: %d %v", r.StatusCode, e)
	}
	list = nil
	decode(t, do(t, "GET", base+"/api/jobs", device, nil), &list)
	for _, x := range list {
		if jobRoute(x.ID) != studioRoute {
			t.Fatalf("listed %s of the gone Mac", x.ID)
		}
	}
	_, j7, _ := submitJob(t, base, device, `{"model":"small","prompt":"seven"}`)
	if !strings.HasPrefix(j7.ID, studioRoute+".") {
		t.Fatalf("after the mini left: %+v", j7)
	}
	if code, _, e := submitJob(t, base, device, `{"model":"small","prompt":"x","parent":"`+j4.ID+`"}`); code != 503 {
		t.Fatalf("follow-up of a gone Mac's job: %d %v", code, e)
	}
	waitDone(t, base, device, j7.ID)
}

// The legacy shared token is one worker ("legacy", route m0) with routes like any other.
func TestLegacyRouted(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	device := signIn(t, srv.URL, "laptop")
	startMac(t, srv.URL, workerToken, 32)
	waitFor(t, "the legacy Mac known", func() bool { return workerItems(t, srv.URL, device)[tunnel.Legacy].Specs != nil })
	code, j, _ := submitJob(t, srv.URL, device, `{"model":"small","prompt":"x"}`)
	if code != 200 || !strings.HasPrefix(j.ID, legacyRoute+".") || j.Worker["id"] != tunnel.Legacy {
		t.Fatalf("legacy job %d %+v", code, j)
	}
	j = waitDone(t, srv.URL, device, j.ID)
	if r := do(t, "GET", srv.URL+j.Files[0], device, nil); r.StatusCode != 200 {
		t.Fatalf("legacy file %s: %d", j.Files[0], r.StatusCode)
	}
}

// An older mlxlab (no routes in its /api/info) alone gets everything unchanged; beside a newer one it is left out.
func TestOldWorker(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	device := signIn(t, srv.URL, "laptop")
	old := http.NewServeMux()
	old.HandleFunc("GET /api/info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"app": "mlxlab", "role": "worker", "name": "old"})
	})
	old.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(routeHeader) != "" {
			t.Error("an old worker got the route header")
		}
		io.WriteString(w, `[{"id":"plain-1","state":"done","kind":"image","request":{}}]`)
	})
	old.HandleFunc("POST /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"plain-2","state":"queued","kind":"image","request":{}}`)
	})
	connectWorker(t, srv.URL, old)
	var list []jobView
	decode(t, do(t, "GET", srv.URL+"/api/jobs", device, nil), &list)
	if len(list) != 1 || list[0].ID != "plain-1" {
		t.Fatalf("old worker's jobs %+v", list)
	}
	if code, j, _ := submitJob(t, srv.URL, device, `{"model":"anything"}`); code != 200 || j.ID != "plain-2" {
		t.Fatalf("old worker's submit %d %+v", code, j)
	}
	if ws := workerItems(t, srv.URL, device); ws[tunnel.Legacy].Routes || !ws[tunnel.Legacy].Primary {
		t.Fatalf("old worker listed %+v", ws[tunnel.Legacy])
	}

	// a newer Mac joins: it takes the jobs, the old one is left out of the merge
	_, tok := enroll(t, srv.URL, device, "New")
	startMac(t, srv.URL, tok, 32)
	waitFor(t, "the new Mac routed", func() bool {
		list = nil
		decode(t, do(t, "GET", srv.URL+"/api/jobs", device, nil), &list)
		return len(list) == 0
	})
	if code, j, _ := submitJob(t, srv.URL, device, `{"model":"small","prompt":"x"}`); code != 200 || !strings.HasPrefix(j.ID, "m1.") {
		t.Fatalf("new Mac's submit %d %+v", code, j)
	} else {
		waitDone(t, srv.URL, device, j.ID)
	}
}

// Uploads go to the primary (or ?worker=); multipart passes through untouched.
func TestUploadToPrimary(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	device := signIn(t, srv.URL, "laptop")
	aID, aTok := enroll(t, srv.URL, device, "A")
	_, bTok := enroll(t, srv.URL, device, "B")
	startMac(t, srv.URL, aTok, 16)
	startMac(t, srv.URL, bTok, 32)
	waitFor(t, "both known", func() bool {
		n := 0
		for _, w := range workerItems(t, srv.URL, device) {
			if w.Specs != nil {
				n++
			}
		}
		return n == 2
	})
	upload := func(q string) string {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", "x.png")
		fw.Write([]byte("png"))
		mw.Close()
		req, _ := http.NewRequest("POST", srv.URL+"/api/upload"+q, &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+device)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out struct{ URL string }
		decode(t, r, &out)
		return out.URL
	}
	if u := upload(""); !strings.HasPrefix(u, "/files/m2/") {
		t.Fatalf("upload to the primary: %q", u)
	}
	if u := upload("?worker=" + aID); !strings.HasPrefix(u, "/files/m1/") {
		t.Fatalf("upload to A: %q", u)
	}
}
