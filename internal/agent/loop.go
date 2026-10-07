package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/llm"
)

// HeldNote is what the model reads instead of a result the guard held back.
const HeldNote = "[result held back: it contained instructions aimed at the assistant]"

// What the model reads about a call that did not run (said plainly: models like to report success anyway).
const (
	notMade     = "NOT DONE — the call was not made: "
	nothingDone = " Nothing was changed. Tell the user it was not done."
)

// Generator is the chat model: it continues a rendered prompt and streams the text.
type Generator interface {
	// Format is the model's tool-call format: "json" (Qwen3) or "xml" (Qwen3.5/3.6), see llm.Render.
	Format() string
	Generate(ctx context.Context, prompt string, onText func(string)) error
}

// Event is what the loop reports (the engine turns it into an engine.Event of the same fields: this package does
// not import the engine, the engine imports it).
type Event struct {
	Type string // text | status | log | tool | file | stats | result
	Text string
	File string
	Data map[string]any
}

// Emit passes an event on.
type Emit func(Event)

// Config is what the loop works with. The hooks open the toolboxes of a zone for one run (they are closed after it);
// a nil hook: that zone is not available on this Mac yet.
type Config struct {
	Home  func(ctx context.Context) ([]Toolbox, error) // the MCP servers (~/.config/mlxlab/mcp.json)
	Web   func(ctx context.Context) (Toolbox, error)   // web_search, web_fetch (sandbox VM)
	Files func(ctx context.Context) (Toolbox, error)   // make_file (offline sandbox VM)
	Guard Guard                                        // nil: results are not screened, answers not graded
	// Always says the user allowed a home tool always (<config dir>/approvals.json): a call of it that changes
	// something runs without asking. Asked at each call (the list may change while a chat runs); nil: always ask.
	// Only home tools: the web and files tools never ask anyway.
	Always func(tool string) bool

	Rounds          int           // rounds of tool calls (0: 8)
	CallTimeout     time.Duration // one call (0: 3 minutes)
	ApprovalTimeout time.Duration // waiting for the user (0: 5 minutes)
	MaxResult       int           // characters of a result the model reads (0: 12000)
}

func (c Config) defaults() Config {
	if c.Rounds <= 0 {
		c.Rounds = 8
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 3 * time.Minute
	}
	if c.ApprovalTimeout <= 0 {
		c.ApprovalTimeout = 5 * time.Minute
	}
	if c.MaxResult <= 0 {
		c.MaxResult = 12000
	}
	return c
}

// Request is one turn of a chat with tools.
type Request struct {
	Zone     Zone
	Messages []llm.Message // the history (tool turns included, as a previous result gave them)
	Think    bool
	OutDir   string // the worker's outputs (files of tools become job files there)
	Gate     *Gate  // where approvals arrive (nil: the context's, see WithGate)
}

// offers says which tools of which zone a chat of zone z is offered.
func offers(z, tool Zone) bool {
	return tool == z || (z == ZoneWeb && tool == ZoneFiles)
}

// toolset is what one run may call.
type toolset struct {
	tools  []Tool
	box    map[string]Toolbox
	boxes  []Toolbox
	byName map[string]Tool
}

func (s *toolset) close() {
	for _, b := range s.boxes {
		_ = b.Close()
	}
}

func (s *toolset) llmTools() []llm.Tool {
	out := make([]llm.Tool, len(s.tools))
	for i, t := range s.tools {
		out[i] = llm.Tool{Name: t.Name, Description: t.Description, Parameters: t.Parameters}
	}
	return out
}

