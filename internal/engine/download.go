package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/llm"
	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/pyenv"
)

// Downloads fetch catalogue models when asked (vboard's Download, the vboard connector's first start), one at a time,
// beside the job queue: a download does not stop a chat. The files go where a job would fetch them on first use (the
// store; a stopped download resumes where it stopped), then the Python runtime of the model's backend is made, and
// what the runtime builds or fetches once (LeVo2.cpp, Stable Audio 3's code) — preparing. After that the first job
// starts without fetching anything. A download runs on its own: whoever started it only asks for its state, and
// asking again joins it.

// Download states.
const (
	DownloadQueued      = "queued"
	DownloadDownloading = "downloading"
	DownloadVerifying   = "verifying"
	DownloadPreparing   = "preparing"
	DownloadDone        = "done"
	DownloadFailed      = "failed"   // Error says why; Need when only the person can help
	DownloadCanceled    = "canceled" // the partial files stay
)

// What only the person can do about a download that failed (DownloadState.Need).
const (
	NeedLicence = "licence" // accept the licence on URL (Hugging Face has no API for it)
	NeedToken   = "token"   // no (valid) Hugging Face token here
	NeedSpace   = "space"   // not enough free disk
	NeedMemory  = "memory"  // the model does not fit this machine's memory
)

// DownloadState is what a download does now.
type DownloadState struct {
	Model string `json:"model"`
	State string `json:"state"`
	Bytes int64  `json:"bytes,omitempty"` // fetched of Total
	Total int64  `json:"total,omitempty"` // bytes to fetch (what was missing when it started)
	ETA   int    `json:"eta,omitempty"`   // seconds still, when it can tell
	Error string `json:"error,omitempty"`
	Need  string `json:"need,omitempty"`
	URL   string `json:"url,omitempty"` // need licence: where to accept it
}

// Active: it still runs (or waits to).
func (s DownloadState) Active() bool {
	switch s.State {
	case DownloadQueued, DownloadDownloading, DownloadVerifying, DownloadPreparing:
		return true
	}
	return false
}

// ErrNoModel: the catalogue has no model of that id.
var ErrNoModel = errors.New("no such model")

// HFEndpoint is Hugging Face (tests point it elsewhere): access to gated repositories is asked there.
var HFEndpoint = "https://huggingface.co"

// diskMargin is kept free beside a download.
const diskMargin = 512 << 20

// Downloads keeps the downloads of one worker.
type Downloads struct {
	Catalog *Catalog
	Store   *models.Store
	Root    string // the config dir: the Python runtimes (pyenv)
	// Free is the free disk where the models go (nil: the store's file system).
	Free func() int64
	// Fetch replaces the network (tests): it reports the bytes as they come and returns when the files are there.
	Fetch func(ctx context.Context, m *Model, report func(done, total int64)) error
	// Prepare replaces making the runtime (tests): env is the Python runtime's name ("" none), build whether the
	// runtime has a step of its own. nil with Fetch set: nothing is prepared.
	Prepare func(ctx context.Context, m *Model, env string, build bool) error
	// Changed is told the new state of a download (nil: nobody); a download's progress tells it often.
	Changed func(DownloadState)

	mu    sync.Mutex
	runs  map[string]*download // by model id: the newest download of each
	queue []*download
	wake  chan struct{}
	once  sync.Once

	// the states for Changed, in the order they happened (one goroutine delivers them)
	noteMu sync.Mutex
	notes  []DownloadState
	noting bool
	access map[string]accessSeen
}

type download struct {
	m      *Model
	st     DownloadState
	ctx    context.Context
	cancel context.CancelFunc
	began  time.Time // the bytes started to come (ETA)
	from   int64     // the bytes when they started
}

type accessSeen struct {
	access string
	at     time.Time
}

// accessTTL: how long an answer of Hugging Face about a gated repository is trusted (a download asks again).
var accessTTL = 5 * time.Minute

