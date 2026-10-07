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
)

// fakeSpeech transcribes every sample the same, and records the audio of the jobs it runs.
type fakeSpeech struct{ audio chan string }

func (fakeSpeech) Name() string   { return "speech" }
func (fakeSpeech) Heavy() bool    { return false }
func (fakeSpeech) Loaded() string { return "" }
func (fakeSpeech) Unload()        {}
func (f fakeSpeech) Run(_ context.Context, _ *engine.Model, r engine.Request, _ string, emit engine.Emit) error {
	f.audio <- r.Audio
	emit(engine.Event{Type: "text", Text: "hallo"})
	return nil
}
func (fakeSpeech) Transcribe(_ context.Context, file, language string) (string, error) {
	return "transcribed (" + language + ")", nil
}

func TestVoicesAPI(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	cat := &engine.Catalog{Models: []*engine.Model{{ID: "qwen3-asr", Kind: engine.Speech, Backend: "speech"}}}
	fs := fakeSpeech{audio: make(chan string, 1)}
	m := jobs.New(cat, []engine.Backend{fs}, dir)
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
	type list struct {
		Voices  []engine.Voice      `json:"voices"`
		Presets map[string][]string `json:"presets"`
	}
	var l list
	if c := do("GET", "/api/voices", "", nil, &l); c != 200 || len(l.Voices) != 0 || len(l.Presets["voxtral-tts"]) != 20 {
		t.Fatalf("empty list: %d %+v", c, l)
	}

	// clone: multipart with a sample, no transcript → transcribed by the speech backend
	tone := filepath.Join(dir, "tone.wav")
	if out, err := exec.Command("ffmpeg", "-y", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=330:duration=8", tone).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("name", "Probe")
	_ = mw.WriteField("language", "de")
	fw, _ := mw.CreateFormFile("sample", "tone.wav")
	b, _ := os.ReadFile(tone)
	_, _ = fw.Write(b)
	mw.Close()
	var clone engine.Voice
	if c := do("POST", "/api/voices", mw.FormDataContentType(), &body, &clone); c != 200 {
		t.Fatalf("clone: %d", c)
	}
	if clone.Kind != "clone" || clone.Transcript != "transcribed (de)" || clone.Sample != "/files/voices/"+clone.ID+".wav" {
		t.Fatalf("clone %+v", clone)
	}
	res, err := http.Get(srv.URL + clone.Sample)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("sample: %v %v", err, res.StatusCode)
	}
	res.Body.Close()
	if c := do("GET", "/files/voices/voices.json", "", nil, nil); c != 404 {
		t.Fatalf("voices.json served: %d", c)
	}
	if c := do("GET", "/files/voices/..%2f..%2fhistory.json", "", nil, nil); c != 404 {
		t.Fatalf("escape: %d", c)
	}
	// a multipart without a sample
	var empty bytes.Buffer
	mw2 := multipart.NewWriter(&empty)
	_ = mw2.WriteField("name", "x")
	mw2.Close()
	if c := do("POST", "/api/voices", mw2.FormDataContentType(), &empty, nil); c != 400 {
		t.Fatalf("no sample: %d", c)
	}

	// design: JSON
	var design engine.Voice
	if c := do("POST", "/api/voices", "application/json", strings.NewReader(`{"name":"Oma","description":"a calm older woman","language":"de"}`), &design); c != 200 || design.Kind != "design" {
		t.Fatalf("design: %d %+v", c, design)
	}
	if c := do("POST", "/api/voices", "application/json", strings.NewReader(`{"name":"Oma"}`), nil); c != 400 {
		t.Fatalf("design without description: %d", c)
	}

	// edit, delete
	var edited engine.Voice
	if c := do("PATCH", "/api/voices/"+clone.ID, "application/json", strings.NewReader(`{"name":"Probe 2","transcript":"Hallo."}`), &edited); c != 200 ||
		edited.Name != "Probe 2" || edited.Transcript != "Hallo." || edited.Sample != clone.Sample {
		t.Fatalf("edit: %d %+v", c, edited)
	}
	if c := do("PATCH", "/api/voices/nope", "application/json", strings.NewReader(`{"name":"x"}`), nil); c != 404 {
		t.Fatalf("edit unknown: %d", c)
	}
	if c := do("GET", "/api/voices", "", nil, &l); c != 200 || len(l.Voices) != 2 || l.Voices[0].Sample != clone.Sample {
		t.Fatalf("list: %+v", l)
	}

	// a transcription job names the sample by its /files/ url; the backend gets the file
	var j jobs.Job
	if c := do("POST", "/api/jobs", "application/json", strings.NewReader(`{"model":"qwen3-asr","audio":"`+clone.Sample+`"}`), &j); c != 200 {
		t.Fatalf("job: %d", c)
	}
	if j.Request.Audio != clone.Sample {
		t.Fatalf("job view audio %q", j.Request.Audio)
	}
	select {
	case a := <-fs.audio:
		if a != filepath.Join(dir, "voices", clone.ID+".wav") {
			t.Fatalf("backend got %q", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("job did not run")
	}
	if c := do("POST", "/api/jobs", "application/json", strings.NewReader(`{"model":"qwen3-asr","audio":"/files/nope.wav"}`), nil); c != 400 {
		t.Fatalf("unknown audio: %d", c)
	}

	if c := do("DELETE", "/api/voices/"+clone.ID, "", nil, nil); c != 204 {
		t.Fatalf("delete: %d", c)
	}
	if c := do("DELETE", "/api/voices/"+clone.ID, "", nil, nil); c != 404 {
		t.Fatalf("delete twice: %d", c)
	}
	if c := do("GET", clone.Sample, "", nil, nil); c != 404 {
		t.Fatalf("deleted sample served: %d", c)
	}
}
