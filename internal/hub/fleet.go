package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/tunnel"
)

// The connected Macs (docs/architecture.md, "Several Macs (workers) at once"): what each is (specs), what it does
// (state), which models it has; kept current from its own event stream and a refresh every 30 s. The fleet also
// carries every Mac's events to the hub's clients (one stream per Mac, however many clients listen).

// Header names the worker understands (internal/web/route.go).
const (
	routeHeader      = "X-Mlxlab-Route"
	workerIDHeader   = "X-Mlxlab-Worker-Id"
	workerNameHeader = "X-Mlxlab-Worker-Name"
)

// refreshEvery: how often a Mac's state and models are asked for besides its events.
var refreshEvery = 30 * time.Second

// Specs is what a Mac is (its /api/info).
type Specs struct {
	Chip      string `json:"chip"`
	RAMGB     int    `json:"ramGB"`
	GPUCores  int    `json:"gpuCores,omitempty"`
	MacOS     string `json:"macOS"`
	OS        string `json:"os,omitempty"`
	OSVersion string `json:"osVersion,omitempty"`
	Arch      string `json:"arch,omitempty"`
	GPU       string `json:"gpu,omitempty"`
	VRAMGB    int    `json:"vramGB,omitempty"`
	Version   string `json:"version"`
}

// WorkerState is what a Mac is doing (its /api/info).
type WorkerState struct {
	Running *string           `json:"running"`
	Queued  int               `json:"queued"`
	Loaded  map[string]string `json:"loaded"`
}

// idle: nothing running, nothing waiting.
func (st WorkerState) idle() bool { return st.Running == nil && st.Queued == 0 }

// load: the jobs it has to do before a new one.
func (st WorkerState) load() int {
	n := st.Queued
	if st.Running != nil {
		n++
	}
	return n
}

// catalogModel is the part of a worker's model the hub routes by.
type catalogModel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Backend string `json:"backend"`
	Size    int64  `json:"size"`
	Here    bool   `json:"here"`
	Images  bool   `json:"images,omitempty"` // a chat model that takes images
	// image and video models (vboard_generate.go): what they do besides text to image, their licence, their defaults
	Tasks         []string       `json:"tasks,omitempty"`
	NonCommercial bool           `json:"nonCommercial,omitempty"`
	Defaults      *modelDefaults `json:"defaults,omitempty"`
	// music, sound and speech models (vboard_audio.go): what they take
	Audio *audioCaps `json:"audio,omitempty"`
}

// modelDefaults are the request fields a model starts with (the part the hub reads).
type modelDefaults struct {
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	FPS    int `json:"fps,omitempty"`
	Frames int `json:"frames,omitempty"`
	// music, sound: the length (seconds)
	Duration float64 `json:"duration,omitempty"`
}

// audioCaps is what a music, sound or speech model takes (engine.AudioCaps).
type audioCaps struct {
	MaxSeconds   float64    `json:"maxSeconds,omitempty"`
	Lyrics       bool       `json:"lyrics,omitempty"`
	Instrumental bool       `json:"instrumental,omitempty"`
	BPM          bool       `json:"bpm,omitempty"`
	Voices       []genVoice `json:"voices,omitempty"`
	Languages    []string   `json:"languages,omitempty"`
	Instruct     bool       `json:"instruct,omitempty"`
	MaxChars     int        `json:"maxChars,omitempty"`
	Words        bool       `json:"words,omitempty"`
}

// genVoice is a speech model's preset voice (engine.PresetVoice).
type genVoice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Language string `json:"language"`
}

// node is one connected Mac.
type node struct {
	id, name, route string
	since           time.Time
	probed          chan struct{} // closed once its /api/info was asked (answered or not)
	cancel          context.CancelFunc

	mu     sync.Mutex
	known  bool // /api/info answered
	routes bool // it understands the route header (an older mlxlab does not: it is served alone, unmapped)
	specs  Specs
	state  WorkerState
	models []catalogModel
}

func (n *node) snapshot() (routes bool, sp Specs, st WorkerState, models []catalogModel) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.routes, n.specs, n.state, n.models
}

func (n *node) isRouted() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.known && n.routes
}

func (n *node) ram() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.specs.RAMGB
}