// RepoURL is the Hugging Face page of a catalogue model ("" none).
func RepoURL(m *Model) string {
	if r := repoOf(m); r != "" {
		return "https://huggingface.co/" + r
	}
	return ""
}

func repoOf(m *Model) string {
	switch {
	case m.repo != "":
		return m.repo
	case m.chat != nil:
		return m.chat.Repo
	case m.pinned != nil:
		return m.pinned.Repo
	case len(m.repos) > 0: // music, sound, speech: the first (the model's own)
		return m.repos[0].repo
	}
	return ""
}

// SetRepo names the Hugging Face repository of a model made outside NewCatalog (tests).
func (m *Model) SetRepo(repo, rev string) { m.repo, m.rev = repo, rev }

// setupKind: the setup offers models of this kind.
func setupKind(k Kind) bool {
	switch k {
	case Chat, Embed, Classify, Image, Video, Music, Sound, Speech:
		return true
	}
	return false
}

// Downloadable: a model of a kind the setup offers (chat, embed, classify, image, video, music, sound, speech) whose
// files this worker can fetch (not Claude Code, not a local runtime's).
func Downloadable(m *Model) bool {
	if !setupKind(m.Kind) {
		return false
	}
	return m.chat != nil || m.pinned != nil || m.repo != "" || len(m.repos) > 0
}

// licences of the catalogue models that do not state theirs (the pickers show it); the manifests state theirs.
var licences = map[string]struct {
	name          string
	nonCommercial bool
}{
	"qwen3-embedding-0.6b": {"Apache-2.0", false},
}

func licenceOf(m *Model) (string, bool) {
	if m.Licence != "" {
		return m.Licence, m.NonCommercial
	}
	if l, ok := licences[m.ID]; ok {
		return l.name, l.nonCommercial
	}
	switch {
	case m.chat != nil: // the Qwen3.5/3.6 checkpoints
		return "Apache-2.0", false
	case m.Backend == "ltx":
		return "LTX-2 Community License", m.NonCommercial
	}
	return "", m.NonCommercial
}

// RecommendedChat is the chat model to propose for a machine with mem bytes of memory: Qwen3.6 35B-A3B from 64 GB,
// the dense 9B from 32 GB, else the 4B.
func RecommendedChat(mem int64) string {
	switch {
	case mem >= 64*gb:
		return llm.Qwen36_35BA3B.Short
	case mem >= 32*gb:
		return llm.Qwen35_9B.Short
	}
	return llm.Qwen35_4B.Short
}

// RecommendedMusic is the song model to propose for a machine with mem bytes of memory: ACE-Step 1.5 turbo (songs
// with vocals, about 10 GB) from 32 GB, else Stable Audio 3 Small (instrumentals, 1.8 GB).
func RecommendedMusic(mem int64) string {
	if mem >= 32*gb {
		return "ace-step-1.5-turbo"
	}
	return "stable-audio-3-small"
}

// SetupRole is what a model is recommended among: its kind, or kind/task for a model that does only a task of its
// own (an upscaler: "image/upscale"; a cutout model: "image/cutout"; the transcriber: "speech/transcribe"; the aligner: "speech/align") — so it does
// not compete with the kind's generators.
func SetupRole(kind Kind, tasks []string) string {
	for _, t := range tasks {
		if t == TaskUpscale || t == TaskCutout || t == "transcribe" || t == TaskAlign {
			return string(kind) + "/" + t
		}
	}
	return string(kind)
}

// recommendedOf is the model to propose first of each role (SetupRole) on a machine with mem bytes ("" none).
func recommendedOf(role string, mem int64) string {
	switch role {
	case string(Chat):
		return RecommendedChat(mem)
	case string(Embed):
		return "qwen3-embedding-0.6b"
	case string(Classify):
		return "laya-typed-decisions"
	case string(Image):
		return RecommendedImage(mem)
	case string(Image) + "/" + TaskUpscale:
		return "seedvr2-3b"
	case string(Image) + "/" + TaskCutout:
		return "birefnet"
	case string(Video):
		return "ltx-2.5-q4"
	case string(Music):
		return RecommendedMusic(mem)
	case string(Sound):
		return "moss-soundeffect"
	case string(Speech):
		return "kokoro-82m"
	case string(Speech) + "/" + TaskAlign:
		return "qwen3-forced-aligner"
	}
	return ""
}