// open opens the toolboxes of the zone: only those, so a web chat never even connects to the home servers.
func open(ctx context.Context, cfg Config, z Zone, emit Emit) (*toolset, error) {
	s := &toolset{box: map[string]Toolbox{}, byName: map[string]Tool{}}
	add := func(origin Zone, b Toolbox) {
		s.boxes = append(s.boxes, b)
		tools, err := b.Tools(ctx)
		if err != nil {
			emit(Event{Type: "log", Text: fmt.Sprintf("%s tools: %v", origin, err)})
			return
		}
		for _, t := range tools {
			t.Zone = origin // a tool belongs to the zone of the box it came from, whatever it says
			if !offers(z, t.Zone) {
				continue
			}
			if _, dup := s.byName[t.Name]; dup {
				emit(Event{Type: "log", Text: fmt.Sprintf("tool %s is offered twice: the first one is kept", t.Name)})
				continue
			}
			s.tools = append(s.tools, t)
			s.byName[t.Name] = t
			s.box[t.Name] = b
		}
	}
	single := func(origin Zone, f func(context.Context) (Toolbox, error), required bool) error {
		if f == nil {
			if required {
				return fmt.Errorf("a %s chat is not available on this Mac yet", origin)
			}
			return nil
		}
		b, err := f(ctx)
		if err != nil {
			if required {
				return fmt.Errorf("%s tools: %w", origin, err)
			}
			emit(Event{Type: "log", Text: fmt.Sprintf("%s tools: %v", origin, err)})
			return nil
		}
		if b != nil {
			add(origin, b)
		}
		return nil
	}
	var err error
	switch z {
	case ZoneHome:
		if cfg.Home == nil {
			err = errors.New("a home chat is not available on this Mac yet")
			break
		}
		var boxes []Toolbox
		if boxes, err = cfg.Home(ctx); err != nil {
			err = fmt.Errorf("home tools: %w", err)
			break
		}
		for _, b := range boxes {
			add(ZoneHome, b)
		}
	case ZoneWeb:
		if err = single(ZoneWeb, cfg.Web, true); err == nil {
			err = single(ZoneFiles, cfg.Files, false)
		}
	case ZoneFiles:
		err = single(ZoneFiles, cfg.Files, true)
	default:
		err = fmt.Errorf("unknown zone %q", z)
	}
	if err == nil && len(s.tools) == 0 {
		err = fmt.Errorf("a %s chat: no tools are available", z)
	}
	if err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// Run answers the last message of the conversation with the zone's tools: it generates, runs the calls the model
// makes (approval, guard, limits), gives the model the results and generates again, up to cfg.Rounds rounds. It
// returns the whole history (the request's and the new turns), which it also reports as the job's result
// ("messages"), so the next turn carries it.
func Run(ctx context.Context, gen Generator, cfg Config, req Request, emit Emit) ([]llm.Message, error) {
	cfg = cfg.defaults()
	msgs := append([]llm.Message(nil), req.Messages...)
	set, err := open(ctx, cfg, req.Zone, emit)
	if err != nil {
		return msgs, err
	}
	defer set.close()
	gate := req.Gate
	if gate == nil {
		gate = GateFrom(ctx)
	}
	r := &run{ctx: ctx, cfg: cfg, req: req, emit: emit, set: set, gate: gate, question: lastUser(msgs)}
	// what earlier turns' tools returned: evidence for grading a follow-up that answers from it
	r.sources = earlierResults(msgs, 6)
	defer func() { emit(Event{Type: "result", Data: map[string]any{"messages": msgs}}) }()

	var answer string
	for round := 1; ; round++ {
		emit(Event{Type: "status", Text: "answering …"})
		prompt := llm.Render(gen.Format(), withToolRules(msgs), set.llmTools(), req.Think)
		if req.Think {
			emit(Event{Type: "text", Text: "<think>"}) // the prompt opened the thought
		}
		f := &textFilter{think: req.Think, emit: emit}
		var out strings.Builder
		gerr := gen.Generate(ctx, prompt, func(s string) { out.WriteString(s); f.write(s) })
		f.flush()
		content, bodies := splitCalls(out.String(), req.Think)
		if req.Think {
			content = "<think>\n" + content
		}
		if gerr != nil {
			msgs = append(msgs, llm.Message{Role: "assistant", Content: content})
			return msgs, gerr
		}
		if len(bodies) == 0 {
			msgs = append(msgs, llm.Message{Role: "assistant", Content: content})
			answer = content
			break
		}
		calls := make([]llm.ToolCall, len(bodies))
		for i, b := range bodies {
			name, args, ok := llm.ParseToolCall(b, set.llmTools())
			if !ok {
				name, args = "", "{}"
			}
			calls[i] = llm.ToolCall{ID: newID(), Name: name, Arguments: args}
		}
		if round > cfg.Rounds {
			// enough: what it wrote before the calls is the answer
			for _, c := range calls {
				r.event(ToolEvent{ID: c.ID, Name: c.Name, Arguments: rawArgs(c.Arguments), State: "blocked",
					Result: "too many rounds of tool calls"})
			}
			emit(Event{Type: "log", Text: fmt.Sprintf("stopped after %d rounds of tool calls", cfg.Rounds)})
			msgs = append(msgs, llm.Message{Role: "assistant", Content: content})
			answer = content
			if strings.TrimSpace(stripThought(content)) == "" {
				const note = "(I stopped: too many tool calls without an answer.)"
				emit(Event{Type: "text", Text: note})
				msgs[len(msgs)-1].Content += note
				answer += note
			}
			break
		}
		msgs = append(msgs, llm.Message{Role: "assistant", Content: content, ToolCalls: calls})
		for i, c := range calls {
			text := r.call(c)
			if round == cfg.Rounds && i == len(calls)-1 {
				text += "\n\n[that was the last round of tool calls: answer now with what you have]"
			}
			msgs = append(msgs, llm.Message{Role: "tool", Content: text, ToolCallID: c.ID})
			if err := ctx.Err(); err != nil {
				return msgs, err
			}
		}
	}

	stats := map[string]any{"zone": string(req.Zone), "toolCalls": r.calls}
	if cfg.Guard != nil {
		v, err := cfg.Guard.Grade(ctx, r.question, stripThought(answer), r.sources)
		if err != nil {
			emit(Event{Type: "log", Text: "guard (grade): " + err.Error()})
		} else {
			stats["verdict"] = v
		}
	}
	emit(Event{Type: "stats", Data: stats})
	return msgs, nil
}

// run is the state of one Run.
type run struct {
	ctx      context.Context
	cfg      Config
	req      Request
	emit     Emit
	set      *toolset
	gate     *Gate
	question string
	sources  []string // what the tools gave (for the grade)
	calls    int
}

func (r *run) event(ev ToolEvent) {
	r.emit(Event{Type: "tool", Data: toMap(ev)})
}

// call runs one call and says what the model reads about it.
func (r *run) call(c llm.ToolCall) string {
	r.calls++
	ev := ToolEvent{ID: c.ID, Name: c.Name, Arguments: rawArgs(c.Arguments)}
	t, ok := r.set.byName[c.Name]
	if !ok {
		// a tool of another zone, or none at all: refused here, whatever the model believes
		ev.State, ev.Zone = "blocked", r.req.Zone
		ev.Result = "not available in a " + string(r.req.Zone) + " chat"
		r.event(ev)
		if c.Name == "" {
			return "error: the tool call could not be read"
		}
		return fmt.Sprintf("error: there is no tool %q in this chat", c.Name)
	}
	ev.Zone = t.Zone
	if t.Changes && t.Zone == ZoneHome && r.cfg.Always != nil && r.cfg.Always(t.Name) {
		ev.Auto = true // allowed always: no card, straight to running
	} else if t.Changes {
		if r.gate == nil {
			ev.State, ev.Result = "denied", "nobody can approve it here"
			r.event(ev)
			return notMade + "it needs the user's approval, and nobody can approve it here." + nothingDone
		}
		allow, timedOut, err := r.gate.Wait(r.ctx, c.ID, r.cfg.ApprovalTimeout, func() {
			ev.State = "approval"
			ev.Deadline = time.Now().Add(r.cfg.ApprovalTimeout) // a reloaded page can still count down
			ev.Description = t.Description                      // what the tool does, for the approval card
			r.event(ev)
		})
		ev.Deadline, ev.Description = time.Time{}, ""
		if err != nil {
			ev.State, ev.Result = "denied", "canceled"
			r.event(ev)
			return "the call was not made: the chat was canceled"
		}
		if !allow {
			ev.State = "denied"
			if timedOut {
				ev.Result = "no answer in time"
				r.event(ev)
				return notMade + "the user did not approve it in time." + nothingDone
			}
			ev.Result = "the user said no"
			r.event(ev)
			return notMade + "the user did not allow it." + nothingDone
		}
	}
	ev.State = "running"
	r.event(ev)
	cctx, cancel := context.WithTimeout(r.ctx, r.cfg.CallTimeout)
	res, err := t.callIn(cctx, r.set.box[c.Name], json.RawMessage(c.Arguments))
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && r.ctx.Err() == nil {
			err = fmt.Errorf("no result within %s", r.cfg.CallTimeout)
		}
		ev.State, ev.Result = "error", err.Error()
		r.event(ev)
		return "error: " + err.Error()
	}
	text := res.Text
	ev.Sources = res.Sources
	for _, f := range res.Files {
		p, err := r.keep(f)
		if err != nil {
			r.emit(Event{Type: "log", Text: fmt.Sprintf("%s: file %s: %v", c.Name, f, err)})
			continue
		}
		r.emit(Event{Type: "file", File: p})
		ev.Files = append(ev.Files, r.url(p))
		text += "\n[file made, shown to the user: " + filepath.Base(p) + "]"
	}
	if t.Untrusted {
		switch {
		case r.cfg.Guard == nil:
			r.emit(Event{Type: "log", Text: c.Name + ": no guard on this Mac, the result is not screened"})
		default:
			v, err := r.cfg.Guard.Screen(r.ctx, r.question, res.Text)
			if err != nil {
				// it could not be screened: it is not read either
				v = Verdict{Label: "injection", Why: "could not be screened: " + err.Error()}
			}
			if v.Label == "injection" {
				ev.State, ev.Verdict, ev.Result = "held", &v, short(res.Text, 300)
				r.event(ev)
				return HeldNote
			}
			ev.Verdict = &v
		}
	}
	ev.State, ev.Result = "done", short(text, 600)
	if res.IsError {
		ev.State = "error"
	}
	r.event(ev)
	if !res.IsError {
		// the grader sees what was called, not just the output (a command's result means little on its own)
		r.sources = append(r.sources, short(c.Name+" "+compactArgs(c.Arguments)+" → "+text, 4000))
	}
	return capText(text, r.cfg.MaxResult)
}

