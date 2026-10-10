package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

// POST /v1/chat/completions (docs/api.md): the request becomes a chat job in the chosen worker's queue, so it shows
// in the job lists and takes its turn with everything else. Its text comes from the workers' event stream (the
// fleet's); the job as the worker has it at the end is what counts (events of a slow listener can be dropped).

// chatPoll: how often a waiting chat asks the worker for its job (in case the end was missed) and pings the client.
var chatPoll = 5 * time.Second

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Stream              bool            `json:"stream"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	PresencePenalty     *float64        `json:"presence_penalty"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Tools               []chatTool      `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	Think               json.RawMessage `json:"think"` // Ollama's: true, or a level ("low" …)
	ChatTemplateKwargs  struct {
		EnableThinking bool `json:"enable_thinking"`
	} `json:"chat_template_kwargs"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"` // a JSON string; some clients send the object itself
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// thinking: off unless asked for, in any of the ways clients ask.
func (c *chatRequest) thinking() bool {
	if e := strings.ToLower(c.ReasoningEffort); e != "" && e != "none" {
		return true
	}
	if c.ChatTemplateKwargs.EnableThinking {
		return true
	}
	var b bool
	if json.Unmarshal(c.Think, &b) == nil {
		return b
	}
	var level string
	if json.Unmarshal(c.Think, &level) == nil {
		return level != "" && level != "none" && level != "false"
	}
	return false
}

// maxTokens is the answer's limit (0: the model's default).
func (c *chatRequest) maxTokens() int {
	if c.MaxCompletionTokens > 0 {
		return c.MaxCompletionTokens
	}
	return max(c.MaxTokens, 0)
}

// messageText is a message's content: a text, or text parts joined (other parts are refused).
func messageText(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("content is a text or a list of parts")
	}
	var texts []string
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("content parts of type %q are not taken (text only)", p.Type)
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n"), nil
}

// job is the chat job of the request (an engine.Request as JSON; the hub does not import the engine).
func (c *chatRequest) job(model string) (map[string]any, *apiError) {
	if len(c.Messages) == 0 {
		return nil, badRequest("messages are missing")
	}
	msgs := make([]map[string]any, len(c.Messages))
	for i, m := range c.Messages {
		role := m.Role
		if role == "developer" { // the newer name of the system message
			role = "system"
		}
		switch role {
		case "system", "user", "assistant", "tool":
		default:
			return nil, badRequest("messages[%d]: unknown role %q", i, m.Role)
		}
		text, err := messageText(m.Content)
		if err != nil {
			return nil, badRequest("messages[%d]: %v", i, err)
		}
		out := map[string]any{"role": role, "content": text}
		if role == "tool" {
			out["toolCallId"] = m.ToolCallID
		}
		if role == "assistant" && len(m.ToolCalls) > 0 {
			calls := make([]map[string]string, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				args := "{}"
				var s string
				if t := bytes.TrimSpace(tc.Function.Arguments); json.Unmarshal(t, &s) == nil {
					args = s
				} else if len(t) > 0 && string(t) != "null" {
					args = string(t)
				}
				calls[j] = map[string]string{"id": tc.ID, "name": tc.Function.Name, "arguments": args}
			}
			out["toolCalls"] = calls
		}
		msgs[i] = out
	}
	job := map[string]any{"model": model, "messages": msgs, "think": c.thinking()}
	if e := strings.ToLower(strings.TrimSpace(c.ReasoningEffort)); strings.HasPrefix(strings.ToLower(model), claudePrefix) &&
		slices.Contains(claudeEfforts, e) {
		job["effort"] = e // Claude Code's --effort
	}
	if string(bytes.TrimSpace(c.ToolChoice)) != `"none"` {
		var tools []map[string]any
		for i, t := range c.Tools {
			if t.Type != "" && t.Type != "function" || t.Function.Name == "" {
				return nil, badRequest("tools[%d]: a function with a name", i)
			}
			tool := map[string]any{"name": t.Function.Name, "description": t.Function.Description}
			if len(t.Function.Parameters) > 0 {
				tool["parameters"] = t.Function.Parameters
			}
			tools = append(tools, tool)
		}
		if len(tools) > 0 {
			job["tools"] = tools
		}
	}
	if t := c.Temperature; t != nil {
		if *t <= 0 {
			job["greedy"] = true
		} else {
			job["temperature"] = *t
		}
	}
	if c.TopP != nil && *c.TopP > 0 {
		job["topP"] = *c.TopP
	}
	if c.PresencePenalty != nil {
		job["presencePenalty"] = *c.PresencePenalty
	}
	if n := c.maxTokens(); n > 0 {
		job["maxTokens"] = n
	}
	return job, nil
}