// FitsMemory: the model runs on a machine with mem bytes (0: unknown, yes). A chat model leaves room for the
// classifier, the embedder and its cache (llm.Chat.Fits); an image or video model only has to fit alone (the
// queue unloads a chat model before it when both do not fit); a music, sound or speech model wants twice its
// weights (its runtime's peak: ACE-Step turbo 10 GB of weights on 32 GB, MOSS 6.7 GB on 16 GB).
func FitsMemory(m *Model, mem int64) bool {
	if mem <= 0 {
		return true
	}
	switch m.Kind {
	case Chat:
		if m.chat != nil {
			return m.chat.Fits(mem)
		}
		return m.Size*100/70 < mem
	case Image, Video:
		return m.Size < mem
	case Music, Sound, Speech:
		return m.Size*2 < mem
	}
	return m.Size*4 < mem
}

// SetupModel is a model as the setup lists it (GET /api/setup; docs/api.md "setting up models from vboard").
type SetupModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Kind          Kind   `json:"kind"`
	About         string `json:"about"`
	Here          bool   `json:"here"`
	Size          int64  `json:"size"`
	Licence       string `json:"licence,omitempty"`
	NonCommercial bool   `json:"nonCommercial"`
	Gated         bool   `json:"gated"`
	Access        string `json:"access,omitempty"` // gated only: granted | needed | token | unknown
	URL           string `json:"url,omitempty"`
	Fits          bool   `json:"fits"`
	Recommended   bool   `json:"recommended"`
	// Tasks: what the model does (Model.Tasks: "upscale" for SeedVR2, "cutout" for BiRefNet, "generate"/"edit"; a song or speech model's);
	// absent: the usual. Recommended is per kind, and per kind/task for an upscaler and a cutout model (SetupRole).
	Tasks []string `json:"tasks,omitempty"`
	// Languages: the languages a speech model speaks (absent: it detects them, or not a speech model) — vboard proposes
	// a model to download only for a language it speaks
	Languages []string `json:"languages,omitempty"`
}

// FreeBytes is the free disk where the models go.
func (d *Downloads) FreeBytes() int64 {
	if d.Free != nil {
		return d.Free()
	}
	dir := d.Store.Dir
	for dir != "" {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return freeBytes(dir)
		}
		up := filepath.Dir(dir)
		if up == dir {
			break
		}
		dir = up
	}
	return 0
}

// Models lists the models of the setup's kinds on this machine (mem bytes of memory), downloaded or not; withAccess
// asks Hugging Face whether the token may fetch the gated ones.
func (d *Downloads) Models(ctx context.Context, mem int64, withAccess bool) []SetupModel {
	free := d.FreeBytes()
	var out []SetupModel
	var gated []int
	d.mu.Lock() // a download marks its model here under it
	for _, m := range d.Catalog.List() {
		if !setupKind(m.Kind) || m.Backend == BackendClaudeCode { // Claude Code: an agent, offered when installed
			continue
		}
		if !m.Here && !Downloadable(m) { // a local runtime's, not here: nothing to offer
			continue
		}
		lic, nc := licenceOf(m)
		fits := FitsMemory(m, mem) && (m.Here || m.Size+diskMargin <= free)
		sm := SetupModel{ID: m.ID, Name: m.Name, Kind: m.Kind, About: m.About, Here: m.Here, Size: m.Size, Licence: lic,
			NonCommercial: nc, Gated: m.Gated, URL: RepoURL(m), Fits: fits,
			Recommended: fits && m.ID == recommendedOf(SetupRole(m.Kind, m.Tasks), mem), Tasks: m.Tasks}
		if m.Kind == Speech && m.Audio != nil {
			sm.Languages = m.Audio.Languages
		}
		if m.Gated {
			sm.Access = "unknown"
			gated = append(gated, len(out))
		}
		out = append(out, sm)
	}
	d.mu.Unlock()
	if withAccess && len(gated) > 0 {
		var wg sync.WaitGroup
		for _, i := range gated {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if m := d.Catalog.Find(out[i].ID); m != nil {
					out[i].Access = d.Access(ctx, m, false)
				}
			}()
		}
		wg.Wait()
	}
	return out
}