// fleet keeps the nodes in step with the tunnel.
type fleet struct {
	tunnel  *tunnel.Hub
	workers *workers
	client  *http.Client
	file    string // <data>/primary.json

	mu      sync.Mutex
	nodes   map[string]*node
	pinned  string
	changed chan struct{}
	subs    map[int]chan []byte
	feeds   map[int]*jobFeed // followJob

	refreshMu sync.Mutex // refreshModels
	refreshed time.Time
	next      int
}

func newFleet(t *tunnel.Hub, ws *workers, dataDir string) *fleet {
	f := &fleet{tunnel: t, workers: ws, client: &http.Client{Transport: t.Transport()},
		file: filepath.Join(dataDir, "primary.json"), nodes: map[string]*node{}, changed: make(chan struct{}),
		subs: map[int]chan []byte{}}
	if b, err := os.ReadFile(f.file); err == nil {
		var p struct{ Primary string }
		if json.Unmarshal(b, &p) == nil {
			f.pinned = p.Primary
		}
	}
	go f.run()
	return f
}

// Changed is closed at the next change of the fleet: a Mac came, went or was probed, the primary changed.
func (f *fleet) Changed() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

func (f *fleet) notify() {
	f.mu.Lock()
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

// run follows the tunnel: a node for every connected Mac.
func (f *fleet) run() {
	for {
		changed := f.tunnel.Changed()
		f.reconcile(f.tunnel.Connected())
		<-changed
	}
}

func (f *fleet) reconcile(conns []tunnel.Conn) {
	f.mu.Lock()
	seen := map[string]bool{}
	var started []*node
	dirty := false
	for _, c := range conns {
		seen[c.ID] = true
		if n := f.nodes[c.ID]; n != nil && n.since.Equal(c.Since) {
			continue
		} else if n != nil {
			n.cancel()
		}
		route := f.workers.route(c.ID)
		if route == "" {
			route = "m" + c.ID // revoked a moment ago: it is about to go
		}
		ctx, cancel := context.WithCancel(context.Background())
		n := &node{id: c.ID, name: c.Name, route: route, since: c.Since, probed: make(chan struct{}), cancel: cancel}
		f.nodes[c.ID] = n
		started = append(started, n)
		go f.watch(ctx, n)
		dirty = true
	}
	for id, n := range f.nodes {
		if !seen[id] {
			n.cancel()
			delete(f.nodes, id)
			dirty = true
		}
	}
	f.mu.Unlock()
	if dirty {
		f.notify()
	}
}

// node is the connected Mac with this id (nil: not connected).
func (f *fleet) node(id string) *node {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nodes[id]
}

// list is every connected Mac, the longest connected first.
func (f *fleet) list() []*node {
	f.mu.Lock()
	out := make([]*node, 0, len(f.nodes))
	for _, n := range f.nodes {
		out = append(out, n)
	}
	f.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].since.Equal(out[j].since) {
			return out[i].since.Before(out[j].since)
		}
		return out[i].id < out[j].id
	})
	return out
}

// wait lets the Macs that just connected answer their first /api/info (a few seconds at most).
func (f *fleet) wait(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, n := range f.list() {
		select {
		case <-n.probed:
		case <-ctx.Done():
			return
		}
	}
}

// pick is what a request goes to: the Macs that understand routes; or, when none does, a single older one that
// gets everything unchanged (nil, nil: nobody is connected).
func (f *fleet) pick(ctx context.Context) (routed []*node, single *node) {
	f.wait(ctx)
	all := f.list()
	for _, n := range all {
		if n.isRouted() {
			routed = append(routed, n)
		}
	}
	if len(routed) == 0 && len(all) > 0 {
		return nil, all[0]
	}
	return routed, nil
}

func (f *fleet) pinnedID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pinned
}

// pin makes a Mac the primary ("" the automatic choice) and remembers it in primary.json.
func (f *fleet) pin(id string) error {
	f.mu.Lock()
	f.pinned = id
	f.mu.Unlock()
	f.notify()
	return writeFileAtomic(f.file, map[string]string{"primary": id})
}

// primary is the Mac whose libraries are served: the pinned one while connected, else the one with the most
// memory (the longest connected on a tie). Among Macs that understand routes when there are any.
func (f *fleet) primary(nodes []*node) *node {
	pinned := f.pinnedID()
	var best *node
	for _, n := range nodes { // nodes are the longest connected first
		if n.id == pinned {
			return n
		}
		if best == nil || n.ram() > best.ram() {
			best = n
		}
	}
	return best
}

