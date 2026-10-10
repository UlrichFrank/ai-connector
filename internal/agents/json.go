package agents

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// The json driver: `claude -p --input-format stream-json --output-format stream-json --verbose
// --include-partial-messages …` stays running; a turn is one user line on its stdin, and the turn ends with the
// "result" line. Claude Code reads stdin as a stream, so the process (and its warm caches) serves every turn of
// the conversation. Cancel sends the control request {"subtype":"interrupt"}: Claude Code stops the turn, answers
// with a result of subtype "error_during_execution", and the session goes on.

// interruptWait: how long an interrupted turn may take to end before the process is stopped.
var interruptWait = 10 * time.Second

// errEndedEarly: the process ended before it said anything (a --resume of a conversation that is gone).
var errEndedEarly = errors.New("claude ended before it answered")

type jsonDriver struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan map[string]any
	done   chan struct{}
	stderr *tail

	wmu sync.Mutex // writes to stdin

	mu       sync.Mutex
	claudeID string
	cost     float64 // the session's total so far (Claude Code reports totals)
	output   bool    // it wrote anything
	reqs     int
}

func startJSON(s *Session, args []string) (driver, error) {
	m := s.m
	full := append([]string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages"}, args...)
	if m.has("--permission-prompts") {
		// nobody answers permission prompts of a headless session: a tool outside the allow-list is refused at once
		full = append(full, "--permission-prompts", "none")
	}
	cmd := exec.Command(m.command(), full...)
	cmd.Dir = s.cwd
	cmd.Env = m.environ(s.env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	d := &jsonDriver{cmd: cmd, stdin: stdin, lines: make(chan map[string]any, 1024), done: make(chan struct{}),
		stderr: &tail{max: 8 << 10}}
	cmd.Stderr = d.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting claude: %w", err)
	}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 256<<10), 64<<20)
		for sc.Scan() {
			var ev map[string]any
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue
			}
			d.mu.Lock()
			d.output = true
			if id, _ := ev["session_id"].(string); id != "" {
				d.claudeID = id
			}
			d.mu.Unlock()
			d.lines <- ev
		}
		cmd.Wait()
		close(d.lines)
		close(d.done)
	}()
	return d, nil
}

func (d *jsonDriver) exited() <-chan struct{} { return d.done }

func (d *jsonDriver) claudeSession() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.claudeID
}

func (d *jsonDriver) close() {
	d.wmu.Lock()
	d.stdin.Close() // Claude Code ends when its input ends
	d.wmu.Unlock()
	select {
	case <-d.done:
	case <-time.After(3 * time.Second):
		interrupt(d.cmd.Process)
		select {
		case <-d.done:
		case <-time.After(3 * time.Second):
			d.cmd.Process.Kill()
		}
	}
}

func (d *jsonDriver) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err = d.stdin.Write(append(b, '\n'))
	return err
}

func (d *jsonDriver) turn(ctx context.Context, text string, emit func(Event)) (Result, error) {
	return d.turnContent(ctx, text, emit)
}

