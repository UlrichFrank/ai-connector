package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) emit(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, e := range r.events {
		if e.Type == "text" {
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

func (r *recorder) tools() []ToolCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ToolCall
	for _, e := range r.events {
		if e.Type == "tool" {
			out = append(out, *e.Tool)
		}
	}
	return out
}

// One process serves every turn of a conversation; text comes as deltas; the result has usage and the turn's cost.
func TestSessionReuse(t *testing.T) {
	m, log := fakeManager(t)
	o := Options{Key: "board-1", Owner: "alice", MCP: []MCPServer{{Name: "vboard", URL: "http://x/mcp", Headers: map[string]string{"Authorization": "Bearer t"}}}}
	var id string
	for i, q := range []string{"one", "two", "three"} {
		r := &recorder{}
		res, err := m.Turn(context.Background(), o, q, r.emit)
		if err != nil {
			t.Fatal(err)
		}
		if r.text() != "echo: "+q || res.Text != "echo: "+q {
			t.Fatalf("turn %d: text %q, result %q", i, r.text(), res.Text)
		}
		if res.OutputTokens != 5 || res.InputTokens != 10 || res.CacheReadTokens != 100 || res.Turns != 1 {
			t.Fatalf("usage %+v", res)
		}
		if res.CostUSD < 0.0099 || res.CostUSD > 0.0101 {
			t.Fatalf("turn %d cost %v (the difference of the totals)", i, res.CostUSD)
		}
		if id != "" && res.Session != id {
			t.Fatalf("another session %s, was %s", res.Session, id)
		}
		id = res.Session
		if !strings.HasPrefix(res.ClaudeSession, "fake-") {
			t.Fatalf("claude session %q", res.ClaudeSession)
		}
	}
	st := starts(t, log)
	if len(st) != 1 {
		t.Fatalf("one process for three turns, got %v", st)
	}
	if !strings.Contains(st[0], "model=sonnet") || !strings.Contains(st[0], "mcp__vboard") || !strings.Contains(st[0], "Read(./**),") {
		t.Fatalf("start %q: the default model, the allow-list and the session's MCP server", st[0])
	}
	list := m.List()
	if len(list) != 1 || list[0].ID != id || list[0].Owner != "alice" || list[0].Driver != "json" || list[0].State != StateIdle ||
		list[0].Turns != 3 || !ValidID(id) {
		t.Fatalf("list %+v", list)
	}
	// the session works in its own folder; its MCP config is kept beside it
	if list[0].Cwd != filepath.Join(m.Root, id) {
		t.Fatalf("cwd %s", list[0].Cwd)
	}
	b, err := os.ReadFile(filepath.Join(m.Root, ".ctl", id, "mcp.json"))
	if err != nil || !strings.Contains(string(b), `"vboard"`) || strings.Contains(string(b), "Bearer t") {
		t.Fatalf("mcp.json: %s %v", b, err)
	}
	// another owner with the same key gets a session of their own
	res, err := m.Turn(context.Background(), Options{Key: "board-1", Owner: "bob"}, "hi", nil)
	if err != nil || res.Session == id {
		t.Fatalf("bob: %+v %v", res, err)
	}
}

func TestToolEvents(t *testing.T) {
	m, _ := fakeManager(t)
	r := &recorder{}
	res, err := m.Turn(context.Background(), Options{Key: "k"}, "use a tool", r.emit)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.text(); got != "Let me look.\n\nFound it." || res.Text != got {
		t.Fatalf("text %q / %q", got, res.Text)
	}
	tools := r.tools()
	if len(tools) != 2 || tools[0].ID != "toolu_1" || tools[0].State != "running" || tools[0].Name != "Read" ||
		tools[0].Summary != "/x/notes.txt" || string(tools[0].Arguments) != `{"file_path":"/x/notes.txt"}` ||
		tools[1].State != "done" || tools[1].Name != "Read" || tools[1].Result != "the notes" {
		t.Fatalf("tools %+v", tools)
	}
}

// Cancel interrupts the turn (a control request); the session and its process stay.
func TestCancelKeepsSession(t *testing.T) {
	m, log := fakeManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	r := &recorder{}
	done := make(chan error, 1)
	go func() {
		_, err := m.Turn(ctx, Options{Key: "k"}, "be slow", r.emit)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(r.text(), "tick tick") {
		if time.Now().After(deadline) {
			t.Fatal("no ticks")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the interrupted turn did not end")
	}
	res, err := m.Turn(context.Background(), Options{Key: "k"}, "after", nil)
	if err != nil || res.Text != "echo: after" {
		t.Fatalf("after cancel: %+v %v", res, err)
	}
	if st := starts(t, log); len(st) != 1 {
		t.Fatalf("starts %v", st)
	}
}

// An idle session is closed; its conversation continues in a new process with --resume.
func TestIdleCloseAndResume(t *testing.T) {
	m, log := fakeManager(t)
	res, err := m.Turn(context.Background(), Options{Key: "k"}, "one", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := m.Reap(time.Now().Add(time.Minute)); n != 0 {
		t.Fatalf("reaped %d too early", n)
	}
	if n := m.Reap(time.Now().Add(31 * time.Minute)); n != 1 || len(m.List()) != 0 {
		t.Fatalf("reaped %d, list %v", n, m.List())
	}
	if _, err := m.Turn(context.Background(), Options{Key: "k"}, "two", nil); err != nil {
		t.Fatal(err)
	}
	st := starts(t, log)
	if len(st) != 2 || !strings.Contains(st[1], "resume="+res.ClaudeSession) {
		t.Fatalf("starts %v (want a resume of %s)", st, res.ClaudeSession)
	}
	// a fresh manager (the worker restarted) resumes it too (resume.json)
	m2 := &Manager{Root: m.Root, Command: m.Command, Env: m.Env, Version: m.Version}
	defer m2.Close()
	if _, err := m2.Turn(context.Background(), Options{Key: "k"}, "three", nil); err != nil {
		t.Fatal(err)
	}
	if st := starts(t, log); len(st) != 3 || !strings.Contains(st[2], "resume="+res.ClaudeSession) {
		t.Fatalf("starts %v", st)
	}
}

// Opening is sent only while the conversation is new here (no turn yet, nothing to resume); Fresh starts it anew.
func TestOpeningAndFresh(t *testing.T) {
	m, log := fakeManager(t)
	o := Options{Key: "k", Opening: "history + one"}
	if res, err := m.Turn(context.Background(), o, "one", nil); err != nil || res.Text != "echo: history + one" {
		t.Fatalf("first turn %+v %v", res, err)
	}
	o.Opening = "history + two"
	if res, _ := m.Turn(context.Background(), o, "two", nil); res.Text != "echo: two" {
		t.Fatalf("second turn %q", res.Text)
	}
	// a resumed conversation (closed meanwhile) has its history: no opening either
	m.Reap(time.Now().Add(time.Hour))
	if res, _ := m.Turn(context.Background(), o, "three", nil); res.Text != "echo: three" {
		t.Fatalf("resumed turn %q", res.Text)
	}
	// fresh: the running session is closed, nothing resumed, the opening is sent
	o.Fresh, o.Opening = true, ""
	if res, _ := m.Turn(context.Background(), o, "anew", nil); res.Text != "echo: anew" {
		t.Fatalf("fresh turn %q", res.Text)
	}
	st := starts(t, log)
	if len(st) != 3 || !strings.Contains(st[1], "resume=fake-") || !strings.HasSuffix(st[2], "resume= tools=Read(./**),Glob(./**),Grep(./**),WebSearch,WebFetch,TodoWrite") {
		t.Fatalf("starts %v", st)
	}
}

func TestOneOffAndStop(t *testing.T) {
	m, log := fakeManager(t)
	res, err := m.Turn(context.Background(), Options{}, "once", nil)
	if err != nil || res.Text != "echo: once" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(m.List()) != 0 {
		t.Fatalf("a one-off session stays: %v", m.List())
	}
	if _, err := os.Stat(filepath.Join(m.Root, res.Session)); !os.IsNotExist(err) {
		t.Fatalf("its folder stays: %v", err)
	}
	res, _ = m.Turn(context.Background(), Options{Key: "k"}, "a", nil)
	if err := m.Stop(res.Session); err != nil || len(m.List()) != 0 {
		t.Fatalf("stop: %v %v", err, m.List())
	}
	if err := m.Stop(res.Session); !errors.Is(err, ErrNoSession) {
		t.Fatalf("stop again: %v", err)
	}
	if len(starts(t, log)) != 2 {
		t.Fatal(starts(t, log))
	}
}

func TestMaxSessions(t *testing.T) {
	m, _ := fakeManager(t)
	m.Config.MaxSessions = 2
	var ids []string
	for _, k := range []string{"a", "b", "c"} {
		res, err := m.Turn(context.Background(), Options{Key: k}, "hi", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.Session)
		time.Sleep(5 * time.Millisecond)
	}
	list := m.List()
	if len(list) != 2 {
		t.Fatalf("list %v", list)
	}
	for _, in := range list {
		if in.ID == ids[0] {
			t.Fatalf("the oldest stays: %v", list)
		}
	}
}

// A process that dies fails the turn; the next turn starts a new one.
func TestProcessDies(t *testing.T) {
	m, log := fakeManager(t)
	if _, err := m.Turn(context.Background(), Options{Key: "k"}, "hello", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Turn(context.Background(), Options{Key: "k"}, "die now", nil); err == nil || !strings.Contains(err.Error(), "claude stopped") {
		t.Fatalf("err %v", err)
	}
	res, err := m.Turn(context.Background(), Options{Key: "k"}, "again", nil)
	if err != nil || res.Text != "echo: again" {
		t.Fatalf("%+v %v", res, err)
	}
	if st := starts(t, log); len(st) != 2 || !strings.Contains(st[1], "resume=fake-") {
		t.Fatalf("starts %v", st)
	}
}

func TestUnavailable(t *testing.T) {
	m := &Manager{Root: t.TempDir(), Command: filepath.Join(t.TempDir(), "no-claude")}
	if _, ok := m.Available(); ok {
		t.Fatal("available without claude")
	}
	if _, err := m.Turn(context.Background(), Options{Key: "k"}, "hi", nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(dir)
	if err != nil || c.Model != "sonnet" || c.IdleMinutes != 30 || c.MaxSessions != 4 || len(c.AllowedTools) == 0 || !*c.AutoTrust {
		t.Fatalf("defaults %+v %v", c, err)
	}
	os.WriteFile(filepath.Join(dir, "agents.json"), []byte(`{"allowedTools":["Read","Edit"],"model":"opus","idleMinutes":5,"maxSessions":1}`), 0o644)
	c, err = LoadConfig(dir)
	if err != nil || c.Model != "opus" || c.IdleMinutes != 5 || c.MaxSessions != 1 || strings.Join(c.AllowedTools, ",") != "Read,Edit" {
		t.Fatalf("file %+v %v", c, err)
	}
}

// The tty driver: an interactive process in a terminal; a turn is typed into it and answered through the hooks;
// a viewer sees the terminal.
func TestTTY(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no tty on Windows")
	}
	m, _ := fakeManager(t)
	r := &recorder{}
	o := Options{Key: "k", Driver: DriverTTY}
	res, err := m.Turn(context.Background(), o, "use a tool please", r.emit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "echo: use a tool please" || r.text() != res.Text || res.Driver != DriverTTY {
		t.Fatalf("result %+v, text %q", res, r.text())
	}
	if tools := r.tools(); len(tools) != 2 || tools[0].ID != "t1" || tools[0].State != "running" || tools[1].State != "done" {
		t.Fatalf("tools %+v", tools)
	}
	term, err := m.Terminal(res.Session)
	if err != nil {
		t.Fatal(err)
	}
	snap, out, detach := term.Attach()
	defer detach()
	if !strings.Contains(string(snap), "fake claude ready") || !strings.Contains(string(snap), "answer: use a tool please") {
		t.Fatalf("scrollback %q", snap)
	}
	if in, _ := m.Get(res.Session); in.Viewers != 1 {
		t.Fatalf("viewers %d", in.Viewers)
	}
	if err := term.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	// a person types into it
	term.Write([]byte("typed by hand\r"))
	var seen strings.Builder
	deadline := time.After(5 * time.Second)
	for !strings.Contains(seen.String(), "answer: typed by hand") {
		select {
		case b := <-out:
			seen.Write(b)
		case <-deadline:
			t.Fatalf("output %q", seen.String())
		}
	}
	// the next API turn is not confused by the person's turn
	res2, err := m.Turn(context.Background(), o, "second", nil)
	if err != nil || res2.Text != "echo: second" || res2.Session != res.Session {
		t.Fatalf("%+v %v", res2, err)
	}
	// attached: never idle-closed
	if n := m.Reap(time.Now().Add(time.Hour)); n != 0 {
		t.Fatalf("reaped while attached: %+v", m.List())
	}
	detach()
	if n := m.Reap(time.Now().Add(time.Hour)); n != 1 {
		t.Fatal("not reaped after detach")
	}
}

func TestOpenTTY(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no tty on Windows")
	}
	m, _ := fakeManager(t)
	in, err := m.OpenTTY("alice", "opus", "")
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(in.ID) || in.Driver != DriverTTY || in.Model != "opus" || in.Owner != "alice" {
		t.Fatalf("%+v", in)
	}
	if _, err := m.OpenTTY("alice", "", "relative/path"); err == nil {
		t.Fatal("a relative cwd")
	}
	if _, err := m.Terminal(in.ID); err != nil {
		t.Fatal(err)
	}
}

// A conversation Claude Code does not have (resume.json from before the worker's own config folder) is not an error:
// Claude Code answers the --resume with an error result, and the turn runs again in a new, fresh process.
func TestResumeGoneStartsAnew(t *testing.T) {
	m, log := fakeManager(t)
	m.Env = append(m.Env, "AGENTS_FAKE_RESUME_GONE=gone-1")
	if err := os.MkdirAll(m.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(filepath.Join(m.Root, "resume.json"), map[string]string{sessionID("", "k"): "gone-1"}); err != nil {
		t.Fatal(err)
	}
	res, err := m.Turn(context.Background(), Options{Key: "k", Opening: "history + one"}, "one", nil)
	if err != nil {
		t.Fatalf("turn: %v (starts %v)", err, starts(t, log))
	}
	// started anew: it got the opening (the history), as a conversation that is new here
	if res.Text != "echo: history + one" {
		t.Fatalf("text %q", res.Text)
	}
	st := starts(t, log)
	if len(st) != 2 || !strings.Contains(st[0], "resume=gone-1") || strings.Contains(st[1], "resume=gone-1") {
		t.Fatalf("starts %v (want a failed resume, then a fresh start)", st)
	}
}