// primaryID is the primary among the connected Macs now ("" none).
func (f *fleet) primaryID() string {
	all := f.list()
	var routed []*node
	for _, n := range all {
		if n.isRouted() {
			routed = append(routed, n)
		}
	}
	if len(routed) == 0 {
		routed = all
	}
	if p := f.primary(routed); p != nil {
		return p.id
	}
	return ""
}

// ---------- talking to a Mac ----------

// request makes a request to a Mac through the tunnel, with its route (when it understands routes).
func (f *fleet) request(ctx context.Context, n *node, method, path string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+tunnel.Host(n.id)+path, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	n.mu.Lock()
	routes := n.routes || !n.known
	n.mu.Unlock()
	if routes {
		setRoute(req.Header, n)
	}
	return f.client.Do(req)
}

func setRoute(h http.Header, n *node) {
	h.Set(routeHeader, n.route)
	h.Set(workerIDHeader, n.id)
	h.Set(workerNameHeader, url.QueryEscape(n.name))
}

// getJSON asks a Mac for path and decodes the answer into v.
func (f *fleet) getJSON(ctx context.Context, n *node, path string, v any) error {
	r, err := f.request(ctx, n, "GET", path, nil, "")
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return &statusError{r.StatusCode}
	}
	return json.NewDecoder(r.Body).Decode(v)
}

type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("status %d", e.code) }

// probe reads a Mac's /api/info (and its models when it understands routes).
func (f *fleet) probe(ctx context.Context, n *node) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var info struct {
		Routes bool        `json:"routes"`
		Specs  Specs       `json:"specs"`
		State  WorkerState `json:"state"`
	}
	err := f.getJSON(ctx, n, "/api/info", &info)
	var se *statusError
	switch {
	case err == nil:
	case errors.As(err, &se) && se.code < 500, isDecode(err) && ctx.Err() == nil:
		info.Routes = false // an older mlxlab (no such endpoint, or not this answer)
	default:
		return err
	}
	var models []catalogModel
	if info.Routes {
		var ms struct {
			Models []catalogModel `json:"models"`
		}
		if err := f.getJSON(ctx, n, "/api/models", &ms); err != nil {
			return err
		}
		models = ms.Models
	}
	n.mu.Lock()
	n.known, n.routes, n.specs, n.state = true, info.Routes, info.Specs, info.State
	if n.state.Loaded == nil {
		n.state.Loaded = map[string]string{}
	}
	if info.Routes {
		n.models = models
	}
	n.mu.Unlock()
	return nil
}

// Asking the workers for their models again when a model is listed nowhere (refreshModels).
var (
	refreshModelsWait = 2 * time.Second // what a request waits for it at most
	refreshModelsGap  = 2 * time.Second // at most one in this time (a client asking for nonsense again and again)
)

// refreshModels asks the Macs for their models now (GET /api/models?refresh=1: a worker first asks its local
// runtimes, so a model pulled a moment ago is there), instead of waiting for the next probe (every 30 s). One at a
// time, at most once in refreshModelsGap, at most refreshModelsWait long; false: it did not ask (too soon).
func (f *fleet) refreshModels(ctx context.Context, nodes []*node) bool {
	f.refreshMu.Lock()
	defer f.refreshMu.Unlock()
	if time.Since(f.refreshed) < refreshModelsGap {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, refreshModelsWait)
	defer cancel()
	var wg sync.WaitGroup
	for _, n := range nodes {
		if !n.isRouted() {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var ms struct {
				Models []catalogModel `json:"models"`
			}
			if err := f.getJSON(ctx, n, "/api/models?refresh=1", &ms); err != nil {
				return
			}
			n.mu.Lock()
			n.models = ms.Models
			n.mu.Unlock()
		}()
	}
	wg.Wait()
	f.refreshed = time.Now()
	f.notify()
	return true
}