func (t Tool) callIn(ctx context.Context, b Toolbox, args json.RawMessage) (Result, error) {
	if b == nil {
		return Result{}, fmt.Errorf("tool %s has no toolbox", t.Name)
	}
	return b.Call(ctx, t.Name, args)
}

// keep makes sure a tool's file is in the outputs (a file elsewhere is copied in).
func (r *run) keep(p string) (string, error) {
	if r.req.OutDir == "" || inside(r.req.OutDir, p) {
		return p, nil
	}
	dir := filepath.Join(r.req.OutDir, "tools")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, time.Now().Format("20060102-150405")+"-"+newID()[5:]+"-"+filepath.Base(p))
	in, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return "", err
	}
	return dst, out.Close()
}

// url is a file's /files/ URL.
func (r *run) url(p string) string {
	if r.req.OutDir == "" {
		return p
	}
	rel, err := filepath.Rel(r.req.OutDir, p)
	if err != nil {
		return p
	}
	return "/files/" + filepath.ToSlash(rel)
}

func inside(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && filepath.IsAbs(p)
}

// splitCalls separates what the model wrote from its tool calls: the text before the first <tool_call> and the body of
// every <tool_call> block (the last may be cut off). With think, calls count only after the thought.
func splitCalls(text string, think bool) (content string, bodies []string) {
	from := 0
	if think {
		i := strings.Index(text, "</think>")
		if i < 0 {
			return text, nil // still thinking: no calls
		}
		from = i + len("</think>")
	}
	i := strings.Index(text[from:], "<tool_call>")
	if i < 0 {
		return text, nil
	}
	content = strings.TrimRight(text[:from+i], " \n")
	rest := text[from+i:]
	for {
		j := strings.Index(rest, "<tool_call>")
		if j < 0 {
			break
		}
		rest = rest[j+len("<tool_call>"):]
		k := strings.Index(rest, "</tool_call>")
		if k < 0 {
			bodies = append(bodies, rest)
			break
		}
		bodies = append(bodies, rest[:k])
		rest = rest[k+len("</tool_call>"):]
	}
	return content, bodies
}

