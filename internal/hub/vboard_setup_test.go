package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/web"
)

// Setting up models from vboard (docs/api.md): the hello's setup, the model list, downloads with progress, what
// only the person can do (needs), cancel, the setup page.

// setupAnswer: a message of the setup protocol for the tests of this file (their requests' ids start with "d_").
func setupAnswer(typ, id string, _ *fakeBridge) bool {
	switch typ {
	case "setup", "models", "downloadStatus", "downloadDone", "needs":
		return true
	case "error":
		return strings.HasPrefix(id, "d_")
	}
	return false
}

// setupMsg is a message of the setup protocol as vboard reads it.
type vbSetupMsg struct {
	Type    string           `json:"type"`
	ID      string           `json:"id"`
	Setup   map[string]any   `json:"setup"`
	Models  []map[string]any `json:"models"`
	Model   string           `json:"model"`
	State   string           `json:"state"`
	Bytes   int64            `json:"bytes"`
	Total   int64            `json:"total"`
	Need    string           `json:"need"`
	URL     string           `json:"url"`
	Message string           `json:"message"`
	Worker  string           `json:"worker"`
}

// nextSetup is the next setup message (of the request id, or any {type:"setup"} for "").
func (b *fakeBridge) nextSetup(t *testing.T, id string) vbSetupMsg {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case raw := <-b.setups:
			var m vbSetupMsg
			json.Unmarshal(raw, &m)
			if id == "" && m.Type == "setup" || id != "" && m.ID == id {
				return m
			}
		case <-deadline:
			t.Fatalf("no setup message for %q", id)
		}
	}
}

func (b *fakeBridge) send(t *testing.T, msg string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
}

// helloSetup is the hello's setup.
func (b *fakeBridge) helloSetup(t *testing.T) map[string]any {
	t.Helper()
	var h struct {
		Setup map[string]any `json:"setup"`
	}
	json.Unmarshal(b.raw, &h)
	if h.Setup == nil {
		t.Fatalf("no setup in the hello %s", b.raw)
	}
	return h.Setup
}

