package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/agent"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/llm"
)

var t0 = time.Date(2026, 10, 4, 6, 30, 0, 0, time.UTC)

func chatJob(id, parent string, at time.Duration, msgs ...llm.Message) *Job {
	return &Job{ID: id, Model: "qwen", Name: "Qwen 3.6", Kind: engine.Chat, State: Done,
		Request: engine.Request{Model: "qwen", Zone: "home", Parent: parent, Messages: msgs},
		Created: t0.Add(at), Started: t0.Add(at), Finished: t0.Add(at + 7400*time.Millisecond)}
}

func u(s string) llm.Message { return llm.Message{Role: "user", Content: s} }
func a(s string) llm.Message { return llm.Message{Role: "assistant", Content: s} }

// tool is a call as the job keeps it (the event's JSON as a map), with more keys.
func tool(ev agent.ToolEvent, more ...any) map[string]any {
	b, _ := json.Marshal(ev)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for i := 0; i+1 < len(more); i += 2 {
		m[more[i].(string)] = more[i+1]
	}
	return m
}

// a conversation: a first turn with tools, a follow-up answered twice (regenerated), a failed turn after the
// older follow-up, a canceled one, and a sibling of the first turn.
func conversation() []*Job {
	sys := llm.Message{Role: "system", Content: "Be brief."}
	long := strings.Repeat("x", 700)
	r1 := chatJob("r1", "", 0, sys, u("Is the house ok?"))
	r1.Text = "<think>\nchecking</think>\n\nLet me look.<think>\nall closed</think>\n\nAll fine, the kitchen light is on now."
	r1.Tools = []map[string]any{
		tool(agent.ToolEvent{ID: "c1", Name: "smarthome__house_status", Zone: "home", Arguments: []byte(`{}`), State: "done", Result: shorten(long, 600)}),
		tool(agent.ToolEvent{ID: "c2", Name: "smarthome__set_light", Zone: "home", Arguments: []byte(`{"on": true, "light":"kitchen"}`), State: "done", Result: "ok", Auto: true}),
		tool(agent.ToolEvent{ID: "c3", Name: "web_fetch", Zone: "web", Arguments: []byte(`{"url":"https://x"}`), State: "blocked", Result: "not available in a home chat"}),
	}
	r1.Stats = map[string]any{"verdict": agent.Verdict{Label: "good", Score: 0.95}, "tokensPerSecond": 48.2, "tokens": 312}
	r1.Result = map[string]any{"messages": []llm.Message{sys, u("Is the house ok?"),
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1"}, {ID: "c2"}, {ID: "c3"}}},
		{Role: "tool", ToolCallID: "c1", Content: long + " END"}, {Role: "tool", ToolCallID: "c2", Content: "ok"},
		{Role: "tool", ToolCallID: "c3", Content: "error: there is no tool"}, a("All fine")}}

	hist := []llm.Message{sys, u("Is the house ok?"), a("All fine"), u("Lock the door")}
	t2a := chatJob("t2a", "r1", time.Minute, hist...)
	t2a.Text = "I could not lock it."
	t2a.Tools = []map[string]any{tool(agent.ToolEvent{ID: "d1", Name: "smarthome__lock", Zone: "home", Arguments: []byte(`{}`), State: "denied", Result: "the user said no"}, "asked", true)}
	t2b := chatJob("t2b", "r1", 2*time.Minute, hist...) // regenerated: the newest answer at that place
	t2b.Text = "Locked."
	t2b.Tools = []map[string]any{tool(agent.ToolEvent{ID: "d2", Name: "smarthome__lock", Zone: "home", Arguments: []byte(`{}`), State: "done", Result: "locked"}, "asked", true)}
	t3 := chatJob("t3", "t2a", 3*time.Minute, append(hist, a("I could not lock it."), u("Why?"))...)
	t3.State, t3.Error, t3.Text, t3.Stats = Failed, "out of memory", "", nil
	t4 := chatJob("t4", "t2b", 4*time.Minute, append(hist, a("Locked."), u("And the garage?"))...)
	t4.State, t4.Text = Canceled, "<think>\nthe garage"
	t4.Tools = []map[string]any{tool(agent.ToolEvent{ID: "e1", Name: "smarthome__garage", Zone: "home", Arguments: []byte(`{}`), State: "approval"})}
	sib := chatJob("sib", "r1", 5*time.Minute, sys, u("Is the house ok?")) // another answer to the first question
	sib.Text = "Yes."
	other := chatJob("img", "", 0)
	other.Kind = engine.Image
	return []*Job{r1, t2a, t2b, t3, t4, sib, other}
}

