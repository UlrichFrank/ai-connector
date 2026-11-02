package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"time"
)

// A client (an application's token) sees and cancels only its own jobs: those whose request carries its id
// (engine.Request.ClientID, set by the server on submission). A device (a person) sees every job, as before.

// jobEventsPoll: a job's event stream asks the worker this often whether the job ended (events can be lost when
// a worker reconnects); it pings as often.
var jobEventsPoll = 15 * time.Second

// jobUpdate is what the hub reads of a worker's event (jobs.Update) or job.
type jobUpdate struct {
	JobID string   `json:"jobId"`
	Job   *jobHead `json:"job"`
	Event *struct {
		Type  string `json:"type"`
		Step  int    `json:"step"`
		Total int    `json:"total"`
	} `json:"event"`
}

// jobQueuePoll: a queued job's place is asked this often (its worker says no event when a job before it ends).
var jobQueuePoll = time.Second

// progress is where a followed job is, as its event stream says it ({"jobId","progress":{…}}): queued with the jobs
// ahead, running with its steps and — once two steps are seen — the seconds still to go (from their pace).
type progress struct {
	state       string
	ahead       int
	step, total int
	firstStep   int
	firstAt     time.Time
	sent        string
}

// next takes what is known now and returns the event to send ("" when nothing changed).
func (p *progress) next(id string) string {
	v := map[string]any{"state": p.state}
	switch p.state {
	case "queued":
		v["ahead"] = p.ahead
	case "running":
		if p.total > 0 {
			v["step"], v["total"] = p.step, p.total
			if p.firstAt.IsZero() {
				p.firstStep, p.firstAt = p.step, time.Now()
			} else if p.step > p.firstStep && p.step <= p.total {
				per := time.Since(p.firstAt).Seconds() / float64(p.step-p.firstStep)
				v["eta"] = int(math.Ceil(per * float64(p.total-p.step)))
			}
		}
	default:
		return ""
	}
	key, _ := json.Marshal(v)
	if string(key) == p.sent {
		return ""
	}
	p.sent = string(key)
	b, _ := json.Marshal(map[string]any{"jobId": id, "progress": v})
	return string(b)
}

// head takes a job as the worker has it.
func (p *progress) head(h *jobHead) {
	p.state = h.State
	if h.Ahead != nil {
		p.ahead = *h.Ahead
	}
	if h.Total > 0 {
		p.step, p.total = h.Step, h.Total
	}
}

