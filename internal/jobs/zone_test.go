package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/agent"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/llm"
)

// agentChat is a chat backend that runs the real agent loop with a scripted model: "fetch" calls web_fetch,
// "light" calls switch_light (approval), anything else is answered at once.
type agentChat struct{ cfg agent.Config }

type script struct{ outs []string }

func (s *script) Format() string { return "json" }
func (s *script) Generate(_ context.Context, _ string, on func(string)) error {
	out := "answer"
	if len(s.outs) > 0 {
		out, s.outs = s.outs[0], s.outs[1:]
	}
	on(out)
	return nil
}

type tb struct{ tools []agent.Tool }

func (b *tb) Tools(context.Context) ([]agent.Tool, error) { return b.tools, nil }
func (b *tb) Call(_ context.Context, name string, _ json.RawMessage) (agent.Result, error) {
	return agent.Result{Text: "result of " + name}, nil
}
func (b *tb) Close() error { return nil }

func newAgentChat() *agentChat {
	home := &tb{tools: []agent.Tool{{Name: "switch_light", Changes: true}}}
	web := &tb{tools: []agent.Tool{{Name: "web_fetch"}}}
	return &agentChat{cfg: agent.Config{
		Home:            func(context.Context) ([]agent.Toolbox, error) { return []agent.Toolbox{home}, nil },
		Web:             func(context.Context) (agent.Toolbox, error) { return web, nil },
		ApprovalTimeout: 5 * time.Second,
	}}
}

func (a *agentChat) Name() string   { return "a" }
func (a *agentChat) Heavy() bool    { return false }
func (a *agentChat) Loaded() string { return "" }
func (a *agentChat) Unload()        {}
func (a *agentChat) Run(ctx context.Context, _ *engine.Model, r engine.Request, out string, emit engine.Emit) error {
	s := &script{}
	switch r.Messages[len(r.Messages)-1].Content {
	case "fetch":
		s.outs = []string{`<tool_call>{"name": "web_fetch", "arguments": {}}</tool_call>`, "fetched"}
	case "light":
		s.outs = []string{`<tool_call>{"name": "switch_light", "arguments": {}}</tool_call>`, "switched"}
	}
	_, err := agent.Run(ctx, s, a.cfg, agent.Request{Zone: agent.Zone(r.Zone), Messages: r.Messages, OutDir: out},
		func(ev agent.Event) { emit(engine.Event{Type: ev.Type, Text: ev.Text, File: ev.File, Data: ev.Data}) })
	return err
}

func turn(t *testing.T, m *Manager, r engine.Request) *Job {
	t.Helper()
	j, err := m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	return wait(t, m, j.ID)
}

func TestZoneKeptAcrossTurns(t *testing.T) {
	m := setup(t, newAgentChat())
	first := turn(t, m, engine.Request{Model: "chat-a", Zone: "web", Messages: []llm.Message{{Role: "user", Content: "fetch"}}})
	if first.State != Done || first.Text != "fetched" {
		t.Fatalf("%+v", first)
	}
	if len(first.Tools) != 1 || first.Tools[0]["state"] != "done" {
		t.Fatalf("tools %v", first.Tools)
	}
	hist := ResultMessages(first)
	if len(hist) != 4 || hist[2].Role != "tool" {
		t.Fatalf("%+v", hist)
	}
	next := append(append([]llm.Message(nil), hist...), llm.Message{Role: "user", Content: "light"})

	// a web chat cannot become a home chat
	if _, err := m.Submit(engine.Request{Model: "chat-a", Zone: "home", Parent: first.ID, Messages: next}); err == nil ||
		err.Error() != "a web chat stays a web chat" {
		t.Fatal(err)
	}
	// nor can its history be carried into a new home chat, or one that continues a plain chat
	if _, err := m.Submit(engine.Request{Model: "chat-a", Zone: "home", Messages: next}); err == nil {
		t.Fatal("web results in a new home chat")
	}
	plain := turn(t, m, engine.Request{Model: "chat-a", Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if _, err := m.Submit(engine.Request{Model: "chat-a", Parent: plain.ID, Messages: next}); err == nil {
		t.Fatal("web results in a plain chat")
	}
	// a zone for an image: no
	if _, err := m.Submit(engine.Request{Model: "img-b", Zone: "web", Prompt: "x"}); err == nil {
		t.Fatal("zone on an image")
	}

	// the follow-up without a zone takes the parent's; the web chat refuses the home tool
	second := turn(t, m, engine.Request{Model: "chat-a", Parent: first.ID, Messages: next})
	if second.Request.Zone != "web" || second.State != Done {
		t.Fatalf("%+v", second)
	}
	if len(second.Tools) != 1 || second.Tools[0]["state"] != "blocked" {
		t.Fatalf("tools %v", second.Tools)
	}

	// the history survives a restart and still continues (the job is done a moment before it is saved)
	saved(t, m)
	m2 := New(m.Catalog, []engine.Backend{newAgentChat()}, m.Dir)
	settle(t, m2)
	again, _ := m2.Job(second.ID)
	h2 := ResultMessages(again)
	if len(h2) != len(ResultMessages(second)) {
		t.Fatalf("%d messages after reload", len(h2))
	}
	third := append(h2, llm.Message{Role: "user", Content: "thanks"})
	j := turn(t, m2, engine.Request{Model: "chat-a", Parent: second.ID, Messages: third})
	if j.State != Done || j.Request.Zone != "web" {
		t.Fatalf("%+v", j)
	}
	saved(t, m2)
}

// saved waits until the manager is idle and its history written.
func saved(t *testing.T, m *Manager) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if m.Settled() {
			return
		}
	}
	t.Fatal("not saved")
}

