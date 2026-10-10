package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The tty driver: an interactive `claude` in a pseudo terminal, kept running. People attach to it from the browser
// (Terminal: the bytes both ways, and the size). A turn from the APIs is typed into it (a bracketed paste and
// Enter); its answer comes from Claude Code's hooks: the session's --settings file has hooks for SessionStart,
// UserPromptSubmit, PreToolUse, PostToolUse and Stop that append their input (JSON) to <ctl>/hooks.jsonl, which
// the driver follows. The Stop hook's last_assistant_message is the answer (else the transcript's last assistant
// text). There is no token streaming in this driver.

// hookEvents are the hooks the driver listens to.
var hookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}

// scrollback: what a viewer that attaches later sees first.
const scrollback = 256 << 10

// readyWait: how long a turn waits for Claude Code to start (SessionStart).
var readyWait = 60 * time.Second

// ptyProcess is the pseudo terminal and the process in it (tty_unix.go; none on Windows).
type ptyProcess interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
	Wait() error
	Interrupt()
	Kill()
}

type ttyDriver struct {
	s     *Session
	p     ptyProcess
	hooks string // <ctl>/hooks.jsonl
	done  chan struct{}
	ready chan struct{}
	hookC chan map[string]any
	stop  chan struct{}

	wmu sync.Mutex

	mu       sync.Mutex
	screen   []byte
	viewers  map[int]chan []byte
	next     int
	claudeID string
	trusted  bool   // the trust question was answered
	plain    []byte // the recent output without escape sequences (to see the trust question)
	started  time.Time
}

// hookCommand appends the hook's input to the file (concatenated JSON values; the driver decodes them one by one).
func hookCommand(file string) string {
	return "cat >> " + shellQuote(file)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func startTTY(s *Session, args []string) (driver, error) {
	hooks := filepath.Join(s.ctl, "hooks.jsonl")
	_ = os.Remove(hooks)
	if err := os.WriteFile(hooks, nil, 0o600); err != nil {
		return nil, err
	}
	h := map[string]any{}
	for _, ev := range hookEvents {
		h[ev] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookCommand(hooks)}}}}
	}
	settings := filepath.Join(s.ctl, "settings.json")
	if err := writeJSONFile(settings, map[string]any{"hooks": h}); err != nil {
		return nil, err
	}
	args = append([]string{"--settings", settings}, args...)
	p, err := startPTY(s.m.command(), args, s.cwd, s.m.environ("TERM=xterm-256color"), 120, 40)
	if err != nil {
		return nil, err
	}
	d := &ttyDriver{s: s, p: p, hooks: hooks, done: make(chan struct{}), ready: make(chan struct{}),
		hookC: make(chan map[string]any, 256), stop: make(chan struct{}), viewers: map[int]chan []byte{}, started: time.Now()}
	go d.read()
	go d.follow()
	return d, nil
}

func (d *ttyDriver) exited() <-chan struct{} { return d.done }

func (d *ttyDriver) claudeSession() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.claudeID
}

func (d *ttyDriver) close() {
	select {
	case <-d.done:
		return
	default:
	}
	d.p.Interrupt()
	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
		d.p.Kill()
		<-d.done
	}
}

