package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Setting up models from vboard (docs/api.md, "vboard: setting up models from vboard"): the hello's setup, the model
// list, downloads on the connector owner's workers. The workers do the work (internal/web/downloads.go: GET
// /api/setup, /api/downloads); the connector asks them through the tunnel and tells vboard. Only the vboard user the
// token belongs to (from /api/v1/me) may download, and only onto the workers of the connector's owner.

// Tunables (tests make them short).
var (
	bridgeDownloadPoll = time.Second // a download's state is asked this often
	setupAsk           = 20 * time.Second
)

// workerSetup is a worker's GET /api/setup.
type workerSetup struct {
	Memory      int64 `json:"memory"`
	Free        int64 `json:"free"`
	HuggingFace struct {
		Token bool `json:"token"`
	} `json:"huggingFace"`
	Models    []setupModel    `json:"models"`
	Downloads []downloadState `json:"downloads"`
}

type setupModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	About         string   `json:"about"`
	Here          bool     `json:"here"`
	Size          int64    `json:"size"`
	Licence       string   `json:"licence,omitempty"`
	NonCommercial bool     `json:"nonCommercial"`
	Gated         bool     `json:"gated"`
	Access        string   `json:"access,omitempty"`
	URL           string   `json:"url,omitempty"`
	Fits          bool     `json:"fits"`
	Recommended   bool     `json:"recommended"`
	Tasks         []string `json:"tasks,omitempty"`
}

// downloadState is a worker's download (engine.DownloadState).
type downloadState struct {
	Model string `json:"model"`
	State string `json:"state"`
	Bytes int64  `json:"bytes"`
	Total int64  `json:"total"`
	ETA   int    `json:"eta"`
	Error string `json:"error"`
	Need  string `json:"need"`
	URL   string `json:"url"`
}

// vbOwner is the vboard user the connector's token belongs to (GET /api/v1/me): setup.owner, the one who may download.
type vbOwner struct {
	mu       sync.Mutex
	id, name string
}

func (o *vbOwner) set(id, name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if id != "" {
		o.id = id
	}
	if name != "" {
		o.name = name
	}
}

func (o *vbOwner) get() (id, name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.id, o.name
}

// setupConn is a connection's part of the setup: the setup vboard was told last, and its downloads being followed.
type setupConn struct {
	mu        sync.Mutex
	key       string
	downloads map[string]context.CancelFunc // request id → stops following it
	models    map[string]string             // request id → model
	nodes     map[string]*node              // request id → the worker it runs on
}

func (c *bridgeConn) su() *setupConn {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	if c.setup == nil {
		c.setup = &setupConn{}
	}
	return c.setup
}

// ownerNodes are the connected workers of the connector's owner, the longest connected first (downloads go only
// there: nobody fills another person's disk).
func (s *Server) ownerNodes(ctx context.Context, conf storedConnector) []*node {
	routed, _ := s.fleet.pick(ctx)
	var out []*node
	for _, n := range routed {
		if s.owns(conf.Owner, n.id) {
			out = append(out, n)
		}
	}
	return out
}

// ownerConnected: a worker of the connector's owner is connected — enough to dial with nothing to offer yet (the
// setup asks the owner for a model).
func (s *Server) ownerConnected(ctx context.Context, conf storedConnector) bool {
	return len(s.ownerNodes(ctx, conf)) > 0
}

// workerSetup asks a worker for its setup (access: ask Hugging Face about the gated models too).
func (s *Server) workerSetup(ctx context.Context, n *node, access bool) (*workerSetup, error) {
	ctx, cancel := context.WithTimeout(ctx, setupAsk)
	defer cancel()
	p := "/api/setup"
	if access {
		p += "?access=1"
	}
	var ws workerSetup
	if err := s.fleet.getJSON(ctx, n, p, &ws); err != nil {
		return nil, err
	}
	return &ws, nil
}

// workerSetups asks the workers at once; a worker that does not answer (an older one) is left out.
func (s *Server) workerSetups(ctx context.Context, nodes []*node, access bool) []*workerSetup {
	out := make([]*workerSetup, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], _ = s.workerSetup(ctx, n, access)
		}()
	}
	wg.Wait()
	return out
}