func isDecode(err error) bool {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	return errors.As(err, &se) || errors.As(err, &te) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// watch keeps a node current until it goes: probed at once (again until it answers), then its events, then a
// refresh every 30 s and soon after every job change.
func (f *fleet) watch(ctx context.Context, n *node) {
	for attempt := 0; ; attempt++ {
		err := f.probe(ctx, n)
		if attempt == 0 {
			close(n.probed)
		}
		if err == nil || attempt == 0 {
			f.notify()
		}
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("worker %q: %v (asking again)", n.name, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	if !n.isRouted() {
		return // an older mlxlab: served alone, as it was
	}
	dirty := make(chan struct{}, 1)
	go f.events(ctx, n, dirty)
	tick := time.NewTicker(refreshEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-dirty:
			select { // a burst of changes is one refresh
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
		if err := f.probe(ctx, n); err != nil && ctx.Err() == nil {
			log.Printf("worker %q: %v", n.name, err)
		}
	}
}

// events follows a Mac's /api/events (again after a second when it ends) and passes every event on.
func (f *fleet) events(ctx context.Context, n *node, dirty chan<- struct{}) {
	for ctx.Err() == nil {
		r, err := f.request(ctx, n, "GET", "/api/events", nil, "")
		if err == nil {
			if r.StatusCode == http.StatusOK {
				br := bufio.NewReaderSize(r.Body, 64<<10)
				for {
					line, err := br.ReadBytes('\n')
					if err != nil {
						break
					}
					data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data: "))
					if !ok {
						continue
					}
					f.broadcast(bytes.Clone(data))
					if bytes.Contains(data, []byte(`"job":`)) { // a job changed: state, queue, what is loaded
						select {
						case dirty <- struct{}{}:
						default:
						}
					}
				}
			}
			r.Body.Close()
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// subscribe gets every Mac's events (data of SSE events) until the returned function is called. A client that
// does not keep up misses events (like the worker's own stream).
func (f *fleet) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 1024)
	f.mu.Lock()
	f.next++
	id := f.next
	f.subs[id] = ch
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		delete(f.subs, id)
		f.mu.Unlock()
	}
}

func (f *fleet) broadcast(data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs {
		select {
		case ch <- data:
		default:
		}
	}
	for _, j := range f.feeds {
		j.push(data)
	}
}

// jobFeedEarly: events kept for a feed while its job's id is not known yet (the moment of the submission); more
// drop the oldest.
const jobFeedEarly = 4096

// jobFeed is one job's events for the one that follows it (a chat completion, a connector's chat): unlike
// subscribe it never drops an event of its job, however slowly they are taken — the queue is bounded by the job's
// own events. Made before the job is submitted (followJob), it keeps every event until setJob names the job, then
// only that job's.
type jobFeed struct {
	f  *fleet
	id int

	mu    sync.Mutex
	key   []byte // `"jobId":"<id>"` once the job is known
	q     [][]byte
	early int  // events kept before the key
	lost  bool // early events were dropped (the job's could be among them)
	ready chan struct{}
}

// followJob starts a job feed; close ends it.
func (f *fleet) followJob() *jobFeed {
	j := &jobFeed{f: f, ready: make(chan struct{}, 1)}
	f.mu.Lock()
	f.next++
	j.id = f.next
	if f.feeds == nil {
		f.feeds = map[int]*jobFeed{}
	}
	f.feeds[j.id] = j
	f.mu.Unlock()
	return j
}

func (j *jobFeed) close() {
	j.f.mu.Lock()
	delete(j.f.feeds, j.id)
	j.f.mu.Unlock()
}

func (j *jobFeed) push(data []byte) {
	j.mu.Lock()
	switch {
	case j.key == nil:
		if j.early >= jobFeedEarly {
			j.q, j.lost = j.q[1:], true
			j.early--
		}
		j.q = append(j.q, data)
		j.early++
	case bytes.Contains(data, j.key):
		j.q = append(j.q, data)
	default:
		j.mu.Unlock()
		return
	}
	j.mu.Unlock()
	select {
	case j.ready <- struct{}{}:
	default:
	}
}

// setJob names the job: from now on only its events are kept (also of those kept so far).
func (j *jobFeed) setJob(id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.key = []byte(`"jobId":"` + id + `"`)
	kept := j.q[:0]
	for _, d := range j.q {
		if bytes.Contains(d, j.key) {
			kept = append(kept, d)
		}
	}
	clear(j.q[len(kept):])
	j.q, j.early = kept, 0
}

// take is what came since the last take (ready says when there is something).
func (j *jobFeed) take() [][]byte {
	j.mu.Lock()
	defer j.mu.Unlock()
	q := j.q
	j.q = nil
	return q
}

// routedSet names the Macs that understand routes now (to see whether the set changed).
func (f *fleet) routedSet() string {
	var ids []string
	for _, n := range f.list() {
		if n.isRouted() {
			ids = append(ids, n.id+"@"+n.since.String())
		}
	}
	return strings.Join(ids, ",")
}