// read copies the terminal's output to the scrollback and the viewers.
func (d *ttyDriver) read() {
	buf := make([]byte, 32<<10)
	for {
		n, err := d.p.Read(buf)
		if n > 0 {
			d.output(bytes.Clone(buf[:n]))
		}
		if err != nil {
			break
		}
	}
	d.p.Wait()
	d.p.Close()
	close(d.stop)
	d.mu.Lock()
	for id, ch := range d.viewers {
		close(ch)
		delete(d.viewers, id)
	}
	d.mu.Unlock()
	close(d.done)
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]|[\s\x00-\x1f]+`)

func (d *ttyDriver) output(b []byte) {
	d.mu.Lock()
	d.screen = append(d.screen, b...)
	if len(d.screen) > scrollback {
		d.screen = bytes.Clone(d.screen[len(d.screen)-scrollback:])
	}
	for id, ch := range d.viewers {
		select {
		case ch <- b:
		default: // a viewer that does not keep up is dropped (it attaches again and gets the scrollback)
			close(ch)
			delete(d.viewers, id)
		}
	}
	answer := false
	if !d.trusted && d.s.opts.Cwd == "" && *d.s.m.Config.AutoTrust && time.Since(d.started) < 2*time.Minute {
		d.plain = append(d.plain, bytes.ToLower(ansi.ReplaceAll(b, nil))...)
		if len(d.plain) > 8<<10 {
			d.plain = d.plain[len(d.plain)-8<<10:]
		}
		if bytes.Contains(d.plain, []byte("itrustthisfolder")) || bytes.Contains(d.plain, []byte("yes,itrust")) {
			d.trusted, answer = true, true
		}
	}
	d.mu.Unlock()
	if answer {
		// Claude Code asks whether to trust a new folder ("No, exit" is chosen): the session's own new folder is
		// trusted — down to "Yes, I trust this folder", Enter
		go func() {
			time.Sleep(400 * time.Millisecond)
			d.Write([]byte("\x1b[B"))
			time.Sleep(250 * time.Millisecond)
			d.Write([]byte("\r"))
		}()
	}
}

// follow reads the hook log as it grows.
func (d *ttyDriver) follow() {
	var off int64
	var pending []byte
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
		}
		f, err := os.Open(d.hooks)
		if err != nil {
			continue
		}
		f.Seek(off, io.SeekStart)
		b, _ := io.ReadAll(f)
		f.Close()
		if len(b) == 0 {
			continue
		}
		off += int64(len(b))
		pending = append(pending, b...)
		used := 0
		for used < len(pending) {
			rest := pending[used:]
			dec := json.NewDecoder(bytes.NewReader(rest))
			var ev map[string]any
			err := dec.Decode(&ev)
			var typ *json.UnmarshalTypeError
			if err == nil || errors.As(err, &typ) {
				used += int(dec.InputOffset())
				if err == nil {
					d.hook(ev)
				}
				continue
			}
			var syn *json.SyntaxError
			if errors.As(err, &syn) { // not JSON: skip to the next line
				if i := bytes.IndexByte(rest, '\n'); i >= 0 {
					used += i + 1
					continue
				}
			}
			break // the rest is not complete yet
		}
		pending = bytes.Clone(pending[used:])
	}
}

func (d *ttyDriver) hook(ev map[string]any) {
	if id := str(ev, "session_id"); id != "" {
		d.mu.Lock()
		d.claudeID = id
		d.mu.Unlock()
	}
	if str(ev, "hook_event_name") == "SessionStart" {
		select {
		case <-d.ready:
		default:
			close(d.ready)
		}
		return
	}
	select {
	case d.hookC <- ev:
	default:
	}
}

func (d *ttyDriver) viewerCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.viewers)
}

// Attach: see Terminal.
func (d *ttyDriver) Attach() ([]byte, <-chan []byte, func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ch := make(chan []byte, 1024)
	snap := bytes.Clone(d.screen)
	select {
	case <-d.done:
		close(ch)
		return snap, ch, func() {}
	default:
	}
	d.next++
	id := d.next
	d.viewers[id] = ch
	d.s.mu.Lock()
	d.s.lastUsed = time.Now()
	d.s.mu.Unlock()
	return snap, ch, func() {
		d.mu.Lock()
		if c, ok := d.viewers[id]; ok {
			close(c)
			delete(d.viewers, id)
		}
		d.mu.Unlock()
		d.s.mu.Lock()
		d.s.lastUsed = time.Now()
		d.s.mu.Unlock()
	}
}

// Write types into the terminal.
func (d *ttyDriver) Write(p []byte) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err := d.p.Write(p)
	return err
}

// Resize sets the terminal's size.
func (d *ttyDriver) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 || cols > 1000 || rows > 1000 {
		return errors.New("a size of 1–1000 columns and rows")
	}
	return d.p.Resize(cols, rows)
}

func (d *ttyDriver) turn(ctx context.Context, text string, emit func(Event)) (Result, error) {
	start := time.Now()
	emit(Event{Type: "status", Text: "waiting for Claude Code …"})
	select {
	case <-d.ready:
	case <-d.done:
		return Result{}, errors.New("claude stopped")
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-time.After(readyWait):
		return Result{}, errors.New("Claude Code did not start in the terminal (attach to it to see why)")
	}
	// what the hooks said before this turn (a person's own turns) is not this turn's
	for drained := false; !drained; {
		select {
		case <-d.hookC:
		default:
			drained = true
		}
	}
	// a bracketed paste keeps newlines in the prompt; Enter sends it
	if err := d.Write([]byte("\x1b[200~" + text + "\x1b[201~")); err != nil {
		return Result{}, err
	}
	time.Sleep(150 * time.Millisecond)
	if err := d.Write([]byte("\r")); err != nil {
		return Result{}, err
	}
	emit(Event{Type: "status", Text: "Claude Code is working …"})
	tools := map[string]string{}
	submitted := false // the prompt reached Claude Code (a Stop before it ends an earlier turn)
	for {
		select {
		case <-ctx.Done():
			d.Write([]byte("\x1b")) // Esc interrupts Claude Code's turn
			return Result{Duration: time.Since(start), ClaudeSession: d.claudeSession()}, ctx.Err()
		case <-d.done:
			return Result{}, errors.New("claude stopped")
		case ev := <-d.hookC:
			switch str(ev, "hook_event_name") {
			case "UserPromptSubmit":
				// this turn's prompt (not one a person typed meanwhile)
				p, ok := ev["prompt"].(string)
				submitted = submitted || !ok || squash(p) == squash(text)
			case "PreToolUse":
				id, name := str(ev, "tool_use_id"), str(ev, "tool_name")
				if id == "" {
					id = name + "-" + time.Now().Format("150405.000")
				}
				tools[name] = id
				in, _ := json.Marshal(ev["tool_input"])
				emit(Event{Type: "tool", Tool: &ToolCall{ID: id, Name: name, State: "running", Arguments: in, Summary: summarize(ev["tool_input"])}})
			case "PostToolUse":
				id, name := str(ev, "tool_use_id"), str(ev, "tool_name")
				if id == "" {
					id = tools[name]
				}
				emit(Event{Type: "tool", Tool: &ToolCall{ID: id, Name: name, State: "done", Result: shorten(contentText(ev["tool_response"]), 600)}})
			case "Stop":
				if !submitted {
					continue
				}
				answer := str(ev, "last_assistant_message")
				if answer == "" {
					answer = lastAssistant(str(ev, "transcript_path"))
				}
				if answer != "" {
					emit(Event{Type: "text", Text: answer})
				}
				return Result{Text: answer, Duration: time.Since(start), ClaudeSession: d.claudeSession(), Turns: 1}, nil
			}
		}
	}
}

// lastAssistant is the text of the transcript's last assistant message(s) after the last prompt.
func lastAssistant(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var parts []string
	for _, line := range bytes.Split(b, []byte("\n")) {
		var e struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		var blocks []map[string]any
		json.Unmarshal(e.Message.Content, &blocks)
		switch e.Type {
		case "user":
			var s string
			if json.Unmarshal(e.Message.Content, &s) == nil {
				parts = nil // a prompt: what came before is an earlier turn
			}
		case "assistant":
			for _, bl := range blocks {
				if str(bl, "type") == "text" {
					parts = append(parts, str(bl, "text"))
				}
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// squash compares prompts without their whitespace (the terminal may change line ends).
func squash(s string) string { return strings.Join(strings.Fields(s), " ") }