// helloSetup is the hello's setup (and {type:"setup"}'s): downloads, the owner, the setup page, whether a Hugging
// Face token is set on the owner's workers, and — with one worker — its memory and free disk.
func (r *vboardRun) helloSetup(ctx context.Context, conf storedConnector) map[string]any {
	nodes := r.s.ownerNodes(ctx, conf)
	setups := r.s.workerSetups(ctx, nodes, false)
	token := false
	for _, ws := range setups {
		token = token || ws != nil && ws.HuggingFace.Token
	}
	st := map[string]any{"downloads": true, "huggingFace": map[string]bool{"token": token}}
	if id, _ := r.vb.get(); id != "" {
		st["owner"] = id
	}
	if u := r.s.setupURL(); u != "" {
		st["setupUrl"] = u
	}
	if len(setups) == 1 && setups[0] != nil {
		st["memory"], st["free"] = setups[0].Memory, setups[0].Free
	}
	r.refreshDone(ctx, nodes, setups)
	return st
}

// setupKey compares setups: the free disk in whole GB (it changes all the time while a model is downloaded).
func setupKey(st map[string]any) string {
	c := make(map[string]any, len(st))
	for k, v := range st {
		c[k] = v
	}
	if f, ok := c["free"].(int64); ok {
		c["free"] = f >> 30
	}
	b, _ := json.Marshal(c)
	return string(b)
}

// refreshDone asks a worker for its models again when one of its downloads finished that the fleet does not know as
// here yet (a download vboard did not start: the first start's embedder and classifier) — the model is then offered.
func (r *vboardRun) refreshDone(ctx context.Context, nodes []*node, setups []*workerSetup) {
	for i, ws := range setups {
		if ws == nil {
			continue
		}
		for _, d := range ws.Downloads {
			if d.State == "done" && !nodeHas(nodes[i], d.Model) {
				r.s.refreshNode(ctx, nodes[i])
				break
			}
		}
	}
}

// nodeHas: the worker lists the model as here.
func nodeHas(n *node, id string) bool {
	_, _, _, ms := n.snapshot()
	for _, m := range ms {
		if m.ID == id && m.Here {
			return true
		}
	}
	return false
}

// refreshNode asks a worker for its models now and tells everyone who waits for a change of the fleet.
func (s *Server) refreshNode(ctx context.Context, n *node) {
	if err := s.fleet.probe(ctx, n); err != nil {
		log.Printf("worker %q: %v", n.name, err)
	}
	s.fleet.notify()
}

// RefreshWorkers asks every worker for its models now (the standalone hub, when its worker downloaded one).
func (s *Server) RefreshWorkers() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, n := range s.fleet.list() {
		_ = s.fleet.probe(ctx, n)
	}
	s.fleet.notify()
}

// syncSetup tells vboard the setup when it changed since the connection was told ({type:"setup"}; no handover).
func (r *vboardRun) syncSetup(ctx context.Context, c *bridgeConn) {
	st := r.helloSetup(ctx, c.conf)
	if ctx.Err() != nil {
		return
	}
	key := setupKey(st)
	su := c.su()
	su.mu.Lock()
	changed := su.key != key
	su.key = key
	su.mu.Unlock()
	if changed {
		c.send("", map[string]any{"type": "setup", "setup": st})
	}
}

// setupMsg is a setup request of vboard: {models}, {download}, {downloadCancel}.
type setupMsg struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Model   string `json:"model"`
	Worker  string `json:"worker"`
	UserID  string `json:"userId"` // who asks (context.userId wins)
	Context *struct {
		UserID string `json:"userId"`
	} `json:"context"`
}

// setupRequest answers a setup request. The model list is work of the connection (a handover waits for it); a
// download is followed on the connection that asked until it ends or the connection does — it runs on regardless,
// and vboard asks again on its next connection.
func (r *vboardRun) setupRequest(ctx context.Context, c *bridgeConn, work *running, raw []byte) {
	var m setupMsg
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	switch m.Type {
	case "models":
		work.add(1)
		go func() {
			defer work.add(-1)
			list := r.modelList(ctx, c.conf)
			if ctx.Err() == nil {
				c.send(m.ID, map[string]any{"type": "models", "id": m.ID, "models": list})
			}
		}()
	case "download":
		su := c.su()
		dctx, cancel := context.WithCancel(ctx)
		su.mu.Lock()
		if su.downloads == nil {
			su.downloads, su.models, su.nodes = map[string]context.CancelFunc{}, map[string]string{}, map[string]*node{}
		}
		if old := su.downloads[m.ID]; old != nil {
			old()
		}
		su.downloads[m.ID] = cancel
		su.mu.Unlock()
		go func() {
			defer func() {
				cancel()
				su.mu.Lock()
				delete(su.downloads, m.ID)
				delete(su.models, m.ID)
				delete(su.nodes, m.ID)
				su.mu.Unlock()
			}()
			r.download(dctx, c, &m)
		}()
	case "downloadCancel":
		su := c.su()
		su.mu.Lock()
		cancel, model, n := su.downloads[m.ID], su.models[m.ID], su.nodes[m.ID]
		su.mu.Unlock()
		if cancel == nil {
			return
		}
		cancel() // nothing more is sent for it
		if n != nil && model != "" {
			go func() {
				ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				if res, err := r.s.fleet.request(ctx, n, http.MethodDelete, "/api/downloads?model="+url.QueryEscape(model), nil, ""); err == nil {
					res.Body.Close()
				}
				r.logf("download %s (%s) canceled", m.ID, model)
			}()
		}
	}
}

