// Package jobs queues the requests of the TUI and the web app and runs them one at a time (one GPU), keeps their
// history (~/.config/mlxlab/history.json) and tells subscribers what happens.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/agent"
	"github.com/philipparndt/ai-connector/internal/approvals"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/llm"
)

// State of a job.
type State string

const (
	Queued   State = "queued"
	Running  State = "running"
	Done     State = "done"
	Failed   State = "failed"
	Canceled State = "canceled"
)

// Job is one request and what came of it.
type Job struct {
	ID       string         `json:"id"`
	Model    string         `json:"model"`
	Name     string         `json:"name"`
	Kind     engine.Kind    `json:"kind"`
	Request  engine.Request `json:"request"`
	State    State          `json:"state"`
	Created  time.Time      `json:"created"`
	Started  time.Time      `json:"started,omitzero"`
	Finished time.Time      `json:"finished,omitzero"`
	Status   string         `json:"status,omitempty"`
	Text     string         `json:"text,omitempty"` // chat: the answer
	Step     int            `json:"step,omitempty"`
	Total    int            `json:"total,omitempty"`
	Files    []string       `json:"files,omitempty"`
	Preview  string         `json:"preview,omitempty"` // a movie so far (the clips that are done)
	Cover    string         `json:"cover,omitempty"`   // a song's cover image
	Starred  bool           `json:"starred,omitempty"` // a favourite
	Stats    map[string]any `json:"stats,omitempty"`
	Result   map[string]any `json:"result,omitempty"`
	// Tools: a chat's tool calls, the latest state of each (agent.ToolEvent as the "tool" events carry it).
	Tools   []map[string]any `json:"tools,omitempty"`
	Logs    []string         `json:"logs,omitempty"`
	Error   string           `json:"error,omitempty"`
	Problem *Problem         `json:"problem,omitempty"` // the error in words, with what to do

	cancel context.CancelFunc
}

// Seconds is how long it ran (so far).
func (j *Job) Seconds() float64 {
	switch {
	case j.Started.IsZero():
		return 0
	case j.Finished.IsZero():
		return time.Since(j.Started).Seconds()
	}
	return j.Finished.Sub(j.Started).Seconds()
}

// Duration formats seconds: 4.2 s, 2:05, 1:02:05.
func Duration(sec float64) string {
	if sec < 60 {
		return strconv.FormatFloat(sec, 'f', 1, 64) + " s"
	}
	t := int(sec + 0.5)
	if t >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", t/3600, t/60%60, t%60)
	}
	return fmt.Sprintf("%d:%02d", t/60, t%60)
}

// Active: queued or running.
func (j *Job) Active() bool { return j.State == Queued || j.State == Running }

func (j *Job) clone() *Job {
	c := *j
	c.Files = append([]string(nil), j.Files...)
	c.Logs = append([]string(nil), j.Logs...)
	c.Stats = cloneMap(j.Stats)
	c.Tools = append([]map[string]any(nil), j.Tools...)
	c.cancel = nil
	return &c
}