type jobHead struct {
	State   string `json:"state"`
	Ahead   *int   `json:"ahead"` // queued: the jobs before it on its worker (jobs.Job.Ahead)
	Step    int    `json:"step"`
	Total   int    `json:"total"`
	Request struct {
		ClientID string `json:"clientId"`
	} `json:"request"`
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

// jobNode is the Mac a job is on (its route), the primary for an id without one; nil when that Mac is offline.
func (s *Server) jobNode(id string, prim *node) *node {
	route := jobRoute(id)
	if route == "" {
		return prim
	}
	n, _, known := s.byRoute(route)
	if !known {
		return prim
	}
	return n
}

// jobOf asks the job's Mac for it: the job as the worker sends it, and what the hub reads of it.
func (s *Server) jobOf(ctx context.Context, id string, prim *node) (json.RawMessage, *jobHead, error) {
	n := s.jobNode(id, prim)
	if n == nil {
		return nil, nil, fmt.Errorf("the Mac of job %s is offline", id)
	}
	a := s.send(ctx, n, http.MethodGet, "/api/jobs/"+url.PathEscape(id), nil, "")
	if a.err != nil {
		return nil, nil, a.err
	}
	if a.status != http.StatusOK {
		return nil, nil, fmt.Errorf("job %s: %d", id, a.status)
	}
	var h jobHead
	if err := json.Unmarshal(a.body, &h); err != nil {
		return nil, nil, err
	}
	return a.body, &h, nil
}

// ownJob: the job is the client's. Unknown, unreachable and other clients' jobs are not.
func (s *Server) ownJob(ctx context.Context, c Client, id string, prim *node) bool {
	_, h, err := s.jobOf(ctx, id, prim)
	return err == nil && h.Request.ClientID == c.ID
}

// eventFilter: which events of the merged stream (GET /api/events) the caller gets — every one for a device, a
// client's own jobs' for a client (the jobs it has now, and those that come with its id).
func (s *Server) eventFilter(r *http.Request, nodes []*node) func(data []byte) bool {
	c, ok := clientOf(r)
	if !ok {
		return func([]byte) bool { return true }
	}
	own := map[string]bool{}
	for _, b := range s.each(r.Context(), nodes, "/api/jobs") {
		var list []struct {
			ID string `json:"id"`
			jobHead
		}
		if json.Unmarshal(b, &list) != nil {
			continue
		}
		for _, j := range list {
			if j.Request.ClientID == c.ID {
				own[j.ID] = true
			}
		}
	}
	return func(data []byte) bool {
		var u jobUpdate
		if json.Unmarshal(data, &u) != nil || u.JobID == "" {
			return false
		}
		if u.Job != nil && u.Job.Request.ClientID == c.ID {
			own[u.JobID] = true
		}
		return own[u.JobID]
	}
}

// jobEvents: GET /api/jobs/{id}/events — one job's events (SSE, as GET /api/events sends them, none dropped): the
// job as it is now first, then every update and event of it; the stream ends after the update that ends the job.
func (s *Server) jobEvents(w http.ResponseWriter, r *http.Request, id string, prim *node) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	feed := s.fleet.followJob() // before the job is read: nothing between is missed
	defer feed.close()
	feed.setJob(id)
	raw, h, err := s.jobOf(r.Context(), id, prim)
	if err != nil {
		notFound(w)
		return
	}
	if c, ok := clientOf(r); ok && h.Request.ClientID != c.ID {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	sendJob := func(raw json.RawMessage) {
		b, _ := json.Marshal(map[string]any{"jobId": id, "job": raw})
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
	sendJob(raw)
	if finished(h.State) {
		return
	}
	var p progress
	sendProgress := func() {
		if ev := p.next(id); ev != "" {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
	}
	p.head(h)
	sendProgress()
	fl.Flush()
	poll := time.NewTicker(jobEventsPoll)
	defer poll.Stop()
	queue := time.NewTicker(jobQueuePoll)
	defer queue.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-poll.C:
			if raw, h, err := s.jobOf(r.Context(), id, prim); err == nil && finished(h.State) {
				sendJob(raw)
				return
			}
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-queue.C:
			if p.state != "queued" {
				continue
			}
			if _, h, err := s.jobOf(r.Context(), id, prim); err == nil && h.State == "queued" {
				p.head(h)
				sendProgress()
				fl.Flush()
			}
		case <-feed.ready:
			end := false
			for _, data := range feed.take() {
				fmt.Fprintf(w, "data: %s\n\n", data)
				var u jobUpdate
				if json.Unmarshal(data, &u) != nil {
					continue
				}
				switch {
				case u.Job != nil && finished(u.Job.State):
					end = true
				case u.Job != nil:
					p.head(u.Job)
					sendProgress()
				case u.Event != nil && u.Event.Type == "progress":
					p.state, p.step, p.total = "running", u.Event.Step, u.Event.Total
					sendProgress()
				}
			}
			fl.Flush()
			if end {
				return
			}
		}
	}
}

// ownFile: the file (a /files/ path) is named by one of the client's jobs — what it made, what it was given — on the
// Mac that holds it.
func (s *Server) ownFile(ctx context.Context, c Client, path string, prim *node) bool {
	n := prim
	if route := fileRoute(path); route != "" {
		if x, _, known := s.byRoute(route); known {
			n = x
		}
	}
	if n == nil {
		return false
	}
	a := s.send(ctx, n, http.MethodGet, "/api/jobs", nil, "")
	if !a.ok() {
		return false
	}
	var list []json.RawMessage
	if json.Unmarshal(a.body, &list) != nil {
		return false
	}
	quoted, _ := json.Marshal(path)
	for _, raw := range list {
		var h jobHead
		if json.Unmarshal(raw, &h) == nil && h.Request.ClientID == c.ID && bytes.Contains(raw, quoted) {
			return true
		}
	}
	return false
}