// Access says whether the Hugging Face token may fetch a gated model: granted, needed (the licence is not accepted
// yet), token (no token, or one Hugging Face does not take), unknown (no answer). fresh asks again.
func (d *Downloads) Access(ctx context.Context, m *Model, fresh bool) string {
	repo := repoOf(m)
	token := models.HFToken()
	if token == "" {
		return NeedToken
	}
	if repo == "" {
		return "unknown"
	}
	h := sha256.Sum256([]byte(token))
	key := repo + "\x00" + hex.EncodeToString(h[:8])
	d.mu.Lock()
	seen, ok := d.access[key]
	d.mu.Unlock()
	if ok && !fresh && time.Since(seen.at) < accessTTL {
		return seen.access
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, HFEndpoint+"/api/models/"+repo+"/auth-check", nil)
	if err != nil {
		return "unknown"
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "unknown"
	}
	res.Body.Close()
	a := "unknown"
	switch res.StatusCode {
	case http.StatusOK:
		a = "granted"
	case http.StatusUnauthorized:
		a = NeedToken // a token Hugging Face does not take
	case http.StatusForbidden:
		a = "needed"
	default:
		return a
	}
	d.mu.Lock()
	if d.access == nil {
		d.access = map[string]accessSeen{}
	}
	d.access[key] = accessSeen{a, time.Now()}
	d.mu.Unlock()
	return a
}

// changed queues a state for Changed. One goroutine at a time delivers the queue, so Changed hears the states in the
// order they happened (queued before downloading) without anyone waiting for it.
func (d *Downloads) changed(st DownloadState) {
	if d.Changed == nil {
		return
	}
	d.noteMu.Lock()
	d.notes = append(d.notes, st)
	if d.noting {
		d.noteMu.Unlock()
		return
	}
	d.noting = true
	d.noteMu.Unlock()
	go func() {
		for {
			d.noteMu.Lock()
			if len(d.notes) == 0 {
				d.noting = false
				d.noteMu.Unlock()
				return
			}
			next := d.notes[0]
			d.notes = d.notes[1:]
			d.noteMu.Unlock()
			d.Changed(next)
		}
	}()
}