// textFilter streams the model's text but not its tool calls (they become tool events).
type textFilter struct {
	think   bool
	emit    Emit
	all     string
	sent    int
	stopped bool
}

const callTag = "<tool_call>"

func (f *textFilter) write(s string) {
	if f.stopped {
		return
	}
	f.all += s
	start := 0
	safe := len(f.all)
	if f.think {
		i := strings.Index(f.all, "</think>")
		if i < 0 {
			f.send(safe)
			return
		}
		start = i + len("</think>")
	}
	if j := strings.Index(f.all[start:], callTag); j >= 0 {
		safe, f.stopped = start+j, true
	} else {
		safe = len(f.all) - partial(f.all[start:], callTag)
	}
	f.send(safe)
}

func (f *textFilter) send(to int) {
	if to > f.sent {
		f.emit(Event{Type: "text", Text: f.all[f.sent:to]})
		f.sent = to
	}
}

func (f *textFilter) flush() {
	if !f.stopped {
		f.send(len(f.all))
	}
}

// partial is the length of the longest end of s that begins tag.
func partial(s, tag string) int {
	for n := min(len(tag)-1, len(s)); n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}

func stripThought(s string) string {
	if i := strings.Index(s, "</think>"); i >= 0 {
		return strings.TrimSpace(s[i+len("</think>"):])
	}
	if strings.HasPrefix(strings.TrimSpace(s), "<think>") {
		return ""
	}
	return strings.TrimSpace(s)
}