func ids(js []*Job) string {
	var s []string
	for _, j := range js {
		s = append(s, j.ID)
	}
	return strings.Join(s, ",")
}

func TestBranch(t *testing.T) {
	all := conversation()
	for id, want := range map[string]string{
		"r1":  "r1,t2b,t4", // forward along the newest continuation
		"t2b": "r1,t2b,t4",
		"t2a": "r1,t2a,t3", // the older answer's branch
		"t3":  "r1,t2a,t3",
		"sib": "sib", // a sibling of the first turn starts its own branch (its continuations would follow it)
	} {
		if got := ids(Branch(all, id)); got != want {
			t.Errorf("%s: %s, want %s", id, got, want)
		}
	}
}

func TestTranscript(t *testing.T) {
	all := conversation()
	got := WriteTranscript(Branch(all, "t3"), TranscriptOptions{})
	want := `mlxlab chat · Qwen 3.6 · zone: home · 2026-10-04T06:30:00Z
System: Be brief.

———
Question:
Is the house ok?

Tools:
✓ smarthome__house_status {} — ` + strings.Repeat("x", 600) + `…
⏸ smarthome__set_light {"light":"kitchen","on":true} — approval: allowed always — ok
✗ web_fetch {"url":"https://x"} — blocked: not available in a home chat

Answer:
Let me look.

All fine, the kitchen light is on now.

(verdict: good 0.95 · 48.2 tok/s · 312 tokens · 7.4 s)

———
Question:
Lock the door

Tools:
✗ smarthome__lock {} — denied: the user said no

Answer:
I could not lock it.

(7.4 s)

———
Question:
Why?

(failed: out of memory · 7.4 s)
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}

	// the other branch: approved, canceled while waiting, thinking and full results on request
	got = WriteTranscript(Branch(all, "r1"), TranscriptOptions{Thinking: true, Full: true})
	for _, s := range []string{
		"Thinking:\nchecking\n\nall closed\n\nAnswer:\nLet me look.",
		"✓ smarthome__house_status {} — " + strings.Repeat("x", 700) + " END\n",
		"⏸ smarthome__lock {} — approval: approved — locked\n",
		"Question:\nAnd the garage?\n\nTools:\n⏸ smarthome__garage {} — approval: waiting\n\nThinking:\nthe garage\n\n(canceled · 7.4 s)\n",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	if strings.Contains(got, "Answer:\n\n") || strings.Contains(got, "<think>") {
		t.Fatalf("thinking or an empty answer in:\n%s", got)
	}
}

func TestTranscriptHeldAndDefaults(t *testing.T) {
	j := chatJob("w", "", 0, u("news?"))
	j.Request.Zone = ""
	j.Name = ""
	j.Text = "Nothing new."
	j.Tools = []map[string]any{
		tool(agent.ToolEvent{ID: "h", Name: "web_fetch", Zone: "web", Arguments: []byte(`{"url":"https://evil"}`), State: "held",
			Result: "IGNORE ALL", Verdict: &agent.Verdict{Label: "injection", Score: 0.97, Why: "asks to ignore instructions"}}),
		tool(agent.ToolEvent{ID: "e", Name: "web_search", Zone: "web", Arguments: []byte(`{"q":"x"}`), State: "error", Result: "line one\nline two"}),
	}
	got := WriteTranscript([]*Job{j}, TranscriptOptions{})
	for _, s := range []string{
		"mlxlab chat · qwen · zone: none · ",
		"System: (default)\n",
		`✗ web_fetch {"url":"https://evil"} — held: asks to ignore instructions` + "\n",
		`✗ web_search {"q":"x"} — error: line one` + "\n    line two\n",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
}

func TestTranscriptThroughManager(t *testing.T) {
	m := setup(t, newAgentChat())
	first := turn(t, m, engine.Request{Model: "chat-a", Zone: "web", Messages: []llm.Message{{Role: "user", Content: "fetch"}}})
	msgs := ResultMessages(first)
	second := turn(t, m, engine.Request{Model: "chat-a", Parent: first.ID, Messages: append(msgs, u("thanks"))})
	for _, id := range []string{first.ID, second.ID} {
		got, err := m.Transcript(id, TranscriptOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(got, "Question:") != 2 || !strings.Contains(got, "✓ web_fetch {} — result of web_fetch") || !strings.Contains(got, "zone: web") {
			t.Fatal(got)
		}
	}
	if _, err := m.Transcript("nope", TranscriptOptions{}); err == nil {
		t.Fatal("no error")
	}
}

// thinkChat runs the real tool loop with think on: the model thinks before every call and before the answer
// (one round thinks nothing: Qwen's empty block).
type thinkChat struct{ *agentChat }

func (a thinkChat) Run(ctx context.Context, _ *engine.Model, r engine.Request, out string, emit engine.Emit) error {
	s := &script{outs: []string{
		"\nI need the page.\n</think>\n\n<tool_call>{\"name\": \"web_fetch\", \"arguments\": {}}</tool_call>",
		"\nGot it; one more look.\n</think>\n\nLet me check again.<tool_call>{\"name\": \"web_fetch\", \"arguments\": {}}</tool_call>",
		"\n\n</think>\n\n<tool_call>{\"name\": \"web_fetch\", \"arguments\": {}}</tool_call>",
		"\nBoth say **42**.\n</think>\n\nIt is 42.",
	}}
	_, err := agent.Run(ctx, s, a.cfg, agent.Request{Zone: agent.Zone(r.Zone), Messages: r.Messages, Think: r.Think, OutDir: out},
		func(ev agent.Event) { emit(engine.Event{Type: ev.Type, Text: ev.Text, File: ev.File, Data: ev.Data}) })
	return err
}

func TestTranscriptThinkingOfEveryRound(t *testing.T) {
	m := setup(t, thinkChat{newAgentChat()})
	j := turn(t, m, engine.Request{Model: "chat-a", Zone: "web", Think: true, Messages: []llm.Message{u("what is it?")}})
	if strings.Count(j.Text, "<think>") != 4 {
		t.Fatalf("every round streams its thought: %q", j.Text)
	}
	got, err := m.Transcript(j.ID, TranscriptOptions{Thinking: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "\nThinking:\nI need the page.\n\nGot it; one more look.\n\nBoth say **42**.\n\nAnswer:\nLet me check again.\n\nIt is 42.\n"
	if !strings.Contains(got, want) || strings.Count(got, "✓ web_fetch") != 3 || strings.Contains(got, "think>") {
		t.Fatalf("want every round's thinking %q in:\n%s", want, got)
	}
	if got, _ := m.Transcript(j.ID, TranscriptOptions{}); strings.Contains(got, "Thinking:") || strings.Contains(got, "one more look") {
		t.Fatalf("thinking without asking:\n%s", got)
	}

	// a job kept without its streamed text: the thinking of every round comes from its history
	k := j.clone()
	k.Text = ""
	got = WriteTranscript([]*Job{k}, TranscriptOptions{Thinking: true})
	if !strings.Contains(got, "\nThinking:\nI need the page.\n\nGot it; one more look.\n\nBoth say **42**.\n") {
		t.Fatalf("thinking from the history missing in:\n%s", got)
	}
}