// turnContent runs a turn whose user message has this content: a text, or content blocks (images.go).
func (d *jsonDriver) turnContent(ctx context.Context, content any, emit func(Event)) (Result, error) {
	start := time.Now()
	if err := d.send(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}}); err != nil {
		return Result{}, d.ended(err)
	}
	t := &turnState{emit: emit, start: start, streamed: map[string]bool{}, tools: map[string]string{}}
	var deadline <-chan time.Time
	done := ctx.Done()
	interrupted := false
	for {
		select {
		case ev, ok := <-d.lines:
			if !ok {
				return t.result(), d.ended(nil)
			}
			if res, end := t.handle(ev); end {
				res.Duration = time.Since(start)
				d.mu.Lock()
				res.ClaudeSession = d.claudeID
				if total, ok := ev["total_cost_usd"].(float64); ok {
					res.CostUSD = total - d.cost
					if res.CostUSD < 0 {
						res.CostUSD = total
					}
					d.cost = total
				}
				d.mu.Unlock()
				if interrupted {
					return res, ctx.Err()
				}
				if isErr, _ := ev["is_error"].(bool); isErr {
					return res, fmt.Errorf("claude: %s", resultError(ev))
				}
				return res, nil
			}
		case <-done:
			done, interrupted = nil, true
			emit(Event{Type: "status", Text: "interrupting …"})
			if err := d.send(map[string]any{"type": "control_request", "request_id": fmt.Sprintf("interrupt-%d", time.Now().UnixNano()),
				"request": map[string]any{"subtype": "interrupt"}}); err != nil {
				interrupt(d.cmd.Process)
			}
			deadline = time.After(interruptWait)
		case <-deadline:
			// it did not stop: the process goes (the next turn starts a new one, which resumes the conversation)
			d.cmd.Process.Kill()
			return t.result(), ctx.Err()
		}
	}
}

// ended explains a process that is gone.
func (d *jsonDriver) ended(err error) error {
	<-d.done
	d.mu.Lock()
	early := !d.output
	d.mu.Unlock()
	msg := strings.TrimSpace(d.stderr.String())
	if early {
		if msg != "" {
			return fmt.Errorf("%w: %s", errEndedEarly, shorten(msg, 600))
		}
		return errEndedEarly
	}
	if msg == "" && err != nil {
		msg = err.Error()
	}
	if msg == "" {
		msg = "the process ended"
	}
	return fmt.Errorf("claude stopped: %s", shorten(msg, 600))
}

func resultError(ev map[string]any) string {
	if r, _ := ev["result"].(string); r != "" {
		return r
	}
	if errs, ok := ev["errors"].([]any); ok && len(errs) > 0 {
		var s []string
		for _, e := range errs {
			s = append(s, fmt.Sprint(e))
		}
		return strings.Join(s, "; ")
	}
	sub, _ := ev["subtype"].(string)
	return sub
}

// turnState maps Claude Code's stream to events.
type turnState struct {
	emit     func(Event)
	start    time.Time
	first    time.Duration
	text     strings.Builder
	msgID    string
	streamed map[string]bool   // a message whose text came as deltas (its whole text is not sent again)
	tools    map[string]string // tool_use id → name
	writing  map[int]*writingTool
}

// writingTool is a tool call whose input the model is writing (streamed as input_json_delta).
type writingTool struct {
	name  string
	bytes int       // the input so far
	said  time.Time // its last status (or its start)
}

// writingEvery: a tool call's input being written is reported at most this often ("writing create_items … 8 KB").
var writingEvery = 2 * time.Second

// toolShort is a tool's name in a status line: an MCP tool's own name (mcp__vboard__create_items → create_items).
func toolShort(name string) string {
	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		if _, tool, ok := strings.Cut(rest, "__"); ok && tool != "" {
			return tool
		}
	}
	return name
}

// sizeText says a size in bytes, KB or MB.
func sizeText(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%d KB", (n+512)>>10)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
}

func (t *turnState) out(s string) {
	if s == "" {
		return
	}
	if t.first == 0 {
		t.first = time.Since(t.start)
	}
	t.text.WriteString(s)
	t.emit(Event{Type: "text", Text: s})
}

// separate starts a new text block on a new paragraph (text before and after a tool call).
func (t *turnState) separate() {
	if s := t.text.String(); s != "" && !strings.HasSuffix(s, "\n\n") {
		if strings.HasSuffix(s, "\n") {
			t.out("\n")
		} else {
			t.out("\n\n")
		}
	}
}