// setupWorld points the Hugging Face token at a file of the test, which is empty, and the access check at a fake
// Hugging Face (setAccess: what it answers for a repository; 200 when not set). The worker has 64 GB.
func setupWorld(t *testing.T) (tokenFile string, setAccess func(repo string, code int)) {
	t.Helper()
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HUGGING_FACE_HUB_TOKEN", "")
	t.Setenv("HF_HOME", "")
	tokenFile = filepath.Join(t.TempDir(), "hf", "token")
	t.Setenv("HF_TOKEN_PATH", tokenFile)
	t.Setenv("MLXLAB_FAKE_RAM_GB", "64")
	var amu sync.Mutex
	access := map[string]int{}
	hf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/models/"), "/auth-check")
		amu.Lock()
		code := access[repo]
		amu.Unlock()
		if code == 0 {
			code = 200
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(hf.Close)
	old := engine.HFEndpoint
	engine.HFEndpoint = hf.URL
	t.Cleanup(func() { engine.HFEndpoint = old })
	return tokenFile, func(repo string, code int) {
		amu.Lock()
		access[repo] = code
		amu.Unlock()
	}
}

// setupMac is a worker that has nothing yet: a chat model, the embedder, the classifier and a gated image model
// to download (the network is a fake: release lets a download finish).
type setupMac struct {
	cat     *engine.Catalog
	dl      *engine.Downloads
	free    int64
	fmu     sync.Mutex
	release chan struct{}
}

func startSetupMac(t *testing.T, base, token string, extra ...*engine.Model) *setupMac {
	t.Helper()
	ms := []*engine.Model{
		{ID: "qwen3.5-4b", Name: "Qwen3.5 4B", Kind: engine.Chat, Backend: "vbchat", Size: 3 * gib},
		{ID: "qwen3.6-35b-a3b", Name: "Qwen3.6", Kind: engine.Chat, Backend: "vbchat", Size: 20 * gib},
		{ID: "qwen3-embedding-0.6b", Name: "Embedder", Kind: engine.Embed, Backend: "vbtext", Size: gib},
		{ID: "laya-typed-decisions", Name: "Laya", Kind: engine.Classify, Backend: "vbtext", Size: gib},
		{ID: "flux2-klein-9b", Name: "FLUX.2 [klein] 9B", Kind: engine.Image, Backend: "vbchat", Size: 35 * gib, Gated: true},
	}
	for _, m := range ms {
		m.SetRepo("org/"+m.ID, "rev")
	}
	ms = append(ms, extra...)
	mac := &setupMac{cat: &engine.Catalog{Models: ms}, free: 500 * gib, release: make(chan struct{}, 16)}
	mac.dl = &engine.Downloads{Catalog: mac.cat, Store: &models.Store{Dir: t.TempDir()},
		Free: func() int64 {
			mac.fmu.Lock()
			defer mac.fmu.Unlock()
			return mac.free
		},
		Fetch: func(ctx context.Context, m *engine.Model, report func(done, total int64)) error {
			report(m.Size/4, 0)
			select {
			case <-mac.release:
			case <-ctx.Done():
				return ctx.Err()
			}
			report(m.Size, 0)
			return nil
		}}
	dir := t.TempDir()
	mgr := jobs.New(mac.cat, []engine.Backend{&vbChat{}, &vbText{}}, dir)
	api := (&web.Server{Jobs: mgr, Prompts: prompts.Open(dir), Version: "test", Downloads: mac.dl}).Handler()
	stop, _ := connectWorkerWith(t, base, token, withRAM(api, 64))
	t.Cleanup(func() {
		stop()
		for _, m := range ms {
			mac.dl.Cancel(m.ID)
		}
	})
	return mac
}

func (m *setupMac) setFree(n int64) {
	m.fmu.Lock()
	m.free = n
	m.fmu.Unlock()
}

// A connector whose owner's worker has no model yet dials anyway, with the setup; the owner downloads a model
// from vboard (progress, done, then offered by a handover), others are sent to the owner; cancel; the model list.
func TestVboardSetupDownloads(t *testing.T) {
	fastBridges(t)
	poll := bridgeDownloadPoll
	bridgeDownloadPoll = 50 * time.Millisecond
	t.Cleanup(func() { bridgeDownloadPoll = poll })
	_, setAccess := setupWorld(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	up := &engine.Model{ID: "seedvr2-3b", Name: "SeedVR2 3B", Kind: engine.Image, Backend: "vbchat", Size: 7 * gib, Tasks: []string{"upscale"}}
	up.SetRepo("org/seedvr2-3b", "rev")
	mac := startSetupMac(t, base, wtok, up)
	v := newFakeVboard(t, stageImages)
	c := addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret"}`)

	// dials with nothing to offer: the setup asks
	b := v.next(t)
	if b.hello.Model != "" || b.hello.Embed != "" || b.hello.Chat != "" || len(b.agents) != 0 {
		t.Fatalf("hello %+v %s", b.hello, b.raw)
	}
	st := b.helloSetup(t)
	if st["downloads"] != true || st["owner"] != "u_1" || st["memory"] != float64(64<<30) || st["free"] != float64(500<<30) ||
		st["huggingFace"].(map[string]any)["token"] != false || st["setupUrl"] != nil {
		t.Fatalf("setup %v", st)
	}
	waitState(t, base, device, c.ID, "connected")

	// the model list: everything of the setup's kinds, with the worker; the chat model for 64 GB recommended
	b.send(t, `{"type":"models","id":"d_1"}`)
	list := b.nextSetup(t, "d_1")
	if list.Type != "models" || len(list.Models) != 6 {
		t.Fatalf("models %+v", list)
	}
	byID := map[string]map[string]any{}
	for _, m := range list.Models {
		byID[m["id"].(string)] = m
		if m["worker"] != "Mac" || m["here"] != false || m["fits"] != true {
			t.Fatalf("entry %v", m)
		}
	}
	if byID["qwen3.6-35b-a3b"]["recommended"] != true || byID["qwen3.5-4b"]["recommended"] != false ||
		byID["qwen3-embedding-0.6b"]["recommended"] != true {
		t.Fatalf("recommended %v", list.Models)
	}
	// the upscaler is recommended among the upscalers (image/upscale), not against the image generators
	if byID["seedvr2-3b"]["recommended"] != true {
		t.Fatalf("upscaler recommended %v", byID["seedvr2-3b"])
	}
	// an upscaler says so (tasks), the others have none
	if tk, _ := byID["seedvr2-3b"]["tasks"].([]any); len(tk) != 1 || tk[0] != "upscale" || byID["flux2-klein-9b"]["tasks"] != nil {
		t.Fatalf("tasks %v", list.Models)
	}
	if f := byID["flux2-klein-9b"]; f["gated"] != true || f["access"] != "token" || f["url"] != "https://huggingface.co/org/flux2-klein-9b" ||
		f["nonCommercial"] != true || f["licence"] != "FLUX Non-Commercial License" {
		t.Fatalf("flux %v", f)
	}

	// someone else on the board: sent to the owner
	b.send(t, `{"type":"download","id":"d_2","model":"qwen3-embedding-0.6b","context":{"userId":"u_2","boardId":"b_1"}}`)
	if m := b.nextSetup(t, "d_2"); m.Type != "needs" || m.Need != "owner" || !strings.Contains(m.Message, "Ask Anna") {
		t.Fatalf("not the owner %+v", m)
	}
	b.send(t, `{"type":"download","id":"d_2b","model":"qwen3-embedding-0.6b"}`) // nobody named: nobody's
	if m := b.nextSetup(t, "d_2b"); m.Type != "needs" || m.Need != "owner" {
		t.Fatalf("nobody %+v", m)
	}

	// the owner: progress, done, then the embedder is offered (a handover: vboards learn models from hello)
	b.send(t, `{"type":"download","id":"d_3","model":"qwen3-embedding-0.6b","context":{"userId":"u_1"}}`)
	m := b.nextSetup(t, "d_3")
	for m.Type == "downloadStatus" && m.Bytes == 0 {
		m = b.nextSetup(t, "d_3")
	}
	if m.Type != "downloadStatus" || m.State != "downloading" || m.Bytes != gib/4 || m.Total != gib || m.Worker != "Mac" {
		t.Fatalf("status %+v", m)
	}
	mac.release <- struct{}{}
	for m.Type == "downloadStatus" {
		m = b.nextSetup(t, "d_3")
	}
	if m.Type != "downloadDone" || m.Model != "qwen3-embedding-0.6b" {
		t.Fatalf("done %+v", m)
	}
	b2 := v.next(t)
	if b2.hello.Embed != "qwen3-embedding-0.6b" || b2.hello.Instance != b.hello.Instance {
		t.Fatalf("handover hello %+v", b2.hello)
	}
	b = b2

	// asked again (a reconnect): the same download, done at once
	b.send(t, `{"type":"download","id":"d_4","model":"qwen3-embedding-0.6b","userId":"u_1"}`)
	if m := b.nextSetup(t, "d_4"); m.Type != "downloadDone" {
		t.Fatalf("again %+v", m)
	}

	// cancel: nothing more is sent, the worker stops it (the partial files stay)
	b.send(t, `{"type":"download","id":"d_5","model":"qwen3.6-35b-a3b","context":{"userId":"u_1"}}`)
	if m := b.nextSetup(t, "d_5"); m.Type != "downloadStatus" {
		t.Fatalf("status %+v", m)
	}
	b.send(t, `{"type":"downloadCancel","id":"d_5"}`)
	waitFor(t, "the worker's download canceled", func() bool {
		s, _ := mac.dl.Status("qwen3.6-35b-a3b")
		return s.State == engine.DownloadCanceled
	})
	time.Sleep(200 * time.Millisecond)
	for len(b.setups) > 0 {
		var x vbSetupMsg
		json.Unmarshal(<-b.setups, &x)
		if x.ID == "d_5" && x.Type != "downloadStatus" {
			t.Fatalf("after cancel %+v", x)
		}
	}

	// a gated model: no token → needs token; a token, licence not accepted → needs licence with the page
	b.send(t, `{"type":"download","id":"d_6","model":"flux2-klein-9b","context":{"userId":"u_1"}}`)
	if m := b.nextSetup(t, "d_6"); m.Type != "needs" || m.Need != "token" {
		for m.Type == "downloadStatus" {
			m = b.nextSetup(t, "d_6")
		}
		if m.Type != "needs" || m.Need != "token" {
			t.Fatalf("token %+v", m)
		}
	}
	if err := models.SaveHFToken("hf_testtoken"); err != nil {
		t.Fatal(err)
	}
	if m := b.nextSetup(t, ""); m.Setup["huggingFace"].(map[string]any)["token"] != true {
		t.Fatalf("setup after the token %+v", m)
	}
	setAccess("org/flux2-klein-9b", 403)
	b.send(t, `{"type":"download","id":"d_7","model":"flux2-klein-9b","context":{"userId":"u_1"}}`)
	m = b.nextSetup(t, "d_7")
	for m.Type == "downloadStatus" {
		m = b.nextSetup(t, "d_7")
	}
	if m.Type != "needs" || m.Need != "licence" || m.URL != "https://huggingface.co/org/flux2-klein-9b" {
		t.Fatalf("licence %+v", m)
	}

	// not enough disk
	mac.setFree(10 * gib)
	b.send(t, `{"type":"download","id":"d_8","model":"qwen3.6-35b-a3b","context":{"userId":"u_1"}}`)
	m = b.nextSetup(t, "d_8")
	for m.Type == "downloadStatus" {
		m = b.nextSetup(t, "d_8")
	}
	if m.Type != "needs" || m.Need != "space" || !strings.Contains(m.Message, "free") {
		t.Fatalf("space %+v", m)
	}
	// and a model the list calls too large for the disk does not fit
	b.send(t, `{"type":"models","id":"d_9"}`)
	for _, e := range b.nextSetup(t, "d_9").Models {
		if e["id"] == "qwen3.6-35b-a3b" && e["fits"] != false {
			t.Fatalf("fits %v", e)
		}
	}
	// an unknown model
	b.send(t, `{"type":"download","id":"d_10","model":"nope","context":{"userId":"u_1"}}`)
	if m := b.nextSetup(t, "d_10"); m.Type != "error" || !strings.Contains(m.Message, "no such model") {
		t.Fatalf("unknown %+v", m)
	}
}

// The standalone hub's setup page: only with its code, the token saved where `hf auth login` keeps it (0600), then
// vboard is told (setup.huggingFace.token).
func TestSetupPage(t *testing.T) {
	fastBridges(t)
	tokenFile, _ := setupWorld(t)
	s, err := New(Config{Password: password, Data: t.TempDir(), Tools: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	l := httptest.NewUnstartedServer(nil)
	base := "http://" + l.Listener.Addr().String()
	page := s.EnableSetupPage(base)
	l.Config.Handler = s.Handler()
	l.Start()
	t.Cleanup(l.Close)
	u, _ := url.Parse(page)
	code := u.Query().Get("code")
	if !strings.HasPrefix(page, base+"/setup?code=") || len(code) < 24 {
		t.Fatalf("page %q", page)
	}

	for _, c := range []string{"", "wrong", code + "x"} {
		r, _ := http.Get(base + "/setup?code=" + c)
		r.Body.Close()
		if r.StatusCode != 403 {
			t.Fatalf("code %q: %d", c, r.StatusCode)
		}
	}
	r, _ := http.Get(page)
	body := readAll(t, r)
	if r.StatusCode != 200 || !strings.Contains(body, `name="token"`) || strings.Contains(body, "<script") ||
		strings.Contains(body, "src=") || r.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("page %d %s", r.StatusCode, body)
	}
	post := func(form url.Values) (int, string) {
		r, err := http.PostForm(base+"/setup", form)
		if err != nil {
			t.Fatal(err)
		}
		return r.StatusCode, readAll(t, r)
	}
	if c, _ := post(url.Values{"code": {"wrong"}, "token": {"hf_abcdefgh"}}); c != 403 {
		t.Fatalf("wrong code: %d", c)
	}
	if c, _ := post(url.Values{"code": {code}, "token": {"not a token"}}); c != 400 {
		t.Fatalf("bad token: %d", c)
	}
	if _, err := os.Stat(tokenFile); err == nil {
		t.Fatal("a token saved")
	}

	// a vboard connected to this hub's worker learns the page, and that a token was set
	_, wtok := mustWorker(t, s)
	startSetupMac(t, base, wtok)
	v := newFakeVboard(t, stageImages)
	if _, err := s.EnsureConnector(ConnectorSettings{Name: "Mac (ai-connector)", URL: v.srv.URL, Token: "vbp_secret", Owner: "local"}); err != nil {
		t.Fatal(err)
	}
	b := v.next(t)
	if st := b.helloSetup(t); st["setupUrl"] != page || st["huggingFace"].(map[string]any)["token"] != false {
		t.Fatalf("setup %v", st)
	}
	if c, body := post(url.Values{"code": {code}, "token": {" hf_abcdefgh \n"}}); c != 200 || !strings.Contains(body, "connected") {
		t.Fatalf("save: %d %s", c, body)
	}
	st, err := os.Stat(tokenFile)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file %v %v", st, err)
	}
	if b, _ := os.ReadFile(tokenFile); string(b) != "hf_abcdefgh" || models.HFToken() != "hf_abcdefgh" {
		t.Fatalf("token %q", b)
	}
	if m := b.nextSetup(t, ""); m.Setup["huggingFace"].(map[string]any)["token"] != true || m.Setup["setupUrl"] != page {
		t.Fatalf("setup update %+v", m)
	}
}

func readAll(t *testing.T, r *http.Response) string {
	t.Helper()
	defer r.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String()
		}
	}
}

