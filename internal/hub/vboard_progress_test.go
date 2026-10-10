package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// An agent that writes a large tool call (minutes for Opus) is not silent meanwhile: the worker's "writing
// create_items … 8 KB" statuses reach vboard as chatDelta status lines before the call's own line; the agent's
// effort (connector setting) reaches Claude Code as --effort.
func TestVboardAgentWritingAndEffort(t *testing.T) {
	fastBridges(t)
	s, srv := newHub(t, t.TempDir())
	t.Cleanup(s.Close)
	base := srv.URL
	alice := signInAs(t, base, "Alice's laptop", "Alice")
	_, qwenTok := enroll(t, base, alice, "Studio")
	_, aliceTok := enroll(t, base, alice, "Alice's Mac")
	startVboardMac(t, base, qwenTok, "chat")
	mac := startFakeClaudeMac(t, base, aliceTok)
	mac.am.Env = append(mac.am.Env, "AGENTS_FAKE_DELTA_MS=115") // 20 pieces of 1 KB: ~2.3 s, one status after 2 s
	v := newFakeVboard(t, stageAgents)
	c := addConnector(t, base, alice, `{"name":"Alice AI","url":"`+v.srv.URL+`","token":"vbp_secret",
		"agents":[{"model":"qwen3.6-35b-a3b"},{"id":"claude","model":"claude-code/opus","effort":" High "}]}`)
	if c.Agents[1].Effort != "high" {
		t.Fatalf("agents %+v", c.Agents)
	}
	b := v.next(t)
	for len(b.agents) < 2 {
		b.agents = nextUpdate(t, b)
	}
	cx := `"agent":"claude","context":{"boardId":"b_0123456789abcdef","userId":"u_1","userName":"Anna","conversationId":"conv-w"}`
	_, ch := b.chatWith(`[{"role":"user","content":"bigwrite twenty items"}]`, "", cx)
	evs := until(t, ch)
	var statuses []string
	for _, ev := range evs {
		if ev.Type == "status" {
			statuses = append(statuses, ev.Text)
		}
	}
	if done := evs[len(evs)-1]; done.Type != "done" || done.Content != "Added 20 items." {
		t.Fatalf("turn %+v", evs)
	}
	writing, call := -1, -1
	for i, st := range statuses {
		if strings.HasPrefix(st, "writing create_items … ") && strings.HasSuffix(st, " KB") && writing < 0 {
			writing = i
		}
		if st == "create_items …" {
			call = i
		}
	}
	if writing < 0 || call < writing {
		t.Fatalf("statuses %q", statuses)
	}
	j := mac.jobsDone()
	if len(j) != 1 || j[0].Request.Effort != "high" {
		t.Fatalf("jobs %+v", j)
	}
	if st := mac.starts(t); len(st) != 1 || argOf(st[0].Args, "--effort") != "high" {
		t.Fatalf("starts %+v", st)
	}

	// /v1/chat/completions: the stream keeps alive (": ping" every chatPoll) while the call is written; its
	// reasoning_effort is Claude Code's effort
	defer func(d time.Duration) { chatPoll = d }(chatPoll)
	chatPoll = 200 * time.Millisecond
	_, app := makeClient(t, base, alice, "app")
	code, out := post(t, base+"/v1/chat/completions", app, `{"model":"claude-code/haiku","stream":true,"reasoning_effort":"low",`+
		`"messages":[{"role":"user","content":"bigwrite"}]}`)
	if code != 200 || !strings.Contains(string(out), `"20 items."`) || !strings.Contains(string(out), "[DONE]") ||
		strings.Count(string(out), ": ping") < 5 {
		t.Fatalf("v1 stream %d %s", code, out)
	}
	if j := mac.jobsDone(); len(j) != 2 || j[0].Request.Effort != "low" && j[1].Request.Effort != "low" {
		t.Fatalf("v1 job effort %+v", j)
	}

	// an effort is for Claude Code agents only, and one of Claude Code's levels
	for _, agents := range []string{`[{"model":"qwen","effort":"high"}]`, `[{"model":"qwen"},{"model":"claude-code/opus","effort":"extreme"}]`} {
		code, out := post(t, base+"/api/connectors", alice, `{"url":"https://v.example","token":"vbp_x","enabled":false,"agents":`+agents+`}`)
		var e map[string]string
		json.Unmarshal(out, &e)
		if code != 400 || e["error"] != "agents" || !strings.Contains(e["message"], "effort") {
			t.Errorf("%s: %d %s", agents, code, out)
		}
	}
}
