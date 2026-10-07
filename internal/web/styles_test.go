package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/styles"
)

// fakeMusic records the requests of the jobs it runs; a training waits until release is closed.
type fakeMusic struct {
	reqs    chan engine.Request
	release chan struct{}
}

func (fakeMusic) Name() string   { return "music" }
func (fakeMusic) Heavy() bool    { return true }
func (fakeMusic) Loaded() string { return "" }
func (fakeMusic) Unload()        {}
func (f fakeMusic) Run(ctx context.Context, _ *engine.Model, r engine.Request, _ string, _ engine.Emit) error {
	f.reqs <- r
	if r.Task == engine.TrainStyle {
		select {
		case <-f.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func TestStylesAPI(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	cat := &engine.Catalog{Models: []*engine.Model{
		{ID: "ace-step-1.5-turbo", Name: "ACE-Step 1.5 turbo", Kind: engine.Music, Backend: "music", Styles: true},
		{ID: "yue2-3b", Name: "YuE2", Kind: engine.Music, Backend: "music"},
	}}
	fm := fakeMusic{reqs: make(chan engine.Request, 4), release: make(chan struct{})}
	m := jobs.New(cat, []engine.Backend{fm}, dir)
	t.Cleanup(func() {
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !m.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	srv := httptest.NewServer((&Server{Jobs: m}).Handler())
	defer srv.Close()
	do := func(method, path, ctype string, body io.Reader, out any) int {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, body)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if out != nil && res.StatusCode == http.StatusOK {
			if err := json.NewDecoder(res.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
		return res.StatusCode
	}
	type view struct {
		ID, Name, BaseModel, Trigger, Status, JobID string
		Strength                                    float64
		Segments                                    int
		Seconds, Estimate                           float64
		Tracks                                      []struct{ ID, Name, URL, File string }
		Running                                     *jobs.Job
	}
	var l struct {
		Styles []view
		Models []map[string]any
	}
	if c := do("GET", "/api/styles", "", nil, &l); c != 200 || len(l.Styles) != 0 || len(l.Models) != 1 || l.Models[0]["id"] != "ace-step-1.5-turbo" {
		t.Fatalf("empty: %d %+v", c, l)
	}
	if c := do("POST", "/api/styles", "application/json", strings.NewReader(`{"name":"x","baseModel":"yue2-3b"}`), nil); c != 400 {
		t.Fatalf("a model without styles: %d", c)
	}
	var st view
	if c := do("POST", "/api/styles", "application/json", strings.NewReader(`{"name":"Deep Trance"}`), &st); c != 200 ||
		st.BaseModel != "ace-step-1.5-turbo" || st.Status != "new" || st.Trigger == "" {
		t.Fatalf("created: %d %+v", c, st)
	}
	if c := do("POST", "/api/styles/"+st.ID+"/train", "", nil, nil); c != 400 {
		t.Fatalf("train without tracks: %d", c)
	}

	// two tracks in one upload
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i, secs := range []string{"40", "35"} {
		f := filepath.Join(t.TempDir(), "t.wav")
		if out, err := exec.Command("ffmpeg", "-y", "-v", "error", "-f", "lavfi", "-i", "sine=frequency="+[]string{"220", "330"}[i]+":duration="+secs, f).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		b, _ := os.ReadFile(f)
		fw, _ := mw.CreateFormFile("file", []string{"one.wav", "two.flac"}[i])
		fw.Write(b)
	}
	mw.Close()
	if c := do("POST", "/api/styles/"+st.ID+"/tracks", mw.FormDataContentType(), &buf, &st); c != 200 || len(st.Tracks) != 2 ||
		st.Status != "ready" || st.Segments != 3 || st.Estimate <= 0 {
		t.Fatalf("tracks: %d %+v", c, st)
	}
	tr := st.Tracks[0]
	if tr.File != "" || !strings.HasPrefix(tr.URL, "/files/styles/"+st.ID+"/tracks/") {
		t.Fatalf("track %+v", tr)
	}
	// the track plays; the style's weights and style.json are not files to fetch
	res, err := http.Get(srv.URL + tr.URL)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("track file: %v %v", err, res.StatusCode)
	}
	res.Body.Close()
	for _, p := range []string{"/files/styles/" + st.ID + "/style.json", "/files/styles/../history.json"} {
		if res, _ := http.Get(srv.URL + p); res.StatusCode != 404 {
			t.Errorf("%s: %d", p, res.StatusCode)
		}
	}

	// rename and strength
	if c := do("PATCH", "/api/styles/"+st.ID, "application/json", strings.NewReader(`{"name":"Trance","strength":0.8}`), &st); c != 200 ||
		st.Name != "Trance" || st.Strength != 0.8 {
		t.Fatalf("patch: %d %+v", c, st)
	}
	if c := do("PATCH", "/api/styles/"+st.ID, "application/json", strings.NewReader(`{"strength":5}`), nil); c != 400 {
		t.Fatalf("strength 5: %d", c)
	}

	// train: a job of the base model with the task and the style; meanwhile the style is busy
	var tj struct {
		Style view
		Job   jobs.Job
	}
	if c := do("POST", "/api/styles/"+st.ID+"/train", "application/json", strings.NewReader(`{"steps":300}`), &tj); c != 200 ||
		tj.Job.Model != "ace-step-1.5-turbo" || tj.Job.Request.Task != engine.TrainStyle || tj.Job.Request.Style != st.ID ||
		tj.Style.JobID != tj.Job.ID {
		t.Fatalf("train: %d %+v", c, tj)
	}
	r := <-fm.reqs
	if r.Style != st.ID {
		t.Fatalf("job request %+v", r)
	}
	if c := do("GET", "/api/styles/"+st.ID, "", nil, &st); c != 200 || st.Status != "training" || st.Running == nil {
		t.Fatalf("while training: %d %+v", c, st)
	}
	if got, _ := styles.Open(filepath.Join(dir, "styles")).Get(st.ID); got.Settings.Steps != 300 {
		t.Fatalf("steps %+v", got.Settings)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/styles/" + st.ID + "/train"}, {"DELETE", "/api/styles/" + st.ID},
		{"DELETE", "/api/styles/" + st.ID + "/tracks/" + tr.ID},
	} {
		if code := do(c.method, c.path, "", nil, nil); code != 409 {
			t.Errorf("%s %s while training: %d", c.method, c.path, code)
		}
	}
	close(fm.release)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !m.Settled(); time.Sleep(5 * time.Millisecond) {
	}

	// a song with the style is an ordinary job (the backend checks the style)
	var j jobs.Job
	if c := do("POST", "/api/jobs", "application/json", strings.NewReader(`{"model":"ace-step-1.5-turbo","prompt":"x","style":"`+st.ID+`","styleStrength":0.5}`), &j); c != 200 {
		t.Fatalf("song: %d", c)
	}
	if r := <-fm.reqs; r.Style != st.ID || r.StyleStrength != 0.5 {
		t.Fatalf("song request %+v", r)
	}

	if c := do("DELETE", "/api/styles/"+st.ID+"/tracks/"+tr.ID, "", nil, &st); c != 200 || len(st.Tracks) != 1 {
		t.Fatalf("delete track: %d %+v", c, st)
	}
	if c := do("DELETE", "/api/styles/"+st.ID, "", nil, nil); c != 204 {
		t.Fatalf("delete: %d", c)
	}
	if c := do("GET", "/api/styles/"+st.ID, "", nil, nil); c != 404 {
		t.Fatalf("deleted: %d", c)
	}
}