func TestApproveThroughManager(t *testing.T) {
	for _, allow := range []bool{true, false} {
		m := setup(t, newAgentChat())
		ch, stop := m.Subscribe()
		j, err := m.Submit(engine.Request{Model: "chat-a", Zone: "home", Messages: []llm.Message{{Role: "user", Content: "light"}}})
		if err != nil {
			t.Fatal(err)
		}
		for u := range ch {
			if ev := u.Event; u.JobID == j.ID && ev != nil && ev.Type == "tool" && ev.Data["state"] == "approval" {
				if err := m.Approve(j.ID, "wrong", true); !errors.Is(err, agent.ErrNotWaiting) {
					t.Fatal(err)
				}
				if err := m.Approve(j.ID, ev.Data["id"].(string), allow); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
		stop()
		done := wait(t, m, j.ID)
		saved(t, m)
		want := "denied"
		if allow {
			want = "done"
		}
		if len(done.Tools) != 1 || done.Tools[0]["state"] != want {
			t.Fatalf("allow %v: %v", allow, done.Tools)
		}
		if err := m.Approve(j.ID, "x", true); !errors.Is(err, ErrNotRunning) {
			t.Fatal(err)
		}
		if msgs := ResultMessages(done); !strings.Contains(msgs[2].Content, map[bool]string{true: "result of", false: "did not allow"}[allow]) {
			t.Fatal(msgs[2].Content)
		}
	}
}

// "Always allow" on an approval approves the call and lists the tool; the next chat runs it without asking.
func TestAllowAlwaysThroughManager(t *testing.T) {
	a := newAgentChat()
	m := setup(t, a)
	a.cfg.Always = m.ApprovalStore().Has
	ch, stop := m.Subscribe()
	j, err := m.Submit(engine.Request{Model: "chat-a", Zone: "home", Messages: []llm.Message{{Role: "user", Content: "light"}}})
	if err != nil {
		t.Fatal(err)
	}
	for u := range ch {
		if ev := u.Event; u.JobID == j.ID && ev != nil && ev.Type == "tool" && ev.Data["state"] == "approval" {
			if err := m.AllowAlways(j.ID, "wrong"); !errors.Is(err, agent.ErrNotWaiting) {
				t.Fatal(err)
			}
			if err := m.AllowAlways(j.ID, ev.Data["id"].(string)); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	stop()
	done := wait(t, m, j.ID)
	if len(done.Tools) != 1 || done.Tools[0]["state"] != "done" || done.Tools[0]["asked"] != true || done.Tools[0]["auto"] != nil {
		t.Fatalf("%v", done.Tools)
	}
	if l, _ := m.ApprovalStore().List(); strings.Join(l, ",") != "switch_light" {
		t.Fatal(l)
	}
	if err := m.AllowAlways(j.ID, "x"); !errors.Is(err, ErrNotRunning) {
		t.Fatal(err)
	}
	// the next chat: no approval at all
	next := turn(t, m, engine.Request{Model: "chat-a", Zone: "home", Messages: []llm.Message{{Role: "user", Content: "light"}}})
	if len(next.Tools) != 1 || next.Tools[0]["state"] != "done" || next.Tools[0]["auto"] != true || next.Tools[0]["asked"] != nil {
		t.Fatalf("%v", next.Tools)
	}
	saved(t, m)
}