// setupRole is what a model is recommended among (engine.SetupRole): its kind, or kind/task for an upscaler
// ("image/upscale") and the transcriber ("speech/transcribe"), which do not compete with the kind's generators.
func setupRole(kind string, tasks []string) string {
	for _, t := range tasks {
		if t == "upscale" || t == "transcribe" {
			return kind + "/" + t
		}
	}
	return kind
}

// modelList is the model list of the owner's workers: one entry per model and worker, at most one recommended per
// role (setupRole; on the worker with the most memory).
func (r *vboardRun) modelList(ctx context.Context, conf storedConnector) []map[string]any {
	nodes := r.s.ownerNodes(ctx, conf)
	setups := r.s.workerSetups(ctx, nodes, true)
	out := []map[string]any{}
	best := map[string]int{}      // role → entry recommended
	bestMem := map[string]int64{} // role → its worker's memory
	for i, ws := range setups {
		if ws == nil {
			continue
		}
		for _, m := range ws.Models {
			e := map[string]any{"id": m.ID, "name": m.Name, "kind": m.Kind, "about": m.About, "here": m.Here, "size": m.Size,
				"licence": m.Licence, "nonCommercial": m.NonCommercial, "gated": m.Gated, "url": m.URL, "fits": m.Fits,
				"recommended": false, "worker": trimRunes(nodes[i].name, agentChars)}
			if m.Gated {
				e["access"] = m.Access
			}
			if len(m.Tasks) > 0 { // an upscaler (["upscale"]) is told apart before it is a generator
				e["tasks"] = m.Tasks
			}
			if role := setupRole(m.Kind, m.Tasks); m.Recommended {
				if j, ok := best[role]; !ok || ws.Memory > bestMem[role] {
					if ok {
						out[j]["recommended"] = false
					}
					best[role], bestMem[role] = len(out), ws.Memory
					e["recommended"] = true
				}
			}
			out = append(out, e)
		}
	}
	return out
}