// chatJob is the part of a worker's job a chat completion reads.
type chatJob struct {
	State  string         `json:"state"`
	Text   string         `json:"text"`
	Error  string         `json:"error"`
	Stats  map[string]any `json:"stats"`
	Result struct {
		ToolCalls []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"toolCalls"`
	} `json:"result"`
}

func finished(state string) bool { return state == "done" || state == "failed" || state == "canceled" }

func (j *chatJob) stat(k string) int {
	v, _ := j.Stats[k].(float64)
	return int(v)
}

func (j *chatJob) usage() map[string]int {
	p, c := j.stat("promptTokens"), j.stat("tokens")
	return map[string]int{"prompt_tokens": p, "completion_tokens": c, "total_tokens": p + c}
}

// toolCalls are the calls of the caller's tools, as OpenAI writes them (with index: the stream's form).
func (j *chatJob) toolCalls() []map[string]any {
	var out []map[string]any
	for i, c := range j.Result.ToolCalls {
		out = append(out, map[string]any{"index": i, "id": c.ID, "type": "function",
			"function": map[string]string{"name": c.Name, "arguments": c.Arguments}})
	}
	return out
}

// finishReason: tool_calls, length (the answer reached its limit) or stop.
func (j *chatJob) finishReason(limit int) string {
	if len(j.Result.ToolCalls) > 0 {
		return "tool_calls"
	}
	if l := j.stat("maxTokens"); l > 0 { // what the worker ran with (its default when the request set none)
		limit = l
	}
	if limit > 0 && j.stat("tokens") >= limit {
		return "length"
	}
	return "stop"
}

// failure is a job that ended without an answer.
func (j *chatJob) failure() *apiError {
	switch j.State {
	case "failed":
		return &apiError{http.StatusBadGateway, "worker_error", j.Error}
	case "canceled":
		return &apiError{http.StatusBadGateway, "worker_error", "the job was canceled"}
	}
	return nil
}

// chatRun is one chat completion: its job on a worker.
type chatRun struct {
	s     *Server
	n     *node
	id    string // the job's id (with the worker's route)
	model string
	owner string // whose job it is: set on a claude-code model's job (run), "" for the others
	since int64
}

// start submits the job to the worker: its id and state ("queued", "running" …).
func (s *Server) startChat(ctx context.Context, n *node, job map[string]any) (string, string, *apiError) {
	b, _ := json.Marshal(job)
	resp, err := s.fleet.request(ctx, n, http.MethodPost, "/api/jobs", bytes.NewReader(b), "application/json")
	if err != nil {
		return "", "", &apiError{http.StatusBadGateway, "worker_error", n.name + ": " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if resp.StatusCode == http.StatusBadRequest {
			return "", "", badRequest("%s", msg)
		}
		return "", "", &apiError{http.StatusBadGateway, "worker_error", fmt.Sprintf("%s: %s %s", n.name, resp.Status, msg)}
	}
	var j struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if json.Unmarshal(raw, &j) != nil || j.ID == "" {
		return "", "", &apiError{http.StatusBadGateway, "worker_error", n.name + ": no job id"}
	}
	if j.State == "" {
		j.State = "queued"
	}
	return j.ID, j.State, nil
}

// get asks the worker for the job.
func (c *chatRun) get(ctx context.Context) (*chatJob, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var j chatJob
	if err := c.s.fleet.getJSON(ctx, c.n, "/api/jobs/"+c.id, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// cancel stops the job (the client went away).
func (c *chatRun) cancel() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.s.fleet.request(ctx, c.n, http.MethodPost, "/api/jobs/"+c.id+"/cancel", nil, "")
	if err != nil {
		log.Printf("cancel %s on %q: %v", c.id, c.n.name, err)
		return
	}
	resp.Body.Close()
}

// chatHooks are what a caller of run hears while its job runs (each may be nil). They are called one at a time, from
// run's goroutine.
type chatHooks struct {
	// started: the job is in the worker's queue (the run's id is set)
	started func()
	// text: a piece of the job's text as it streams; at the end what the event stream missed, so that the pieces
	// add up to the job's whole text
	text func(string)
	// tick: at once when the job is in the queue, again when its state changes (it starts), and every chatPoll while
	// it waits or runs, with its state as last heard ("queued", "running")
	tick func(state string)
	// event: every other event of the job as it comes (status, tool …)
	event func(typ, text string, data map[string]any)
}

// run submits the job to the worker and follows it to its end; it returns the job as the worker has it then. When
// ctx ends first, the job is canceled (the error returned is then ctx's). feed must be made (fleet.followJob) before
// the call, so that none of the job's events is missed. Shared by /v1/chat/completions and the connectors
// (vboard.go).
func (c *chatRun) run(ctx context.Context, feed *jobFeed, job map[string]any, h chatHooks) (*chatJob, *apiError) {
	delete(job, "owner") // the server says whose a job is (placeChat)
	if c.owner != "" {
		job["owner"] = c.owner
	}
	id, state, e := c.s.startChat(ctx, c.n, job)
	if e != nil {
		return nil, e
	}
	c.id = id
	feed.setJob(id)
	if h.started != nil {
		h.started()
	}
	var got strings.Builder
	onText := func(s string) {
		got.WriteString(s)
		if h.text != nil {
			h.text(s)
		}
	}
	j, err := c.follow(ctx, feed, state, onText, h.tick, h.event)
	if err != nil {
		if ctx.Err() != nil {
			c.cancel()
		}
		return nil, &apiError{http.StatusBadGateway, "worker_error", err.Error()}
	}
	// the job's text is the whole answer: what the stream missed at the end comes from it
	if h.text != nil {
		if rest, ok := strings.CutPrefix(j.Text, got.String()); ok {
			if rest != "" {
				h.text(rest)
			}
		} else {
			log.Printf("chat %s: the streamed text is not the job's (events were dropped)", c.id)
		}
	}
	return j, nil
}

// follow waits for the job's end, passing its text on as it streams (onText), calling tick at once with the state
// the job started in, on every change of its state and every chatPoll, and onEvent (if any) with its other events.
// It returns the job as the worker has it then.
//
// The text is passed on only where it belongs: a text event says where its piece starts in the job's text ("at",
// jobs.Update.At), so a piece that is already passed on is not passed again, and a gap (a piece the feed never got:
// the worker's own stream drops for a slow reader) is filled from the job (its text so far) before what follows — or,
// when the job cannot be asked, nothing more streams and run sends the rest at the end. Pieces without "at" (older
// workers) are passed on as they come.
func (c *chatRun) follow(ctx context.Context, feed *jobFeed, state string, onText func(string), tick func(string),
	onEvent func(typ, text string, data map[string]any)) (*chatJob, error) {
	poll := time.NewTicker(chatPoll)
	defer poll.Stop()
	misses := 0
	if tick != nil {
		tick(state)
	}
	text := &textTrack{out: onText, fetch: func() (string, error) {
		j, err := c.get(ctx)
		if err != nil {
			return "", err
		}
		return j.Text, nil
	}, id: c.id}
	handle := func(data []byte) (done bool) {
		var u struct {
			JobID string `json:"jobId"`
			Job   *struct {
				State string `json:"state"`
			} `json:"job"`
			Event *struct {
				Type string         `json:"type"`
				Text string         `json:"text"`
				Data map[string]any `json:"data"`
			} `json:"event"`
			At *int `json:"at"`
		}
		if json.Unmarshal(data, &u) != nil || u.JobID != c.id {
			return false
		}
		if u.Event != nil && u.Event.Type == "text" {
			text.piece(u.At, u.Event.Text)
		} else if u.Event != nil && onEvent != nil {
			onEvent(u.Event.Type, u.Event.Text, u.Event.Data)
		}
		if u.Job != nil && u.Job.State != "" && u.Job.State != state {
			state = u.Job.State
			if tick != nil && !finished(state) {
				tick(state) // it started: said at once
			}
		}
		return u.Job != nil && finished(u.Job.State)
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-feed.ready:
			for _, data := range feed.take() {
				if handle(data) {
					return c.get(ctx)
				}
			}
		case <-poll.C:
			if tick != nil {
				tick(state)
			}
			j, err := c.get(ctx)
			switch {
			case err == nil && finished(j.State):
				return j, nil
			case err == nil:
				misses = 0
				state = j.State
			case ctx.Err() != nil:
				return nil, ctx.Err()
			default:
				if misses++; misses >= 3 { // gone for a while (the worker's tunnel dropped and stayed down)
					return nil, fmt.Errorf("%s does not answer: %w", c.n.name, err)
				}
			}
		}
	}
}

// textTrack passes a job's text on as its pieces come, each where it belongs (follow): "at" is where the piece starts
// in the job's text.
type textTrack struct {
	out     func(string)
	fetch   func() (string, error) // the job's text so far
	id      string
	sent    int  // bytes of the job's text passed on
	stopped bool // a gap could not be filled: nothing more is passed on (run sends the rest at the end)
}

func (t *textTrack) piece(at *int, s string) {
	switch {
	case t.out == nil || t.stopped:
	case at == nil || *at == t.sent: // in order (or an older worker that does not say)
		t.out(s)
		t.sent += len(s)
	case *at < t.sent: // passed on already (filled from the job)
		if end := *at + len(s); end > t.sent {
			t.out(s[t.sent-*at:])
			t.sent = end
		}
	default: // a gap: the job has the text in between
		text, err := t.fetch()
		if err != nil || len(text) < *at+len(s) {
			t.stopped = true
			log.Printf("chat %s: a gap in the streamed text (at %d, %d passed on); the rest comes at the end", t.id, *at, t.sent)
			return
		}
		t.out(text[t.sent:])
		t.sent = len(text)
	}
}

// chatFor is the person a chat is for (a device's user, a client token's or a connector's owner) and how they came
// (viaDevice, viaClient, viaConnector).
type chatFor struct{ user, via string }

// placeChat picks the worker for a chat model and makes the run that submits and follows its job; shared by
// /v1/chat/completions and the connectors. A claude-code model goes the way POST /api/jobs sends it (agents.go):
// only to a worker of the person it is for, who is then the job's owner (run sets it).
func (s *Server) placeChat(ctx context.Context, name string, who chatFor) (*chatRun, *apiError) {
	routed, _ := s.fleet.pick(ctx)
	if !isClaude(routed, name) {
		n, m, e := s.place(ctx, name, "chat")
		if e != nil {
			return nil, e
		}
		return &chatRun{s: s, n: n, model: m.ID, since: time.Now().Unix()}, nil
	}
	model := strings.ToLower(strings.TrimSpace(name))
	if fm, ok := findModel(fleetModels(routed), name); ok {
		model = fm.ID
	}
	n, no := s.claudeNode(who.user, routed, "", who.via)
	if no != nil {
		code := strings.ReplaceAll(no.name, "-", "_") // no-worker → no_worker, as the other refusals here
		if no.name == "offline" {
			code = "no_worker"
		}
		return nil, &apiError{no.status, code, no.message}
	}
	return &chatRun{s: s, n: n, model: model, owner: who.user, since: time.Now().Unix()}, nil
}

// v1Chat: POST /v1/chat/completions.
func (s *Server) v1Chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if e := readBody(w, r, &req); e != nil {
		e.openAI(w)
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		badRequest("model is missing").openAI(w)
		return
	}
	if _, e := req.job(""); e != nil { // a bad request is told before a worker is looked for
		e.openAI(w)
		return
	}
	run, e := s.placeChat(r.Context(), req.Model, chatFor{userOf(r), viaOf(r)})
	if e != nil {
		e.openAI(w)
		return
	}
	job, _ := req.job(run.model)
	if c, ok := clientOf(r); ok {
		job["client"] = c.Name
	}
	feed := s.fleet.followJob() // before the job exists: none of its events is missed
	defer feed.close()
	if req.Stream {
		run.stream(w, r, feed, job, req.maxTokens())
		return
	}
	j, e := run.run(r.Context(), feed, job, chatHooks{})
	if e != nil {
		if r.Context().Err() == nil {
			e.openAI(w)
		}
		return
	}
	if e := j.failure(); e != nil {
		e.openAI(w)
		return
	}
	var reasoning, content strings.Builder
	sp := &thinkSplit{emit: func(thought bool, s string) {
		if thought {
			reasoning.WriteString(s)
		} else {
			content.WriteString(s)
		}
	}}
	sp.write(j.Text)
	sp.flush()
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if calls := j.toolCalls(); len(calls) > 0 {
		for _, c := range calls {
			delete(c, "index") // the stream's field
		}
		msg["tool_calls"] = calls
		if content.Len() == 0 {
			msg["content"] = nil
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": run.id, "object": "chat.completion", "created": run.since, "model": run.model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": j.finishReason(req.maxTokens())}},
		"usage":   j.usage(),
	})
}

// stream answers as server-sent events, chunk by chunk.
// The stream opens before the job is submitted (a ": ping" at once), so the client sees it open while the job waits
// in the queue; a failure from then on is sent in the stream.
func (c *chatRun) stream(w http.ResponseWriter, r *http.Request, feed *jobFeed, job map[string]any, limit int) {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flush()
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": c.id, "object": "chat.completion.chunk", "created": c.since, "model": c.model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	fail := func(e *apiError) {
		send(e.body())
		io.WriteString(w, "data: [DONE]\n\n")
		flush()
	}
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, ": ping\n\n")
	flush()
	sp := &thinkSplit{emit: func(thought bool, s string) {
		if thought {
			send(chunk(map[string]any{"reasoning_content": s}, nil))
		} else {
			send(chunk(map[string]any{"content": s}, nil))
		}
	}}
	j, e := c.run(r.Context(), feed, job, chatHooks{
		started: func() { send(chunk(map[string]any{"role": "assistant", "content": ""}, nil)) },
		text:    sp.write,
		tick: func(string) {
			io.WriteString(w, ": ping\n\n") // while it waits in the queue or loads its model
			flush()
		},
	})
	if e != nil {
		if r.Context().Err() == nil {
			fail(e)
		}
		return
	}
	sp.flush()
	if e := j.failure(); e != nil {
		fail(e)
		return
	}
	delta := map[string]any{}
	if calls := j.toolCalls(); len(calls) > 0 {
		delta["tool_calls"] = calls
	}
	last := chunk(delta, j.finishReason(limit))
	last["usage"] = j.usage()
	send(last)
	io.WriteString(w, "data: [DONE]\n\n")
	flush()
}

// thinkSplit separates a chat's text into the thought (<think>…</think>, reasoning_content) and the answer
// (content), piece by piece as it streams. A tag can arrive split across pieces, so an end of the text that could
// begin one is held back until the next piece. The newlines around the thought are dropped (the templates put them
// there).
type thinkSplit struct {
	emit func(thought bool, s string)

	buf      string
	thinking bool
	answered bool   // the answer has begun: a later <think> is text
	thought  bool   // the thought has begun (its leading newlines are dropped)
	nl       string // newlines at the thought's end so far, held back
}

func (t *thinkSplit) write(s string) {
	t.buf += s
	for {
		tag := "</think>"
		if !t.thinking {
			tag = "<think>"
			// a thought only opens the text: once the answer begins with anything else, it is all answer
			lead := strings.TrimLeft(t.buf, "\n \t")
			if t.answered || !strings.HasPrefix(lead, tag) && !strings.HasPrefix(tag, lead) {
				t.out(t.buf)
				t.buf = ""
				return
			}
		}
		if i := strings.Index(t.buf, tag); i >= 0 {
			t.out(t.buf[:i])
			t.buf = t.buf[i+len(tag):]
			t.thinking = !t.thinking
			t.nl = ""
			continue
		}
		keep := partialTag(t.buf, tag)
		t.out(t.buf[:len(t.buf)-keep])
		t.buf = t.buf[len(t.buf)-keep:]
		return
	}
}

// flush passes on what was held back (the text ended).
func (t *thinkSplit) flush() {
	t.out(t.buf)
	t.buf = ""
}

func (t *thinkSplit) out(s string) {
	if t.thinking {
		if !t.thought {
			if s = strings.TrimLeft(s, "\n"); s == "" {
				return
			}
			t.thought = true
		}
		s = t.nl + s
		body := strings.TrimRight(s, "\n")
		t.nl = s[len(body):]
		if body != "" {
			t.emit(true, body)
		}
		return
	}
	if !t.answered {
		if s = strings.TrimLeft(s, "\n"); s == "" {
			return
		}
		t.answered = true
	}
	t.emit(false, s)
}

// partialTag is the length of the longest end of s that begins tag.
func partialTag(s, tag string) int {
	for n := min(len(tag)-1, len(s)); n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}