func lastUser(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

func rawArgs(a string) json.RawMessage {
	if json.Valid([]byte(a)) {
		return json.RawMessage(a)
	}
	b, _ := json.Marshal(a)
	return b
}

func capText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("\n[… %d more characters cut]", len(r)-n)
}

func short(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

// compactArgs: a call's arguments (JSON) on one line, for the grader.
func compactArgs(a string) string {
	var b bytes.Buffer
	if json.Compact(&b, []byte(a)) != nil || b.Len() == 0 {
		return "{}"
	}
	return b.String()
}

// toolRules is told to the model in every chat with tools (at render time only: it is not part of the history).
// A follow-up like "but there is a direct train" made the model drop the timetable's answer and invent times.
const toolRules = "You have tools. For facts a tool can check — timetables, device and house states, files, web pages — " +
	"call the tool in this turn instead of answering from memory or from earlier answers, especially when the user " +
	"doubts a result: check again and report what the data says, also when it contradicts the user. Never invent " +
	"times, numbers or states; if a tool cannot tell, say so."

// withToolRules adds toolRules to the system prompt (a system message of its own if there is none).
func withToolRules(msgs []llm.Message) []llm.Message {
	out := append([]llm.Message(nil), msgs...)
	for i, m := range out {
		if m.Role == "system" {
			m.Content = strings.TrimRight(m.Content, "\n") + "\n\n" + toolRules
			out[i] = m
			return out
		}
	}
	return append([]llm.Message{{Role: "system", Content: toolRules}}, out...)
}

// earlierResults: the last n tool results before this turn's question, labelled with their tool, for the grader.
func earlierResults(msgs []llm.Message, n int) []string {
	last := len(msgs) - 1
	for last >= 0 && msgs[last].Role != "user" {
		last--
	}
	names := map[string]string{}
	var out []string
	for _, m := range msgs[:max(last, 0)] {
		for _, c := range m.ToolCalls {
			names[c.ID] = c.Name + " " + compactArgs(c.Arguments)
		}
		if m.Role == "tool" {
			call := names[m.ToolCallID]
			if call == "" {
				call = "a tool"
			}
			out = append(out, short("(earlier turn) "+call+" → "+m.Content, 1500))
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}