func (t *turnState) result() Result {
	return Result{Text: t.text.String(), FirstText: t.first}
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func obj(m map[string]any, k string) map[string]any {
	o, _ := m[k].(map[string]any)
	return o
}

func num(m map[string]any, k string) int {
	f, _ := m[k].(float64)
	return int(f)
}

// handle takes one line; end: it was the turn's result.
func (t *turnState) handle(ev map[string]any) (Result, bool) {
	switch str(ev, "type") {
	case "system":
		if str(ev, "subtype") == "status" && str(ev, "status") == "requesting" {
			t.emit(Event{Type: "status", Text: "thinking …"})
		}
	case "stream_event":
		e := obj(ev, "event")
		switch str(e, "type") {
		case "message_start":
			t.msgID = str(obj(e, "message"), "id")
			t.writing = nil
		case "content_block_start":
			cb := obj(e, "content_block")
			switch str(cb, "type") {
			case "text":
				t.separate()
			case "tool_use":
				t.emit(Event{Type: "status", Text: "using " + str(cb, "name") + " …"})
				if t.writing == nil {
					t.writing = map[int]*writingTool{}
				}
				t.writing[num(e, "index")] = &writingTool{name: str(cb, "name"), said: time.Now()}
			}
		case "content_block_delta":
			switch d := obj(e, "delta"); str(d, "type") {
			case "text_delta":
				t.streamed[t.msgID] = true
				t.out(str(d, "text"))
			case "input_json_delta":
				// the model writes a tool call's input (a large one takes minutes): its size now and then
				if w := t.writing[num(e, "index")]; w != nil {
					w.bytes += len(str(d, "partial_json"))
					if time.Since(w.said) >= writingEvery {
						w.said = time.Now()
						t.emit(Event{Type: "status", Text: "writing " + toolShort(w.name) + " … " + sizeText(w.bytes)})
					}
				}
			}
		case "content_block_stop":
			delete(t.writing, num(e, "index"))
		}
	case "assistant":
		msg := obj(ev, "message")
		id := str(msg, "id")
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			cb, _ := b.(map[string]any)
			switch str(cb, "type") {
			case "text":
				if !t.streamed[id] {
					t.separate()
					t.out(str(cb, "text"))
				}
			case "tool_use":
				tid, name := str(cb, "id"), str(cb, "name")
				t.tools[tid] = name
				in, _ := json.Marshal(cb["input"])
				t.emit(Event{Type: "tool", Tool: &ToolCall{ID: tid, Name: name, State: "running", Arguments: in,
					Summary: summarize(cb["input"])}})
			}
		}
	case "user":
		msg := obj(ev, "message")
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			cb, _ := b.(map[string]any)
			if str(cb, "type") != "tool_result" {
				continue
			}
			tid := str(cb, "tool_use_id")
			state := "done"
			if isErr, _ := cb["is_error"].(bool); isErr {
				state = "error"
			}
			t.emit(Event{Type: "tool", Tool: &ToolCall{ID: tid, Name: t.tools[tid], State: state,
				Result: shorten(contentText(cb["content"]), 600)}})
		}
	case "result":
		res := t.result()
		if res.Text == "" {
			res.Text = str(ev, "result")
		}
		u := obj(ev, "usage")
		res.InputTokens = num(u, "input_tokens")
		res.OutputTokens = num(u, "output_tokens")
		res.CacheReadTokens = num(u, "cache_read_input_tokens")
		res.CacheWriteTokens = num(u, "cache_creation_input_tokens")
		res.Turns = num(ev, "num_turns")
		return res, true
	}
	return Result{}, false
}

// contentText is a tool result's text (a string, or text blocks).
func contentText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, p := range x {
			if m, ok := p.(map[string]any); ok && str(m, "type") == "text" {
				parts = append(parts, str(m, "text"))
			}
		}
		return strings.Join(parts, "\n")
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// summarize says what a tool call is about in a few words: its path, pattern, command, URL or query.
func summarize(input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range []string{"file_path", "path", "pattern", "command", "url", "query", "description", "prompt"} {
		if s, ok := m[k].(string); ok && s != "" {
			return shorten(s, 120)
		}
	}
	b, _ := json.Marshal(m)
	return shorten(string(b), 120)
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = bytes.Clone(t.b[len(t.b)-t.max:])
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
