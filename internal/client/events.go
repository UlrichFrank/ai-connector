package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
)

const maxRetry = 10 * time.Second

// Events follows GET /api/events. The channel starts with every job (an Update with Job), then carries what
// happens; after a dropped connection it reconnects by itself and sends every job again, plus an Update with only
// the JobID for each job that went meanwhile, so nothing is missed. It closes when ctx ends.
//
// It fails at once only when the server will not have us (ErrSignIn, a wrong URL); an offline Mac is waited for.
func (c *Client) Events(ctx context.Context) (<-chan jobs.Update, error) {
	res, err := c.open(ctx, "/api/events")
	if err != nil && !errors.Is(err, ErrOffline) {
		return nil, err
	}
	ch := make(chan jobs.Update, 256)
	known := map[string]bool{} // the jobs the channel told of, to notice the ones deleted while we were away
	put := func(u jobs.Update) bool {
		switch {
		case u.Job != nil:
			known[u.JobID] = true
		case u.Event == nil:
			delete(known, u.JobID)
		}
		select {
		case ch <- u:
			return true
		case <-ctx.Done():
			return false
		}
	}
	// resync: every job again (the stream is already open, so nothing falls between the list and the stream)
	resync := func() error {
		js, err := c.Jobs(ctx)
		if err != nil {
			return err
		}
		now := map[string]bool{}
		for _, j := range js {
			now[j.ID] = true
		}
		for id := range known {
			if !now[id] && !put(jobs.Update{JobID: id}) {
				return ctx.Err()
			}
		}
		for _, j := range js {
			if !put(jobs.Update{JobID: j.ID, Job: j}) {
				return ctx.Err()
			}
		}
		return nil
	}
	data := func(b []byte) bool {
		var u jobs.Update
		if json.Unmarshal(b, &u) != nil || u.JobID == "" {
			return true // not ours; keep reading
		}
		return put(u)
	}
	go func() {
		defer close(ch)
		c.follow(ctx, "/api/events", res, err, resync, data)
	}()
	return ch, nil
}

// WorkerEvents follows the hub's GET /api/worker/events: whether the Mac is connected, at once and on every
// change. It reconnects like Events.
func (c *Client) WorkerEvents(ctx context.Context) (<-chan WorkerState, error) {
	res, err := c.open(ctx, "/api/worker/events")
	if err != nil {
		return nil, err
	}
	ch := make(chan WorkerState, 8)
	go func() {
		defer close(ch)
		c.follow(ctx, "/api/worker/events", res, nil, nil, func(b []byte) bool {
			var s WorkerState
			if json.Unmarshal(b, &s) != nil {
				return true
			}
			select {
			case ch <- s:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()
	return ch, nil
}

// follow reads a stream until ctx ends, reconnecting with backoff. res is the first connection (nil: it failed with
// err); connected runs after each connect before the stream is read.
func (c *Client) follow(ctx context.Context, path string, res *http.Response, err error, connected func() error, data func([]byte) bool) {
	wait := c.retry
	for {
		if res == nil && err == nil {
			res, err = c.open(ctx, path)
		}
		if err == nil && connected != nil {
			if err = connected(); err != nil {
				res.Body.Close()
			}
		}
		if err == nil {
			c.state(path, nil)
			wait = c.retry
			err = c.read(res, data)
			if err == nil || errors.Is(err, io.EOF) {
				err = errors.New("the connection closed")
			}
		}
		res = nil
		if ctx.Err() != nil {
			return
		}
		c.state(path, err)
		err = nil
		if !sleep(ctx, wait) {
			return
		}
		wait = min(wait*2, maxRetry)
	}
}

func (c *Client) state(path string, err error) {
	if c.OnStream != nil {
		c.OnStream(path, err)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Client) open(ctx context.Context, path string) (*http.Response, error) {
	req, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if err := check(res); err != nil {
		res.Body.Close()
		return nil, err
	}
	return res, nil
}

// read parses server-sent events: "data:" lines up to a blank line are one message, ":" lines are comments (the
// pings). A stream that stays silent longer than idle is closed: a connection that died without a word (a laptop
// waking up) would otherwise hang for good.
func (c *Client) read(res *http.Response, data func([]byte) bool) error {
	var once sync.Once
	timer := time.AfterFunc(c.idle, func() { once.Do(func() { res.Body.Close() }) })
	defer timer.Stop()
	defer once.Do(func() { res.Body.Close() })
	br := bufio.NewReaderSize(res.Body, 64<<10)
	var msg []byte
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return err
		}
		timer.Reset(c.idle)
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0:
			if len(msg) > 0 && !data(msg) {
				return nil
			}
			msg = msg[:0]
		case line[0] == ':':
		default:
			field, value, _ := bytes.Cut(line, []byte(":"))
			if string(field) != "data" {
				continue // event:, id:, retry: are not used
			}
			value = bytes.TrimPrefix(value, []byte(" "))
			if len(msg) > 0 {
				msg = append(msg, '\n')
			}
			msg = append(msg, value...)
		}
	}
}

// Apply records an event in a job the way the worker does, so a client keeps its copy of a running job current
// between the full job updates (which come at the start and the end).
func Apply(j *jobs.Job, ev engine.Event) {
	switch ev.Type {
	case "status":
		j.Status = ev.Text
	case "log":
		j.Logs = append(j.Logs, ev.Text)
		if len(j.Logs) > 400 {
			j.Logs = j.Logs[len(j.Logs)-400:]
		}
	case "text":
		j.Text += ev.Text
	case "progress":
		j.Step, j.Total = ev.Step, ev.Total
	case "file":
		j.Files = append(j.Files, ev.File)
		j.Preview = ""
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
		j.Result = ev.Data
	}
}
