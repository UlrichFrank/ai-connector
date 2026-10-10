package hub

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/agents"
	"github.com/philipparndt/ai-connector/internal/agents/fakeclaude"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/web"
)

// TestMain doubles as a fake Claude Code (internal/agents/fakeclaude): the agent tests run this test binary as claude.
func TestMain(m *testing.M) {
	if fakeclaude.Active() {
		os.Exit(fakeclaude.Main(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// claudeMac is a worker with the real Claude Code backend and agents manager, the fake claude standing in.
type claudeMac struct {
	mgr       *jobs.Manager
	am        *agents.Manager
	log, args string // the fake's start log and its arguments (JSON per start)
	stop      func()
}

func startFakeClaudeMac(t *testing.T, base, token string) *claudeMac {
	t.Helper()
	dir := t.TempDir()
	m := &claudeMac{log: filepath.Join(dir, "claude.log"), args: filepath.Join(dir, "claude.args")}
	m.am = &agents.Manager{Root: filepath.Join(dir, "agents"), Command: os.Args[0], Version: "9.9.9 (fake)",
		Env: []string{fakeclaude.Env, "AGENTS_FAKE_LOG=" + m.log, "AGENTS_FAKE_ARGS=" + m.args}}
	cat := &engine.Catalog{}
	cc := &engine.ClaudeCodeBackend{Agents: m.am, Catalog: cat}
	cc.Discover()
	m.mgr = jobs.New(cat, []engine.Backend{cc}, dir)
	api := (&web.Server{Jobs: m.mgr, Prompts: prompts.Open(dir), Version: "test", Agents: m.am}).Handler()
	stop, _ := connectWorkerWith(t, base, token, withRAM(api, 64))
	m.stop = func() {
		stop()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !m.mgr.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	}
	t.Cleanup(func() {
		m.stop()
		m.am.Close()
	})
	return m
}

// starts are the fake's starts: its arguments, MCP config (as written and as expanded) and AIC_MCP_ environment.
func (m *claudeMac) starts(t *testing.T) []struct {
	Args                []string
	McpRaw, McpExpanded string
} {
	t.Helper()
	var out []struct {
		Args                []string
		McpRaw, McpExpanded string
	}
	f, err := os.Open(m.args)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var s struct {
			Args                []string
			McpRaw, McpExpanded string
		}
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func argOf(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (m *claudeMac) jobsDone() []*jobs.Job {
	var out []*jobs.Job
	for _, j := range m.mgr.Jobs() {
		if j.State == jobs.Done {
			out = append(out, j)
		}
	}
	return out
}

func nextUpdate(t *testing.T, b *fakeBridge) []vbAgent {
	t.Helper()
	select {
	case u := <-b.updates:
		return u
	case <-time.After(10 * time.Second):
		t.Fatal("no agents update")
	}
	return nil
}

// The agents a connector offers: only those a connected worker runs now — Claude Code only on the owner's worker —
// told in hello and, when workers come and go, in {type:"agents"} on the same connection; hello.chat stays the first
// model for older vboards; a handover's new connection has them in its hello again.
func TestVboardAgentsAdvertised(t *testing.T) {
	fastBridges(t)
	s, srv := newHub(t, t.TempDir())
	t.Cleanup(s.Close)
	base := srv.URL
	alice := signInAs(t, base, "Alice's laptop", "Alice")
	bob := signInAs(t, base, "Bob's phone", "Bob")
	_, qwenTok := enroll(t, base, alice, "Studio")
	aliceID, aliceTok := enroll(t, base, alice, "Alice's Mac")
	_, bobTok := enroll(t, base, bob, "Bob's Mac")
	startVboardMac(t, base, qwenTok, "chat")
	startFakeClaudeMac(t, base, bobTok) // someone else's Claude Code is never offered

	v := newFakeVboard(t, stageAgents)
	c := addConnector(t, base, alice, `{"name":"Alice AI","url":"`+v.srv.URL+`","token":"vbp_secret",
		"agents":[{"model":"qwen3.6-35b-a3b"},{"model":"claude-code/haiku","name":"Claude"},{"id":"op","model":"claude-code/opus"}]}`)
	wantConf := []ConnectorAgent{{ID: "qwen3.6-35b-a3b", Model: "qwen3.6-35b-a3b", Kind: "model"},
		{ID: "claude-code-haiku", Model: "claude-code/haiku", Name: "Claude", Kind: "agent"}, {ID: "op", Model: "claude-code/opus", Kind: "agent"}}
	if !reflect.DeepEqual(c.Agents, wantConf) {
		t.Fatalf("agents %+v", c.Agents)
	}
	b := v.next(t)
	qwen := vbAgent{ID: "qwen3.6-35b-a3b", Name: "Qwen3.6", Kind: "model", Worker: "Studio"}
	if b.hello.Chat != "qwen3.6-35b-a3b" || !reflect.DeepEqual(b.agents, []vbAgent{qwen}) {
		t.Fatalf("alice's worker offline: hello %+v agents %+v", b.hello, b.agents)
	}

	// alice's worker with Claude Code connects: the list grows on the same connection (no new hello)
	mac := startFakeClaudeMac(t, base, aliceTok)
	claude := vbAgent{ID: "claude-code-haiku", Name: "Claude", Kind: "agent", Worker: "Alice's Mac"}
	opus := vbAgent{ID: "op", Name: "Claude Code · Opus", Kind: "agent", Worker: "Alice's Mac"}
	if got := nextUpdate(t, b); !reflect.DeepEqual(got, []vbAgent{qwen, claude, opus}) {
		t.Fatalf("update %+v", got)
	}
	got := waitState(t, base, alice, c.ID, "connected")
	waitFor(t, "advertised in the console", func() bool {
		got = connectorOf(t, base, alice, c.ID)
		return len(got.Advertised) == 3
	})
	if got.Advertised[1] != (AdvertisedAgent{ID: "claude-code-haiku", Name: "Claude", Kind: "agent", Worker: "Alice's Mac", Model: "claude-code/haiku"}) {
		t.Fatalf("advertised %+v", got.Advertised)
	}
	if _, dials := v.count(); dials != 1 {
		t.Fatalf("%d connections", dials)
	}

	// a handover: the new hello has the agents
	b.write(map[string]string{"type": "reconnect"})
	b2 := v.next(t)
	if !reflect.DeepEqual(b2.agents, []vbAgent{qwen, claude, opus}) || b2.hello.Chat != "qwen3.6-35b-a3b" {
		t.Fatalf("after the handover %+v %+v", b2.hello, b2.agents)
	}

	// alice's worker goes: Claude Code is gone from the list
	mac.stop()
	if got := nextUpdate(t, b2); !reflect.DeepEqual(got, []vbAgent{qwen}) {
		t.Fatalf("update after the worker left %+v", got)
	}
	waitFor(t, "workers known", func() bool { return !workerItems(t, base, alice)[aliceID].Connected })
	if _, dials := v.count(); dials != 2 {
		t.Fatalf("%d connections", dials)
	}
}

// The agents' list is checked: the first must be a model, ids unique, at most 20.
func TestConnectorAgentsValidation(t *testing.T) {
	fastBridges(t)
	_, base, device := vbHub(t)
	for _, x := range []struct{ agents, msg string }{
		{`[{"model":"claude-code/haiku"},{"model":"qwen"}]`, "must be a chat model"},
		{`[{"model":"qwen"},{"model":"qwen"}]`, "twice"},
		{`[{"model":"qwen","id":"a"},{"model":"claude-code/opus","id":"a"}]`, "twice"},
		{`[{"model":""}]`, "choose a model"},
		{`[{"model":"q","id":"has space"}]`, "without spaces"},
		{`[` + strings.TrimSuffix(strings.Repeat(`{"model":"m"},`, 21), ",") + `]`, "At most 20"},
	} {
		code, b := post(t, base+"/api/connectors", device, `{"url":"https://v.example","token":"vbp_x","enabled":false,"agents":`+x.agents+`}`)
		var e map[string]string
		json.Unmarshal(b, &e)
		if code != 400 || e["error"] != "agents" || !strings.Contains(e["message"], x.msg) {
			t.Errorf("%s: %d %s", x.agents, code, b)
		}
	}
	c := addConnector(t, base, device, `{"url":"https://v.example","token":"vbp_x","enabled":false}`)
	if c.Agents == nil || len(c.Agents) != 0 || c.Advertised == nil {
		t.Fatalf("default %+v", c)
	}
	code, out := patchConnector(t, base, device, c.ID, `{"agents":[{"model":" qwen3.6 ","name":" Qwen "},{"model":"claude-code/sonnet","kind":"model"}]}`)
	if code != 200 || fmtJSON(out["agents"]) != `[{"id":"qwen3.6","kind":"model","model":"qwen3.6","name":"Qwen"},{"id":"claude-code-sonnet","kind":"agent","model":"claude-code/sonnet","name":""}]` {
		t.Fatalf("patch %d %v", code, fmtJSON(out["agents"]))
	}
	if code, _ := patchConnector(t, base, device, c.ID, `{"agents":[{"model":"claude-code/sonnet"}]}`); code != 400 {
		t.Fatalf("patch to an agent first: %d", code)
	}
	if code, out := patchConnector(t, base, device, c.ID, `{"name":"x"}`); code != 200 || len(out["agents"].([]any)) != 2 {
		t.Fatalf("patch keeps the agents: %v", out)
	}
	if code, out := patchConnector(t, base, device, c.ID, `{"agents":[]}`); code != 200 || len(out["agents"].([]any)) != 0 {
		t.Fatalf("patch back to the default: %v", out)
	}
}

// Requests by agent: a model's (with vboard's tools, calls returned), the default when none is named, an unknown one;
// an agent's: a Claude Code turn on the owner's worker in a session of the conversation, with vboard's MCP server
// (the token in the environment only, bound to the board) and the board in the system prompt, tool calls as status
// lines, no tool calls in chatDone; the next turn in the same warm session; a new conversation fresh; cancel.
func TestVboardAgentChat(t *testing.T) {
	fastBridges(t)
	s, srv := newHub(t, t.TempDir())
	t.Cleanup(s.Close)
	base := srv.URL
	alice := signInAs(t, base, "Alice's laptop", "Alice")
	_, qwenTok := enroll(t, base, alice, "Studio")
	_, aliceTok := enroll(t, base, alice, "Alice's Mac")
	qmac := startVboardMac(t, base, qwenTok, "chat")
	mac := startFakeClaudeMac(t, base, aliceTok)
	v := newFakeVboard(t, stageAgents)
	c := addConnector(t, base, alice, `{"name":"Alice AI","url":"`+v.srv.URL+`","token":"vbp_secret",
		"agents":[{"model":"qwen3.6-35b-a3b"},{"id":"claude","model":"claude-code/haiku"}]}`)
	b := v.next(t)
	for len(b.agents) < 2 { // the claude worker may come after the first hello
		b.agents = nextUpdate(t, b)
	}

	// a model by its id, with tools: its calls come back
	tools := `[{"name":"create_items","description":"x","parameters":{"type":"object"}}]`
	_, ch := b.chatWith(`[{"role":"user","content":"hi"}]`, tools, `"agent":"qwen3.6-35b-a3b"`)
	evs := until(t, ch)
	if done := evs[len(evs)-1]; done.Type != "done" || len(done.ToolCalls) != 1 || done.ToolCalls[0].Name != "create_items" {
		t.Fatalf("model chat %+v", evs)
	}
	if r := qmac.chat.last(); len(r.Tools) != 1 || r.Owner != "" || r.Session != "" {
		t.Fatalf("model job %+v", r)
	}
	// no agent: the default (an older vboard, search)
	_, ch = b.chat(`[{"role":"user","content":"hi"}]`, tools)
	if done := until(t, ch); done[len(done)-1].Type != "done" || len(qmac.chat.got) != 2 {
		t.Fatalf("default chat %+v", done)
	}
	// an agent that is not offered
	_, ch = b.chatWith(`[{"role":"user","content":"hi"}]`, "", `"agent":"nope"`)
	if evs := until(t, ch); evs[0].Type != "error" || !strings.Contains(evs[0].Error, `"nope" is not available`) {
		t.Fatalf("unknown agent %+v", evs)
	}

	// an agent's first turn
	sys := `{"role":"system","content":"You are Claude, working for Anna on the vboard board \"Plan\" (id b_0123456789abcdef)."}`
	cx := `"agent":"claude","context":{"boardId":"b_0123456789abcdef","boardName":"Plan","userId":"u_1","userName":"Anna","role":"owner",` +
		`"lang":"en","selection":["i_1","i_2"],"view":{"x":10,"y":20,"w":800,"h":600}}`
	_, ch = b.chatWith(`[`+sys+`,{"role":"user","content":"add one yellow sticky saying hello"}]`, "", cx)
	evs = until(t, ch)
	var text strings.Builder
	var statuses []string
	for _, ev := range evs {
		switch ev.Type {
		case "delta":
			text.WriteString(ev.Text)
		case "status":
			statuses = append(statuses, ev.Text)
		}
	}
	done := evs[len(evs)-1]
	if done.Type != "done" || done.Content != "Added a yellow sticky." || text.String() != done.Content || done.ToolCalls == nil || len(done.ToolCalls) != 0 {
		t.Fatalf("agent turn %+v", evs)
	}
	if !contains(statuses, "create_items …") {
		t.Fatalf("statuses %q", statuses)
	}
	jobs1 := mac.jobsDone()
	if len(jobs1) != 1 {
		t.Fatalf("jobs %d", len(jobs1))
	}
	r := jobs1[0].Request
	if r.Owner != "Alice" || r.Client != "vboard: Alice AI" || !strings.HasPrefix(r.Session, "vboard:"+c.ID+":u_1:b_0123456789abcdef:") || !r.Fresh ||
		len(r.Tools) != 0 || len(r.MCP) != 1 || r.MCP[0].Name != "vboard" || r.MCP[0].URL != v.srv.URL+"/mcp?board=b_0123456789abcdef" ||
		r.MCP[0].Headers["Authorization"] != "Bearer vbp_secret" {
		t.Fatalf("agent job %+v", r)
	}
	last := r.Messages[len(r.Messages)-1].Content
	if !strings.HasPrefix(last, "[vboard, now: selected item(s): i_1, i_2; the person sees x 10, y 20, 800 × 600 px]") ||
		!strings.HasSuffix(last, "add one yellow sticky saying hello") {
		t.Fatalf("turn %q", last)
	}
	st := mac.starts(t)
	if len(st) != 1 {
		t.Fatalf("starts %d", len(st))
	}
	sp := argOf(st[0].Args, "--append-system-prompt")
	if !strings.Contains(sp, `working for Anna on the vboard board`) || !strings.Contains(sp, `MCP server "vboard"`) ||
		!strings.Contains(sp, `pass boardId "b_0123456789abcdef"`) || !strings.Contains(sp, "Anna (role on the board: owner)") {
		t.Fatalf("system prompt %q", sp)
	}
	if allowed := argOf(st[0].Args, "--allowedTools"); !strings.Contains(allowed, "mcp__vboard") || strings.Contains(allowed, "Bash") || strings.Contains(allowed, "Edit") {
		t.Fatalf("allowed %q", allowed)
	}
	if strings.Contains(st[0].McpRaw, "vbp_secret") || !strings.Contains(st[0].McpRaw, "${AIC_MCP_0_0}") || !strings.Contains(st[0].McpExpanded, "Bearer vbp_secret") ||
		!strings.Contains(st[0].McpRaw, "/mcp?board=b_0123456789abcdef") {
		t.Fatalf("mcp %s / %s", st[0].McpRaw, st[0].McpExpanded)
	}
	if strings.Contains(strings.Join(st[0].Args, " "), "vbp_secret") {
		t.Fatalf("the token in the arguments %q", st[0].Args)
	}

	// the next turn: the same session (one process), only the new message
	cx2 := strings.Replace(cx, `"selection":["i_1","i_2"],`, ``, 1)
	_, ch = b.chatWith(`[`+sys+`,{"role":"user","content":"add one yellow sticky saying hello"},{"role":"assistant","content":"Added a yellow sticky."},`+
		`{"role":"user","content":"thanks"}]`, "", cx2)
	evs = until(t, ch)
	if done := evs[len(evs)-1]; done.Type != "done" || !strings.HasPrefix(done.Content, "echo: [vboard, now: nothing is selected") || !strings.HasSuffix(done.Content, "thanks") ||
		strings.Contains(done.Content, "conversation so far") {
		t.Fatalf("second turn %+v", evs)
	}
	jobs2 := mac.jobsDone()
	if len(jobs2) != 2 || len(mac.starts(t)) != 1 || jobs2[0].Request.Session != r.Session && jobs2[1].Request.Session != r.Session {
		t.Fatalf("second turn: %d jobs, %d starts", len(jobs2), len(mac.starts(t)))
	}
	for _, j := range jobs2 {
		if j.ID != jobs1[0].ID && j.Request.Fresh {
			t.Fatalf("a follow-up is not fresh")
		}
	}

	// a cancel interrupts the turn; the session stays for the next one
	id, ch := b.chatWith(`[`+sys+`,{"role":"user","content":"add one yellow sticky saying hello"},{"role":"assistant","content":"Added a yellow sticky."},`+
		`{"role":"user","content":"be slow"}]`, "", cx2)
	waitFor(t, "the agent's turn running", func() bool {
		for _, j := range mac.mgr.Jobs() {
			if j.State == jobs.Running {
				return true
			}
		}
		return false
	})
	b.write(map[string]string{"type": "chatCancel", "id": id})
	waitFor(t, "the turn canceled", func() bool {
		for _, j := range mac.mgr.Jobs() {
			if j.State == jobs.Running || j.State == jobs.Queued {
				return false
			}
		}
		return true
	})
	time.Sleep(200 * time.Millisecond)
	for len(ch) > 0 {
		if ev := <-ch; ev.Type == "done" || ev.Type == "error" {
			t.Fatalf("after the cancel: %+v", ev)
		}
	}
	_, ch = b.chatWith(`[`+sys+`,{"role":"user","content":"add one yellow sticky saying hello"},{"role":"assistant","content":"Added a yellow sticky."},`+
		`{"role":"user","content":"again"}]`, "", cx2)
	if evs := until(t, ch); evs[len(evs)-1].Type != "done" || !strings.HasSuffix(evs[len(evs)-1].Content, "again") || len(mac.starts(t)) != 1 {
		t.Fatalf("after the cancel %+v (%d starts)", evs, len(mac.starts(t)))
	}

	// a new conversation that opens with the same words: a fresh session (a new process, nothing resumed)
	_, ch = b.chatWith(`[`+sys+`,{"role":"user","content":"add one yellow sticky saying hello"}]`, "", cx)
	if evs := until(t, ch); evs[len(evs)-1].Type != "done" {
		t.Fatalf("new conversation %+v", evs)
	}
	if st := mac.starts(t); len(st) != 2 || argOf(st[1].Args, "--resume") != "" {
		t.Fatalf("starts %d", len(st))
	}
	// another board: another session
	_, ch = b.chatWith(`[{"role":"user","content":"hi"}]`, "", strings.ReplaceAll(cx, "b_0123456789abcdef", "b_fedcba9876543210"))
	if evs := until(t, ch); evs[len(evs)-1].Type != "done" || len(mac.starts(t)) != 3 {
		t.Fatalf("another board %+v", evs)
	}

	// the owner's worker goes: Claude Code is no longer offered, and a request for it says so
	mac.stop()
	if got := nextUpdate(t, b); len(got) != 1 {
		t.Fatalf("update %+v", got)
	}
	_, ch = b.chatWith(`[{"role":"user","content":"hi"}]`, "", cx)
	if evs := until(t, ch); evs[0].Type != "error" || !strings.Contains(evs[0].Error, `"claude" is not available now`) {
		t.Fatalf("offline %+v", evs)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
