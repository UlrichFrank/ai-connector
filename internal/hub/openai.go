package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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
	since int64
}

// start submits the job to the worker.
func (s *Server) startChat(ctx context.Context, n *node, job map[string]any) (string, *apiError) {
	b, _ := json.Marshal(job)
	resp, err := s.fleet.request(ctx, n, http.MethodPost, "/api/jobs", bytes.NewReader(b), "application/json")
	if err != nil {
		return "", &apiError{http.StatusBadGateway, "worker_error", n.name + ": " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if resp.StatusCode == http.StatusBadRequest {
			return "", badRequest("%s", msg)
		}
		return "", &apiError{http.StatusBadGateway, "worker_error", fmt.Sprintf("%s: %s %s", n.name, resp.Status, msg)}
	}
	var j struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &j) != nil || j.ID == "" {
		return "", &apiError{http.StatusBadGateway, "worker_error", n.name + ": no job id"}
	}
	return j.ID, nil
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

// follow waits for the job's end, passing its text on as it streams (onText) and calling tick every chatPoll. It
// returns the job as the worker has it then.
func (c *chatRun) follow(ctx context.Context, events <-chan []byte, onText func(string), tick func()) (*chatJob, error) {
	key := []byte(`"jobId":"` + c.id + `"`)
	poll := time.NewTicker(chatPoll)
	defer poll.Stop()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case data := <-events:
			if !bytes.Contains(data, key) {
				continue
			}
			var u struct {
				JobID string `json:"jobId"`
				Job   *struct {
					State string `json:"state"`
				} `json:"job"`
				Event *struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"event"`
			}
			if json.Unmarshal(data, &u) != nil || u.JobID != c.id {
				continue
			}
			if u.Event != nil && u.Event.Type == "text" && onText != nil {
				onText(u.Event.Text)
			}
			if u.Job != nil && finished(u.Job.State) {
				return c.get(ctx)
			}
		case <-poll.C:
			if tick != nil {
				tick()
			}
			j, err := c.get(ctx)
			switch {
			case err == nil && finished(j.State):
				return j, nil
			case err == nil:
				misses = 0
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
	n, m, e := s.place(r.Context(), req.Model, "chat")
	if e != nil {
		e.openAI(w)
		return
	}
	job, _ := req.job(m.ID)
	if c, ok := clientOf(r); ok {
		job["client"] = c.Name
	}
	events, stop := s.fleet.subscribe() // before the job exists: none of its events is missed
	defer stop()
	run := &chatRun{s: s, n: n, model: m.ID, since: time.Now().Unix()}
	if req.Stream {
		run.stream(w, r, events, job, req.maxTokens())
		return
	}
	id, e := s.startChat(r.Context(), n, job)
	if e != nil {
		e.openAI(w)
		return
	}
	run.id = id
	j, err := run.follow(r.Context(), events, nil, nil)
	if err != nil {
		if r.Context().Err() != nil {
			run.cancel()
			return
		}
		(&apiError{http.StatusBadGateway, "worker_error", err.Error()}).openAI(w)
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
		"id": id, "object": "chat.completion", "created": run.since, "model": run.model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": j.finishReason(req.maxTokens())}},
		"usage":   j.usage(),
	})
}

// stream answers as server-sent events, chunk by chunk.
// The stream opens before the job is submitted (a ": ping" at once), so the client sees it open while the job waits
// in the queue; a failure from then on is sent in the stream.
func (c *chatRun) stream(w http.ResponseWriter, r *http.Request, events <-chan []byte, job map[string]any, limit int) {
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
	id, e := c.s.startChat(r.Context(), c.n, job)
	if e != nil {
		fail(e)
		return
	}
	c.id = id
	send(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	sp := &thinkSplit{emit: func(thought bool, s string) {
		if thought {
			send(chunk(map[string]any{"reasoning_content": s}, nil))
		} else {
			send(chunk(map[string]any{"content": s}, nil))
		}
	}}
	var got strings.Builder
	j, err := c.follow(r.Context(), events, func(s string) { got.WriteString(s); sp.write(s) }, func() {
		io.WriteString(w, ": ping\n\n") // while it waits in the queue or loads its model
		flush()
	})
	if err != nil {
		if r.Context().Err() != nil {
			c.cancel()
			return
		}
		fail(&apiError{http.StatusBadGateway, "worker_error", err.Error()})
		return
	}
	// the job's text is the whole answer: what the stream missed at the end comes from it
	if rest, ok := strings.CutPrefix(j.Text, got.String()); ok {
		sp.write(rest)
	} else {
		log.Printf("chat %s: the streamed text is not the job's (events were dropped)", c.id)
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