// mustWorker is the standalone hub's local worker.
func mustWorker(t *testing.T, s *Server) (string, string) {
	t.Helper()
	tok, err := s.LocalWorker("Mac", "local")
	if err != nil {
		t.Fatal(err)
	}
	return "Mac", tok
}

// Claude Code is the default while there is no chat model at all (the first agent's model left to the default chat
// model, as `ai-connector vboard` sets it when Claude Code is ready); a chat model downloaded goes first.
func TestVboardClaudeFirstUntilAChatModel(t *testing.T) {
	fastBridges(t)
	poll := bridgeDownloadPoll
	bridgeDownloadPoll = 50 * time.Millisecond
	t.Cleanup(func() { bridgeDownloadPoll = poll })
	setupWorld(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	mac := startSetupMac(t, base, wtok, &engine.Model{ID: "claude-code/sonnet", Name: "Claude Code · Sonnet", Kind: engine.Chat, Backend: "vbchat", Here: true})
	v := newFakeVboard(t, stageImages)
	addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret","agents":[{"model":""},{"model":"claude-code/sonnet"}]}`)
	b := v.next(t)
	if b.hello.Chat != "" || len(b.agents) != 1 || b.agents[0].ID != "claude-code-sonnet" || b.agents[0].Kind != "agent" {
		t.Fatalf("no chat model: %+v %+v", b.hello, b.agents)
	}
	b.send(t, `{"type":"download","id":"d_1","model":"qwen3.5-4b","context":{"userId":"u_1"}}`)
	mac.release <- struct{}{}
	m := b.nextSetup(t, "d_1")
	for m.Type == "downloadStatus" {
		m = b.nextSetup(t, "d_1")
	}
	if m.Type != "downloadDone" {
		t.Fatalf("done %+v", m)
	}
	b = v.next(t)
	if b.hello.Chat != "qwen3.5-4b" || len(b.agents) != 2 || b.agents[0].ID != "default" || b.agents[0].Kind != "model" || b.agents[1].Kind != "agent" {
		t.Fatalf("with a chat model: %+v %+v", b.hello, b.agents)
	}
}

// The oldest vboard takes a hello with nothing to offer (its fields empty; the setup ignored), and the embedder
// arrives by handover once a download vboard did not start is done (the first start's).
func TestVboardSetupOldVboard(t *testing.T) {
	fastBridges(t)
	setupWorld(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	mac := startSetupMac(t, base, wtok)
	v := newFakeVboard(t, stageClassify)
	c := addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret"}`)
	b := v.next(t)
	if b.hello.Model != "" || b.hello.Name != "ai-connector" {
		t.Fatalf("hello %+v", b.hello)
	}
	waitState(t, base, device, c.ID, "connected")
	if res := b.ask(t, "classify", `"states":[{"a":"b"}],"instructions":"x","options":{"a":"b"}`); res.Error == "" {
		t.Fatalf("classify without a model %+v", res)
	}
	// the worker downloads the classifier by itself (as `ai-connector vboard` does on its first start)
	if _, err := mac.dl.Start("laya-typed-decisions", 64*gib); err != nil {
		t.Fatal(err)
	}
	mac.release <- struct{}{}
	b2 := v.next(t)
	if b2.hello.Model != "laya-typed-decisions" {
		t.Fatalf("hello after the download %+v", b2.hello)
	}
}

// A speech model downloaded from vboard (with generators "*", as `ai-connector vboard` sets it) is offered as a
// generator at once: {type:"generators"} on the open connection after downloadDone. The list has music, sound and
// speech models with a recommendation per kind.
func TestVboardSetupAudioGenerator(t *testing.T) {
	fastBridges(t)
	fastGenerate(t)
	poll := bridgeDownloadPoll
	bridgeDownloadPoll = 50 * time.Millisecond
	t.Cleanup(func() { bridgeDownloadPoll = poll })
	setupWorld(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	kokoro := &engine.Model{ID: "kokoro-82m", Name: "Kokoro 82M", Kind: engine.Speech, Backend: "speech", Size: gib / 2, Audio: kokoroCaps,
		Tasks: []string{"presets", "speed", "language"}}
	qwen := &engine.Model{ID: "qwen3-tts-1.7b", Name: "Qwen3-TTS 1.7B", Kind: engine.Speech, Backend: "speech", Size: 12 * gib, Audio: qwenCaps}
	asr := &engine.Model{ID: "qwen3-asr", Name: "Qwen3-ASR", Kind: engine.Speech, Backend: "speech", Size: 2 * gib, Tasks: []string{"transcribe"}}
	song := &engine.Model{ID: "ace-step-1.5-turbo", Name: "ACE-Step 1.5 turbo", Kind: engine.Music, Backend: "music", Size: 10 * gib,
		Defaults: engine.Request{Duration: 60}, Audio: &engine.AudioCaps{MaxSeconds: 600, Lyrics: true, Instrumental: true, BPM: true}}
	sound := &engine.Model{ID: "moss-soundeffect", Name: "MOSS-SoundEffect", Kind: engine.Sound, Backend: "sound", Size: 7 * gib,
		Defaults: engine.Request{Duration: 8}, Audio: &engine.AudioCaps{MaxSeconds: 30}}
	for _, m := range []*engine.Model{kokoro, qwen, asr, song, sound} {
		m.SetRepo("org/"+m.ID, "rev")
	}
	mac := startSetupMac(t, base, wtok, kokoro, qwen, asr, song, sound)
	v := newFakeVboard(t, stageGenerators)
	addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret","generators":["*"]}`)
	b := v.next(t)
	if gs := b.helloGenerators(); len(gs) != 0 {
		t.Fatalf("generators before the download %+v", gs)
	}

	b.send(t, `{"type":"models","id":"d_1"}`)
	rec := map[string]bool{}
	for _, e := range b.nextSetup(t, "d_1").Models {
		if e["recommended"] == true {
			rec[e["id"].(string)] = true
		}
	}
	if !rec["kokoro-82m"] || rec["qwen3-tts-1.7b"] || rec["qwen3-asr"] || !rec["ace-step-1.5-turbo"] || !rec["moss-soundeffect"] {
		t.Fatalf("recommended %v", rec)
	}

	b.send(t, `{"type":"download","id":"d_2","model":"kokoro-82m","context":{"userId":"u_1"}}`)
	mac.release <- struct{}{}
	m := b.nextSetup(t, "d_2")
	for m.Type == "downloadStatus" {
		m = b.nextSetup(t, "d_2")
	}
	if m.Type != "downloadDone" || m.Model != "kokoro-82m" {
		t.Fatalf("done %+v", m)
	}
	gs := nextGenerators(t, b)
	if len(gs) != 1 || gs[0].ID != "kokoro-82m" || gs[0].Kind != "speech" || gs[0].Worker != "Mac" {
		t.Fatalf("generators after the download %+v", gs)
	}
	if _, dials := v.count(); dials != 1 {
		t.Fatalf("%d connections: a generator must not force a handover", dials)
	}
}