// download downloads a model on a worker of the connector's owner for the connector's vboard user, and reports:
// {downloadStatus} while it runs (at most one a second, one at least every keep-alive), then {downloadDone} — the
// model is offered by then — or {needs} (what only the person can do) or {error}.
func (r *vboardRun) download(ctx context.Context, c *bridgeConn, m *setupMsg) {
	send := func(v map[string]any) {
		v["id"] = m.ID
		c.send(m.ID, v)
	}
	fail := func(msg string) {
		if ctx.Err() != nil {
			return
		}
		msg = trimRunes(msg, bridgeErrorChars)
		r.logf("download %s (%s) failed: %s", m.ID, m.Model, msg)
		send(map[string]any{"type": "error", "message": msg})
	}
	needs := func(need, u, msg string) {
		if ctx.Err() != nil {
			return
		}
		r.logf("download %s (%s) needs %s: %s", m.ID, m.Model, need, msg)
		v := map[string]any{"type": "needs", "model": m.Model, "need": need, "message": trimRunes(msg, bridgeErrorChars)}
		if u != "" {
			v["url"] = u
		}
		send(v)
	}
	owner, ownerName := r.vb.get()
	if ownerName == "" {
		ownerName = c.user
	}
	who := m.UserID
	if m.Context != nil && m.Context.UserID != "" {
		who = m.Context.UserID
	}
	if m.Model == "" {
		fail("which model? (download needs \"model\")")
		return
	}
	if owner == "" {
		fail("this connector does not know whose token it uses (vboard's /api/v1/me named no user id), so it downloads for nobody")
		return
	}
	if who != owner {
		if ownerName == "" {
			ownerName = "the owner of this AI"
		}
		needs("owner", "", fmt.Sprintf("Ask %s to download %s: only the person whose token connects this AI can download models", ownerName, m.Model))
		return
	}
	n, err := r.downloadNode(ctx, c.conf, m)
	if err != nil {
		fail(err.Error())
		return
	}
	su := c.su()
	su.mu.Lock()
	su.models[m.ID], su.nodes[m.ID] = m.Model, n
	su.mu.Unlock()

	body, _ := json.Marshal(map[string]string{"model": m.Model})
	st, err := r.downloadCall(ctx, n, http.MethodPost, "/api/downloads", body)
	if err != nil {
		fail(fmt.Sprintf("%s: %v", n.name, err))
		return
	}
	r.logf("download %s: %s on %s (%s)", m.ID, m.Model, n.name, st.State)
	var lastSent time.Time
	lastKey := ""
	for {
		switch st.State {
		case "done":
			r.s.refreshNode(ctx, n) // the model is offered now: agents, generators or a handover follow as usual
			if ctx.Err() == nil {
				r.logf("download %s: %s is on %s", m.ID, m.Model, n.name)
				send(map[string]any{"type": "downloadDone", "model": m.Model})
			}
			return
		case "failed":
			if st.Need != "" {
				needs(st.Need, st.URL, st.Error)
			} else {
				fail(st.Error)
			}
			return
		case "canceled":
			fail(m.Model + ": the download was canceled")
			return
		}
		key := fmt.Sprintf("%s %d %d", st.State, st.Bytes, st.Total)
		if since := time.Since(lastSent); lastSent.IsZero() || key != lastKey && since >= time.Second || since >= bridgeKeepAlive {
			v := map[string]any{"type": "downloadStatus", "model": m.Model, "state": st.State, "worker": trimRunes(n.name, agentChars)}
			if st.Total > 0 {
				v["bytes"], v["total"] = st.Bytes, st.Total
			}
			if st.ETA > 0 {
				v["eta"] = st.ETA
			}
			send(v)
			lastSent, lastKey = time.Now(), key
		}
		select {
		case <-ctx.Done():
			return // the connection ended or vboard canceled: the worker goes on (or was told to stop)
		case <-time.After(bridgeDownloadPoll):
		}
		next, err := r.downloadCall(ctx, n, http.MethodGet, "/api/downloads?model="+url.QueryEscape(m.Model), nil)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if r.s.fleet.node(n.id) == nil {
				fail(n.name + " went away: the download goes on when it is back — ask again then")
				return
			}
			continue // a hiccup of the tunnel: ask again
		}
		st = next
	}
}

// downloadNode is the worker a download goes to: the one named (of the owner's), else the owner's where the model
// fits best (the most free disk).
func (r *vboardRun) downloadNode(ctx context.Context, conf storedConnector, m *setupMsg) (*node, error) {
	nodes := r.s.ownerNodes(ctx, conf)
	if len(nodes) == 0 {
		who := conf.Owner
		if who == "" {
			who = "the connector's owner"
		}
		return nil, fmt.Errorf("no computer of %s is connected to download %s to", who, m.Model)
	}
	if m.Worker != "" {
		for _, n := range nodes {
			if strings.EqualFold(n.name, m.Worker) || strings.EqualFold(trimRunes(n.name, agentChars), m.Worker) {
				return n, nil
			}
		}
		return nil, fmt.Errorf("%q is not a connected computer of %s", m.Worker, conf.Owner)
	}
	if len(nodes) == 1 {
		return nodes[0], nil
	}
	var pick *node
	var pickFree int64 = -1
	listed := (*node)(nil)
	for i, ws := range r.s.workerSetups(ctx, nodes, false) {
		if ws == nil {
			continue
		}
		for _, x := range ws.Models {
			if x.ID != m.Model {
				continue
			}
			if listed == nil {
				listed = nodes[i]
			}
			if x.Here {
				return nodes[i], nil // already there: done at once
			}
			if x.Fits && ws.Free > pickFree {
				pick, pickFree = nodes[i], ws.Free
			}
		}
	}
	switch {
	case pick != nil:
		return pick, nil
	case listed != nil:
		return listed, nil // the worker says why it cannot
	}
	return nodes[0], nil
}

// downloadCall is a call to a worker's /api/downloads.
func (r *vboardRun) downloadCall(ctx context.Context, n *node, method, path string, body []byte) (downloadState, error) {
	cctx, cancel := context.WithTimeout(ctx, setupAsk)
	defer cancel()
	var rd io.Reader
	ctype := ""
	if body != nil {
		rd, ctype = bytes.NewReader(body), "application/json"
	}
	res, err := r.s.fleet.request(cctx, n, method, path, rd, ctype)
	if err != nil {
		return downloadState{}, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(b))
		if res.StatusCode == http.StatusNotFound && strings.Contains(msg, "404 page not found") {
			msg = "this worker cannot download models on request (an older ai-connector: update it)"
		}
		return downloadState{}, fmt.Errorf("%s", msg)
	}
	var st downloadState
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	return st, nil
}