// mapWith is a copy of m with one more entry.
func mapWith(m map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for a, b := range m {
		out[a] = b
	}
	out[k] = v
	return out
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	c := make(map[string]any, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// Update is what subscribers get: a job's new state (Job) or one of its events (Event, with JobID).
type Update struct {
	JobID string        `json:"jobId"`
	Job   *Job          `json:"job,omitempty"`
	Event *engine.Event `json:"event,omitempty"`
	// At: a text event's place in the job's text (its length in bytes before the piece), so that a follower that
	// missed a piece sees the gap (and takes the text from the job) instead of joining the pieces wrongly
	At *int `json:"at,omitempty"`
}

// Manager runs the jobs.
type Manager struct {
	Catalog  *engine.Catalog
	Backends map[string]engine.Backend
	Dir      string // ~/.config/mlxlab: outputs/, history.json
	// Approvals is the list of tools allowed always (nil: <Dir>/approvals.json), see AllowAlways.
	Approvals *approvals.Store

	mu    sync.Mutex
	jobs  []*Job
	queue chan *Job
	subs  map[int]chan Update
	next  int
	seq   int
	gates map[string]*agent.Gate // the running job's: where approvals of its tool calls go

	saving         sync.Mutex
	snaps, written int  // history snapshots taken (under mu), the newest written (under saving)
	busy           bool // the worker is on a job (until it is saved)
	beside         int  // jobs running beside the queue (until they are saved)

	current  string               // the backend of the queue's running job ("" none)
	lastUsed map[string]time.Time // per backend: when its last job ended (or it was first seen loaded)
}

const maxLogs = 400

// New makes a manager and starts its worker.
func New(cat *engine.Catalog, backends []engine.Backend, dir string) *Manager {
	m := &Manager{Catalog: cat, Backends: map[string]engine.Backend{}, Dir: dir, queue: make(chan *Job, 1024), subs: map[int]chan Update{},
		gates: map[string]*agent.Gate{}}
	for _, b := range backends {
		m.Backends[b.Name()] = b
	}
	for _, j := range m.load() {
		m.dispatch(j)
	}
	go m.work()
	return m
}

// OutDir is where files go.
func (m *Manager) OutDir() string { return filepath.Join(m.Dir, "outputs") }

// Submit queues a request.
func (m *Manager) Submit(r engine.Request) (*Job, error) {
	md := m.Catalog.Find(r.Model)
	if md == nil {
		return nil, fmt.Errorf("no model %q", r.Model)
	}
	if _, ok := m.Backends[md.Backend]; !ok {
		return nil, fmt.Errorf("%s: backend %s is not available", md.ID, md.Backend)
	}
	r.Model = md.ID
	r.Folder = CleanFolder(r.Folder)
	// a model's params (a generic runtime's): checked before the job is queued, the defaults filled in
	if len(r.Params) > 0 || len(md.Params) > 0 {
		p, err := engine.CheckParams(md, r.Params)
		if err != nil {
			return nil, err
		}
		r.Params = p
	}
	if err := m.checkImages(md, &r); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if err := m.checkZone(md.Kind, &r); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if r.Parent != "" && r.Folder == "" {
		if p := m.find(r.Parent); p != nil {
			r.Folder = p.Request.Folder // a version lives with its original
		}
	}
	m.seq++
	id := time.Now().Format("150405") + "-" + strconv.Itoa(m.seq)
	m.mu.Unlock()
	// the images go into files (not the history) before the job exists
	if err := m.storeImages(id, &r); err != nil {
		return nil, err
	}
	m.mu.Lock()
	j := &Job{ID: id, Model: md.ID, Name: md.Name, Kind: md.Kind,
		Request: r, State: Queued, Created: time.Now(), Status: "queued"}
	m.jobs = append(m.jobs, j)
	snap := j.clone()
	m.mu.Unlock()
	m.publish(Update{JobID: j.ID, Job: snap})
	m.dispatch(j)
	m.save()
	return snap, nil
}

// checkZone keeps a chat's zone (internal/agent): a follow-up runs in its parent's zone, and its history carries
// only tool turns the worker itself returned with the parent. Called with m.mu held.
func (m *Manager) checkZone(kind engine.Kind, r *engine.Request) error {
	if kind != engine.Chat {
		if r.Zone != "" {
			return errors.New("a zone is for chats only")
		}
		if len(r.Tools) > 0 {
			return errors.New("tools are for chats only")
		}
		return nil
	}
	if len(r.Tools) > 0 && r.Zone != "" {
		return errors.New("a chat has either a zone (tools on the worker) or the caller's tools, not both")
	}
	if r.Zone == "" && r.Parent == "" {
		// an application's conversation (the OpenAI API): its history may carry its own tools' calls and results.
		// Nothing runs tools in a chat without a zone, and a follow-up of it can never take one (FollowUp), so no
		// tool result reaches a zone this way.
		return nil
	}
	var parent *agent.Parent
	if r.Parent != "" {
		p := m.find(r.Parent)
		switch {
		case p != nil:
			parent = &agent.Parent{Zone: agent.Zone(p.Request.Zone), Messages: ResultMessages(p)}
		case r.Zone != "" || hasToolTurns(r.Messages):
			return errors.New("the chat this turn continues is gone: start a new one")
		}
	}
	z, err := agent.FollowUp(agent.Zone(r.Zone), r.Messages, parent)
	if err != nil {
		return err
	}
	r.Zone = string(z)
	return nil
}

func hasToolTurns(msgs []llm.Message) bool {
	for _, m := range msgs {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// ResultMessages is the history a chat job returned (result.messages; after a restart it is read back from JSON).
func ResultMessages(j *Job) []llm.Message {
	v, ok := j.Result["messages"]
	if !ok {
		return nil
	}
	if msgs, ok := v.([]llm.Message); ok {
		return msgs
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var msgs []llm.Message
	if json.Unmarshal(b, &msgs) != nil {
		return nil
	}
	return msgs
}

// Settled says nothing is queued or running and the history on disk is up to date (tests wait for it).
func (m *Manager) Settled() bool {
	m.mu.Lock()
	busy, snaps := m.busy || m.beside > 0, m.snaps
	for _, j := range m.jobs {
		busy = busy || j.Active()
	}
	m.mu.Unlock()
	m.saving.Lock()
	defer m.saving.Unlock()
	return !busy && m.written >= snaps
}

// ErrNotRunning: the job is not running (an approval came too late).
var ErrNotRunning = errors.New("the job is not running")

// Approve answers a running chat's tool call that waits for the user (agent.ErrNotWaiting: none waits by that id).
func (m *Manager) Approve(id, call string, allow bool) error {
	m.mu.Lock()
	j, g := m.find(id), m.gates[id]
	m.mu.Unlock()
	if j == nil {
		return errors.New("no such job")
	}
	if g == nil {
		return ErrNotRunning
	}
	return g.Decide(call, allow)
}

// ErrNotAlways: the call cannot be allowed always (not a home tool that waits for approval).
var ErrNotAlways = errors.New("only a home tool that waits for approval can be allowed always")

// AllowAlways approves a waiting call and puts its tool on the list of tools allowed always (approvals.json), so
// its next calls run without asking. Only a call of a home tool that waits for approval qualifies.
func (m *Manager) AllowAlways(id, call string) error {
	m.mu.Lock()
	j, g := m.find(id), m.gates[id]
	var name, zone, state string
	if j != nil {
		for _, t := range j.Tools {
			if t["id"] == call {
				name, _ = t["name"].(string)
				zone, _ = t["zone"].(string)
				state, _ = t["state"].(string)
			}
		}
	}
	m.mu.Unlock()
	if j == nil {
		return errors.New("no such job")
	}
	if g == nil {
		return ErrNotRunning
	}
	if name == "" || state != "approval" {
		return agent.ErrNotWaiting
	}
	if agent.Zone(zone) != agent.ZoneHome {
		return ErrNotAlways
	}
	if err := g.Decide(call, true); err != nil {
		return err
	}
	return m.ApprovalStore().Add(name)
}

// ApprovalStore is the list of tools allowed always.
func (m *Manager) ApprovalStore() *approvals.Store {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Approvals == nil {
		m.Approvals = approvals.Open(m.Dir)
	}
	return m.Approvals
}

// Import puts a file that came from elsewhere (an uploaded song) into the history as a finished job, so it can be
// played and worked on like a generated one.
func (m *Manager) Import(kind engine.Kind, name, file string, r engine.Request) *Job {
	now := time.Now()
	m.mu.Lock()
	m.seq++
	j := &Job{ID: now.Format("150405") + "-" + strconv.Itoa(m.seq), Model: "upload", Name: name, Kind: kind, Request: r,
		State: Done, Created: now, Started: now, Finished: now, Status: "imported", Files: []string{file}}
	m.jobs = append(m.jobs, j)
	snap := j.clone()
	m.mu.Unlock()
	m.save()
	m.publish(Update{JobID: j.ID, Job: snap})
	return snap
}

// Cancel stops a queued or running job.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	j := m.find(id)
	if j == nil {
		m.mu.Unlock()
		return errors.New("no such job")
	}
	switch j.State {
	case Queued:
		j.State, j.Status, j.Finished = Canceled, "canceled", time.Now()
	case Running:
		if j.cancel != nil {
			j.cancel()
		}
	}
	snap := j.clone()
	m.mu.Unlock()
	m.publish(Update{JobID: id, Job: snap})
	return nil
}

// Retry runs a failed or canceled job's request again as a new job, which takes the old one's place.
func (m *Manager) Retry(id string) (*Job, error) {
	old, ok := m.Job(id)
	if !ok {
		return nil, errors.New("no such job")
	}
	if old.Active() {
		return nil, errors.New("the job is still running")
	}
	r := old.Request
	// the seed that was used (a random one is chosen when the request has none): the same result is tried again,
	// and a movie finds the clips it already rendered
	if r.Seed == 0 {
		if s, ok := old.Stats["seed"].(float64); ok {
			r.Seed = int64(s)
		} else if s, ok := old.Stats["seed"].(int64); ok {
			r.Seed = s
		}
	}
	j, err := m.Submit(r)
	if err != nil {
		return nil, err
	}
	m.Delete(id)
	return j, nil
}

// Star marks a job as a favourite (or not).
func (m *Manager) Star(id string, on bool) (*Job, error) {
	m.mu.Lock()
	j := m.find(id)
	if j == nil {
		m.mu.Unlock()
		return nil, errors.New("no such job")
	}
	j.Starred = on
	snap := j.clone()
	m.mu.Unlock()
	m.save()
	m.publish(Update{JobID: id, Job: snap})
	return snap, nil
}

// SetCover makes an image file a song's cover ("" removes it).
func (m *Manager) SetCover(id, file string) (*Job, error) {
	m.mu.Lock()
	j := m.find(id)
	if j == nil {
		m.mu.Unlock()
		return nil, errors.New("no such job")
	}
	j.Cover = file
	snap := j.clone()
	m.mu.Unlock()
	m.save()
	m.publish(Update{JobID: id, Job: snap})
	return snap, nil
}

// Rename sets a job's title (a song's name in the studio).
func (m *Manager) Rename(id, title string) (*Job, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, errors.New("a title")
	}
	m.mu.Lock()
	j := m.find(id)
	if j == nil {
		m.mu.Unlock()
		return nil, errors.New("no such job")
	}
	j.Request.Title = title
	snap := j.clone()
	m.mu.Unlock()
	m.save()
	m.publish(Update{JobID: id, Job: snap})
	return snap, nil
}

// CleanFolder tidies a folder path: "/ a // b /" → "a/b".
func CleanFolder(f string) string {
	var parts []string
	for _, p := range strings.Split(f, "/") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "/")
}

// Move puts a job and every version made from it (their parent chain leads to it) into a folder.
func (m *Manager) Move(id, folder string) ([]*Job, error) {
	folder = CleanFolder(folder)
	m.mu.Lock()
	if m.find(id) == nil {
		m.mu.Unlock()
		return nil, errors.New("no such job")
	}
	family := map[string]bool{id: true}
	for changed := true; changed; {
		changed = false
		for _, j := range m.jobs {
			if !family[j.ID] && family[j.Request.Parent] {
				family[j.ID], changed = true, true
			}
		}
	}
	var moved []*Job
	for _, j := range m.jobs {
		if family[j.ID] {
			j.Request.Folder = folder
			moved = append(moved, j.clone())
		}
	}
	m.mu.Unlock()
	m.save()
	for _, j := range moved {
		m.publish(Update{JobID: j.ID, Job: j})
	}
	return moved, nil
}

// RenameFolder renames a folder (and the folders inside it); to "" it removes the folder: its songs move up to
// where it was. Songs are never deleted.
func (m *Manager) RenameFolder(from, to string) (int, error) {
	from, to = CleanFolder(from), CleanFolder(to)
	if from == "" {
		return 0, errors.New("which folder")
	}
	parent := ""
	if i := strings.LastIndex(from, "/"); i >= 0 {
		parent = from[:i]
	}
	m.mu.Lock()
	var moved []*Job
	for _, j := range m.jobs {
		f := j.Request.Folder
		if f != from && !strings.HasPrefix(f, from+"/") {
			continue
		}
		rest := strings.TrimPrefix(f, from)
		if to == "" {
			j.Request.Folder = CleanFolder(parent + rest)
		} else {
			j.Request.Folder = CleanFolder(to + rest)
		}
		moved = append(moved, j.clone())
	}
	m.mu.Unlock()
	m.save()
	for _, j := range moved {
		m.publish(Update{JobID: j.ID, Job: j})
	}
	return len(moved), nil
}

// Delete removes a finished job from the history (its files stay).
func (m *Manager) Delete(id string) {
	m.mu.Lock()
	for i, j := range m.jobs {
		if j.ID == id && !j.Active() {
			m.jobs = append(m.jobs[:i], m.jobs[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	m.save()
	m.publish(Update{JobID: id})
}

// DeleteWithFiles removes a finished job and the files it made: its outputs, a song's melody score next to it, a
// movie's previews and clips. A file another job still uses (a version's source, a cover, a reference image) stays.
func (m *Manager) DeleteWithFiles(id string) error {
	m.mu.Lock()
	j := m.find(id)
	if j == nil {
		m.mu.Unlock()
		return errors.New("no such job")
	}
	if j.Active() {
		m.mu.Unlock()
		return errors.New("still running: cancel it first")
	}
	var files []string
	for _, f := range j.Files {
		files = append(files, f, strings.TrimSuffix(f, filepath.Ext(f))+".abc")
	}
	files = append(files, imageFiles(j.Request)...)
	if j.Preview != "" {
		files = append(files, j.Preview)
	}
	clips, _ := j.Stats["clipsDir"].(string)
	used := map[string]bool{}
	for _, o := range m.jobs {
		if o.ID == id {
			continue
		}
		for _, f := range o.Files {
			used[f] = true
		}
		used[o.Cover], used[o.Request.Source], used[o.Preview] = true, true, true
		for _, f := range o.Request.Images {
			used[f] = true
		}
		for _, f := range imageFiles(o.Request) { // a retried chat's images
			used[f] = true
		}
		if c, _ := o.Stats["clipsDir"].(string); c != "" && c == clips {
			clips = "" // another version of the movie still has its clips
		}
	}
	m.mu.Unlock()
	out := m.OutDir()
	inside := func(p string) bool {
		rel, err := filepath.Rel(out, p)
		return err == nil && !strings.HasPrefix(rel, "..") && rel != "."
	}
	for _, f := range files {
		if f != "" && !used[f] && inside(f) {
			_ = os.Remove(f)
		}
	}
	if clips != "" && inside(clips) {
		_ = os.RemoveAll(clips)
	}
	m.Delete(id)
	return nil
}

func (m *Manager) find(id string) *Job {
	for _, j := range m.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// Jobs lists the jobs, newest last.
func (m *Manager) Jobs() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, len(m.jobs))
	for i, j := range m.jobs {
		out[i] = j.clone()
	}
	return out
}

// Job is one job.
func (m *Manager) Job(id string) (*Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.find(id)
	if j == nil {
		return nil, false
	}
	return j.clone(), true
}

// subBuffer: updates kept for a subscriber that has not taken them yet.
const subBuffer = 4096

// Subscribe gets every update until the returned function is called. A subscriber that does not keep up misses
// events, never a job's state (publish).
func (m *Manager) Subscribe() (<-chan Update, func()) {
	ch := make(chan Update, subBuffer)
	m.mu.Lock()
	m.next++
	id := m.next
	m.subs[id] = ch
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		if _, ok := m.subs[id]; ok {
			delete(m.subs, id)
			close(ch)
		}
		m.mu.Unlock()
	}
}

func (m *Manager) publish(u Update) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range m.subs {
		select {
		case ch <- u:
		default: // a subscriber that does not keep up misses events
			if u.Job != nil { // but not a job's state (its end above all): an older event makes room for it
				makeRoom(ch)
				ch <- u
			}
		}
	}
}

// makeRoom drops one update from a full subscriber channel: the oldest event (a follower sees the gap in the text by
// its "at" and takes the text from the job), else the oldest state. Called with m.mu held, so nothing else sends
// meanwhile. The subscriber may take updates concurrently: that leaves more room, but then a few of the older updates
// can reach it out of order (text is placed by its "at"; a job's newer state supersedes an older one) — the update
// that needed the room is always sent after them.
func makeRoom(ch chan Update) {
	var q []Update
	for len(q) < cap(ch) {
		select {
		case u := <-ch:
			q = append(q, u)
			continue
		default:
		}
		break
	}
	if len(q) < cap(ch) {
		for _, u := range q { // the subscriber took some meanwhile: there is room already
			ch <- u
		}
		return
	}
	drop := 0
	for i, u := range q {
		if u.Job == nil {
			drop = i
			break
		}
	}
	for i, u := range q {
		if i != drop {
			ch <- u
		}
	}
}

// Loaded lists what each backend has in memory.
func (m *Manager) Loaded() map[string]string {
	out := map[string]string{}
	for n, b := range m.Backends {
		out[n] = b.Loaded()
	}
	return out
}

// Unload frees every backend's memory.
func (m *Manager) Unload() {
	for _, b := range m.Backends {
		b.Unload()
	}
}

func (m *Manager) work() { m.loop(m.queue, false) }

// dispatch queues a job, or starts it at once when its backend runs beside the queue (engine.Parallel: an agent
// that uses no memory here, see internal/agents).
func (m *Manager) dispatch(j *Job) {
	if !m.Beside(j) {
		m.queue <- j
		return
	}
	one := make(chan *Job, 1)
	one <- j
	close(one)
	go m.loop(one, true)
}

// Beside says the job runs beside the queue, not in it.
func (m *Manager) Beside(j *Job) bool {
	md := m.Catalog.Find(j.Model)
	if md == nil {
		return false
	}
	p, ok := m.Backends[md.Backend].(engine.Parallel)
	return ok && p.Parallel()
}

// loop runs the jobs of a queue one after another: the queue's, or one job beside it.
func (m *Manager) loop(queue <-chan *Job, beside bool) {
	for j := range queue {
		m.mu.Lock()
		if j.State != Queued {
			m.mu.Unlock()
			continue
		}
		if beside {
			m.beside++
		} else {
			m.busy = true
		}
		ctx, cancel := context.WithCancel(context.Background())
		j.cancel = cancel
		gate := agent.NewGate()
		m.gates[j.ID] = gate
		ctx = agent.WithGate(ctx, gate)
		j.State, j.Started, j.Status = Running, time.Now(), "starting …"
		md := m.Catalog.Find(j.Model)
		b := m.Backends[md.Backend]
		if !beside {
			m.current = md.Backend
		}
		snap := j.clone()
		m.mu.Unlock()
		m.publish(Update{JobID: j.ID, Job: snap})

		// one heavy model in memory at a time: a 20 GB chat model and a video model do not fit together
		if b.Heavy() {
			for _, o := range m.Backends {
				if o != b && o.Heavy() && o.Loaded() != "" {
					m.event(j, engine.Event{Type: "status", Text: "unloading " + o.Loaded() + " …"})
					o.Unload()
				}
			}
		}
		_ = os.MkdirAll(m.OutDir(), 0o755)
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			req, err := withImageData(j.Request)
			if err != nil {
				return err
			}
			return b.Run(ctx, md, req, m.OutDir(), func(ev engine.Event) { m.event(j, ev) })
		}()
		canceled := ctx.Err() != nil
		cancel()

		m.mu.Lock()
		delete(m.gates, j.ID)
		j.Finished, j.cancel = time.Now(), nil
		switch {
		case canceled && (err == nil || errors.Is(err, context.Canceled)):
			j.State, j.Status = Canceled, "canceled"
		case err != nil:
			p := Explain(err.Error())
			j.State, j.Status, j.Error, j.Problem = Failed, "failed", err.Error(), &p
		default:
			j.State, j.Status = Done, "done in "+Duration(j.Seconds())
		}
		var coverOf *Job
		if j.State == Done && j.Request.CoverFor != "" && len(j.Files) > 0 {
			if song := m.find(j.Request.CoverFor); song != nil {
				song.Cover = j.Files[0] // the picture made for a song becomes its cover
				coverOf = song.clone()
			}
		}
		snap = j.clone()
		m.mu.Unlock()
		// saved before it is announced: `mlxlab run` exits on the announcement, and a job still "queued" in the
		// history would run again at the next start
		m.save()
		m.mu.Lock()
		if beside {
			m.beside--
		} else {
			m.busy, m.current = false, ""
		}
		if m.lastUsed == nil {
			m.lastUsed = map[string]time.Time{}
		}
		m.lastUsed[md.Backend] = time.Now()
		m.mu.Unlock()
		m.publish(Update{JobID: j.ID, Job: snap})
		if coverOf != nil {
			m.publish(Update{JobID: coverOf.ID, Job: coverOf})
		}
	}
}

// event records an event in the job and passes it on.
func (m *Manager) event(j *Job, ev engine.Event) {
	var at *int
	m.mu.Lock()
	switch ev.Type {
	case "status":
		j.Status = ev.Text
	case "log":
		j.Logs = append(j.Logs, ev.Text)
		if len(j.Logs) > maxLogs {
			j.Logs = j.Logs[len(j.Logs)-maxLogs:]
		}
	case "text":
		n := len(j.Text)
		at = &n
		j.Text += ev.Text
	case "progress":
		j.Step, j.Total = ev.Step, ev.Total
	case "file":
		j.Files = append(j.Files, ev.File)
		j.Preview = "" // the whole film is here
	case "preview":
		j.Preview = ev.File
	case "stats":
		if j.Stats == nil {
			j.Stats = map[string]any{}
		}
		for k, v := range ev.Data {
			j.Stats[k] = v
		}
	case "result":
		if msgs, ok := ev.Data["messages"].([]llm.Message); ok && llm.HasImages(msgs) {
			ev.Data = mapWith(ev.Data, "messages", withoutImageData(msgs)) // the history keeps references only
		}
		j.Result = ev.Data
	case "tool":
		id, _ := ev.Data["id"].(string)
		found := false
		for i, t := range j.Tools {
			if t["id"] == id {
				kept := ev.Data
				// a call that waited for the user keeps saying so ("asked"), for the card and the transcript
				if t["state"] == "approval" || t["asked"] == true {
					kept = mapWith(ev.Data, "asked", true)
				}
				j.Tools[i], found = kept, true
			}
		}
		if !found {
			j.Tools = append(j.Tools, ev.Data)
		}
	}
	m.mu.Unlock()
	m.publish(Update{JobID: j.ID, Event: &ev, At: at})
}

func (m *Manager) historyPath() string { return filepath.Join(m.Dir, "history.json") }

func (m *Manager) save() {
	m.mu.Lock()
	m.snaps++
	snap := m.snaps
	var done []*Job // and the queued ones: they run after a restart
	for _, j := range m.jobs {
		if j.State != Running {
			done = append(done, j.clone())
		}
	}
	m.mu.Unlock()
	if len(done) > 500 {
		done = done[len(done)-500:]
	}
	b, err := json.MarshalIndent(done, "", " ")
	if err != nil {
		return
	}
	// one write at a time (they share the .tmp file), and never an older snapshot after a newer one: else a job
	// done could be written back as still queued
	m.saving.Lock()
	defer m.saving.Unlock()
	if snap < m.written {
		return
	}
	_ = os.MkdirAll(m.Dir, 0o755)
	tmp := m.historyPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, m.historyPath())
		m.written = snap
	}
}

// load reads the history; it returns the jobs that were still queued (to run again).
func (m *Manager) load() []*Job {
	b, err := os.ReadFile(m.historyPath())
	if err != nil {
		return nil
	}
	var js []*Job
	if json.Unmarshal(b, &js) != nil {
		return nil
	}
	var queued []*Job
	for _, j := range js {
		if strings.TrimSpace(j.ID) == "" {
			continue
		}
		switch j.State {
		case Queued:
			queued = append(queued, j)
		case Running: // the program stopped while it ran: it can be retried (a movie keeps its finished clips)
			j.State, j.Status, j.Error = Failed, "interrupted", "mlxlab stopped while this was running"
			j.Problem = &Problem{Title: "Interrupted", Hint: "mlxlab was stopped while this ran. Retry it — a movie keeps the clips it had already finished."}
		}
		m.jobs = append(m.jobs, j)
		if n := strings.LastIndex(j.ID, "-"); n >= 0 {
			if v, err := strconv.Atoi(j.ID[n+1:]); err == nil && v > m.seq {
				m.seq = v // new ids stay unique
			}
		}
	}
	return queued
}