func (d *Downloads) find(id string) *Model {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, m := range d.Catalog.List() {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// Start downloads a model (mem: this machine's memory, 0 unknown), or joins its running download; a model that is
// here is done at once. A download that failed or was canceled starts again (resuming its files).
func (d *Downloads) Start(id string, mem int64) (DownloadState, error) {
	m := d.find(id)
	if m == nil {
		return DownloadState{}, fmt.Errorf("%w %q", ErrNoModel, id)
	}
	if !Downloadable(m) {
		if m.Here {
			return DownloadState{Model: m.ID, State: DownloadDone}, nil
		}
		return DownloadState{}, fmt.Errorf("%s is not downloaded through the setup", m.ID)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.runs[m.ID]; r != nil && r.st.Active() {
		return r.st, nil
	}
	if d.runs == nil {
		d.runs = map[string]*download{}
	}
	switch {
	case m.Here:
		r := &download{m: m, st: DownloadState{Model: m.ID, State: DownloadDone}}
		d.runs[m.ID] = r
		return r.st, nil
	case !FitsMemory(m, mem):
		r := &download{m: m, st: DownloadState{Model: m.ID, State: DownloadFailed, Need: NeedMemory,
			Error: fmt.Sprintf("%s needs more memory than this machine has (%s)", m.Name, models.Human(mem))}}
		d.runs[m.ID] = r
		return r.st, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &download{m: m, st: DownloadState{Model: m.ID, State: DownloadQueued, Total: m.Size}, ctx: ctx, cancel: cancel}
	d.runs[m.ID] = r
	d.queue = append(d.queue, r)
	d.once.Do(func() {
		d.wake = make(chan struct{}, 1)
		go d.loop()
	})
	select {
	case d.wake <- struct{}{}:
	default:
	}
	d.changed(r.st)
	return r.st, nil
}

// Status is a model's newest download (false: none since the worker started).
func (d *Downloads) Status(id string) (DownloadState, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.runs[strings.ToLower(strings.TrimSpace(id))]
	if r == nil {
		return DownloadState{}, false
	}
	return r.st, true
}

// List is every download since the worker started, by model.
func (d *Downloads) List() []DownloadState {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]DownloadState, 0, len(d.runs))
	for _, r := range d.runs {
		out = append(out, r.st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// Cancel stops a model's download (the partial files stay: a later one resumes); false: none runs.
func (d *Downloads) Cancel(id string) bool {
	d.mu.Lock()
	r := d.runs[strings.ToLower(strings.TrimSpace(id))]
	if r == nil || !r.st.Active() {
		d.mu.Unlock()
		return false
	}
	r.cancel()
	if r.st.State == DownloadQueued {
		r.st.State, r.st.ETA = DownloadCanceled, 0
		for i, q := range d.queue {
			if q == r {
				d.queue = append(d.queue[:i], d.queue[i+1:]...)
				break
			}
		}
	}
	d.changed(r.st)
	d.mu.Unlock()
	return true
}

// loop runs the queue, one download at a time.
func (d *Downloads) loop() {
	for {
		d.mu.Lock()
		var r *download
		if len(d.queue) > 0 {
			r, d.queue = d.queue[0], d.queue[1:]
		}
		d.mu.Unlock()
		if r == nil {
			<-d.wake
			continue
		}
		d.run(r)
	}
}

func (d *Downloads) set(r *download, f func(st *DownloadState)) {
	d.mu.Lock()
	f(&r.st)
	d.changed(r.st) // queued under the lock: in the order the states were set
	d.mu.Unlock()
}

// run does one download: the access to a gated model, the files (with the disk checked first), their check, the
// Python runtime.
func (d *Downloads) run(r *download) {
	m, ctx := r.m, r.ctx
	defer r.cancel()
	fail := func(err error) {
		d.set(r, func(st *DownloadState) {
			st.ETA = 0
			if ctx.Err() != nil {
				st.State = DownloadCanceled
				return
			}
			st.State, st.Error = DownloadFailed, err.Error()
			var n *needError
			if errors.As(err, &n) {
				st.Need, st.URL = n.need, n.url
			}
		})
	}
	d.set(r, func(st *DownloadState) { st.State = DownloadDownloading })
	if m.Gated {
		switch d.Access(ctx, m, true) {
		case NeedToken:
			fail(&needError{NeedToken, "", m.Name + " is gated on Hugging Face: a Hugging Face token is needed on this machine"})
			return
		case "needed":
			fail(&needError{NeedLicence, RepoURL(m), m.Name + ": accept its licence on " + RepoURL(m) + " (with the Hugging Face account of the token), then try again"})
			return
		}
	}
	var files []models.Model
	missing := int64(0)
	if d.Fetch != nil {
		if !m.Here {
			missing = m.Size
		}
	} else {
		var err error
		if files, err = d.plan(ctx, m); err != nil {
			fail(err)
			return
		}
		for _, hm := range files {
			missing += d.Store.Missing(hm)
		}
	}
	if free := d.FreeBytes(); missing+diskMargin > free {
		fail(&needError{NeedSpace, "", fmt.Sprintf("%s needs %s on disk, %s are free", m.Name, models.Human(missing+diskMargin), models.Human(free))})
		return
	}
	var mu sync.Mutex
	var done int64
	report := func(n, total int64) {
		mu.Lock()
		defer mu.Unlock()
		done = n
		d.set(r, func(st *DownloadState) {
			if total > 0 {
				st.Total = total
			}
			st.Bytes = min(n, st.Total)
			if r.began.IsZero() && n > 0 {
				r.began, r.from = time.Now(), n
			}
			if el := time.Since(r.began).Seconds(); !r.began.IsZero() && el >= 2 && n > r.from {
				rate := float64(n-r.from) / el
				st.ETA = int(float64(st.Total-st.Bytes)/rate + 0.5)
			}
		})
	}
	d.set(r, func(st *DownloadState) { st.Total = missing })
	if d.Fetch != nil {
		if err := d.Fetch(ctx, m, report); err != nil {
			fail(err)
			return
		}
	} else {
		s := *d.Store
		s.Progress = nil
		var last time.Time
		s.Bytes = func(n int64) {
			mu.Lock()
			now := done + n
			mu.Unlock()
			if time.Since(last) >= 250*time.Millisecond || now >= missing {
				last = time.Now()
				report(now, 0)
				return
			}
			mu.Lock()
			done = now
			mu.Unlock()
		}
		for _, hm := range files {
			if _, err := s.Ensure(ctx, hm); err != nil {
				if errors.Is(err, models.ErrGated) || strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "HTTP 403") {
					err = d.gated(ctx, m)
				}
				fail(err)
				return
			}
		}
	}
	d.set(r, func(st *DownloadState) { st.State, st.Bytes, st.ETA = DownloadVerifying, st.Total, 0 })
	var short int64
	for _, hm := range files {
		short += d.Store.Missing(hm)
	}
	if short > 0 {
		fail(fmt.Errorf("%s: %s did not arrive complete", m.Name, models.Human(short)))
		return
	}
	if err := d.prepare(ctx, r, m); err != nil {
		fail(err)
		return
	}
	where := m.Where
	switch {
	case d.Fetch != nil:
	case len(m.repos) > 0: // music, sound, speech: their repositories in the store (as the catalogue says)
		where = d.Store.Dir
	case m.repo != "":
		where = filepath.Join(d.Store.Dir, filepath.FromSlash(files[0].Repo), files[0].Rev)
	default:
		where = d.Store.Path(files[0])
	}
	d.set(r, func(st *DownloadState) {
		m.Here, m.Where = true, where // under the lock Models reads it with
		st.State = DownloadDone
	})
}

// gated explains a refused download of a gated model: no token, or the licence not accepted.
func (d *Downloads) gated(ctx context.Context, m *Model) error {
	if d.Access(ctx, m, true) == NeedToken {
		return &needError{NeedToken, "", m.Name + " is gated on Hugging Face: a Hugging Face token is needed on this machine"}
	}
	return &needError{NeedLicence, RepoURL(m), m.Name + ": accept its licence on " + RepoURL(m) + ", then try again"}
}

// prepare makes the model's runtime (preparing): its Python environment, then its step of its own (building
// LeVo2.cpp, fetching Stable Audio 3's code) — what the first job would do otherwise.
func (d *Downloads) prepare(ctx context.Context, r *download, m *Model) error {
	if d.Fetch != nil && d.Prepare == nil {
		return nil
	}
	env, build := runtimeOf(m)
	if n := runtimeName(m); d.Prepare == nil && n != "" && len(runtimeManifest(n).Tools[Platform()]) > 0 {
		// a server runtime's program (llama.cpp, ComfyUI): fetched with the model, not at its first job
		d.set(r, func(st *DownloadState) { st.State = DownloadPreparing })
		if _, err := ensureTools(ctx, d.Root, n, func(string) {}); err != nil {
			return fmt.Errorf("the %s runtime: %w", n, err)
		}
	}
	if env == nil && build == nil || d.Prepare == nil && build == nil && env.Ready(d.Root) {
		return nil
	}
	d.set(r, func(st *DownloadState) { st.State = DownloadPreparing })
	if d.Prepare != nil {
		name := ""
		if env != nil {
			name = env.Name
		}
		return d.Prepare(ctx, m, name, build != nil)
	}
	if env != nil {
		if err := env.Ensure(ctx, d.Root, func(string) {}); err != nil {
			return fmt.Errorf("the %s runtime: %w", env.Name, err)
		}
	}
	if build != nil {
		if _, err := build(ctx, d.Root, func(Event) {}); err != nil {
			return fmt.Errorf("the %s runtime: %w", m.runtime, err)
		}
	}
	return nil
}

// runtimeOf is the Python runtime a model's backend runs in (nil: none, the Go engine) and the runtime's step of its
// own (nil: none).
// runtimeOf is the Python environment of the model's runtime on this machine (nil none: Go, a local server, a server
// runtime of tools only) and its build step — from the runtime's manifest (runtimeName), the one source of which
// runtime a model runs in.
func runtimeOf(m *Model) (*pyenv.Env, func(ctx context.Context, root string, emit Emit) ([]string, error)) {
	n := runtimeName(m)
	if n == "" {
		return nil, nil
	}
	var env *pyenv.Env
	if e, ok := envFor(n, Platform()); ok {
		env = &e
	}
	return env, runtimeBuildFns[runtimeManifest(n).Build]
}

// plan is what a download of m fetches: the files of each repository, as the backend's first job would. A speech
// model gets all its parts (Qwen3-TTS: CustomVoice, Base and VoiceDesign), not only the one it counts as here with.
func (d *Downloads) plan(ctx context.Context, m *Model) ([]models.Model, error) {
	switch {
	case m.chat != nil:
		return []models.Model{m.chat.Model}, nil
	case m.pinned != nil:
		return []models.Model{*m.pinned}, nil
	case m.Backend == "mflux" || m.Backend == "cutout":
		// the files FluxBackend.Run (and upscale) loads; CutoutBackend.Run's (the code, the config, the weights)
		files := m.files
		if len(files) == 0 {
			files = []string{"text_encoder/", "tokenizer/", "transformer/", "vae/", "scheduler/", "model_index.json"}
		}
		hm, err := d.Store.FromHub(ctx, m.Name, m.repo, m.rev, func(p string) bool { return models.Matches(p, files...) })
		if err != nil {
			return nil, d.hubErr(ctx, m, err)
		}
		return []models.Model{hm}, nil
	case m.Backend == "ltx":
		// the files LTXBackend.download fetches
		hm, err := d.Store.FromHub(ctx, m.Name, m.repo, m.rev, func(p string) bool { return !models.Matches(p, m.ignore...) })
		if err != nil {
			return nil, d.hubErr(ctx, m, err)
		}
		return []models.Model{hm}, nil
	case m.repo != "":
		// a model of a generic, server or ComfyUI runtime: its source as its backend fetches it (files, else all but
		// ignore)
		keep := func(string) bool { return true }
		switch {
		case len(m.files) > 0:
			keep = func(p string) bool { return models.Matches(p, m.files...) }
		case len(m.ignore) > 0:
			keep = func(p string) bool { return !models.Matches(p, m.ignore...) }
		}
		hm, err := d.Store.FromHub(ctx, m.Name, m.repo, m.rev, keep)
		if err != nil {
			return nil, d.hubErr(ctx, m, err)
		}
		return []models.Model{hm}, nil
	case len(m.repos) > 0:
		// every repository the backend's Run fetches (MusicBackend.Run, SoundBackend.files, SpeechBackend.speak, a
		// generic runtime's)
		var out []models.Model
		for _, hr := range m.repos {
			hm, err := d.Store.FromHub(ctx, m.Name, hr.repo, hr.rev, hr.keep())
			if err != nil {
				return nil, d.hubErr(ctx, m, err)
			}
			out = append(out, hm)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s is not downloaded through the setup", m.ID)
}

func (d *Downloads) hubErr(ctx context.Context, m *Model, err error) error {
	if errors.Is(err, models.ErrGated) {
		return d.gated(ctx, m)
	}
	return err
}

// needError is a failure only the person can help with.
type needError struct{ need, url, msg string }

func (e *needError) Error() string { return e.msg }
