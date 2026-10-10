// Package e2e tests Claude Code agents end to end on a worker: the claude-code engine backend in the job manager
// and the worker API, with the fake claude (internal/agents/fakeclaude) standing in for Claude Code.
package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/agents"
	"github.com/philipparndt/ai-connector/internal/agents/fakeclaude"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/llm"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/web"
)

func TestMain(m *testing.M) {
	if fakeclaude.Active() {
		os.Exit(fakeclaude.Main(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func wait(t *testing.T, m *jobs.Manager, id string, st jobs.State) *jobs.Job {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if j, ok := m.Job(id); ok && j.State == st {
			return j
		}
	}
	j, _ := m.Job(id)
	t.Fatalf("job %s: %s (%s), want %s", id, j.State, j.Error, st)
	return nil
}

func TestClaudeCodeJobs(t *testing.T) {
	log := filepath.Join(t.TempDir(), "claude.log")
	am := &agents.Manager{Root: t.TempDir(), Command: os.Args[0], Env: []string{fakeclaude.Env, "AGENTS_FAKE_LOG=" + log}, Version: "9.9.9"}
	defer am.Close()
	cat := &engine.Catalog{}
	b := &engine.ClaudeCodeBackend{Agents: am, Catalog: cat}
	if ms := b.Discover(); len(ms) != 3 || cat.Find("claude-code/sonnet") == nil || cat.Find("claude-code/sonnet").Kind != engine.Chat {
		t.Fatalf("models %v", ms)
	}
	dir := t.TempDir()
	mgr := jobs.New(cat, []engine.Backend{b}, dir)
	t.Cleanup(func() {
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !mgr.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	chat := func(text string) engine.Request {
		return engine.Request{Model: "claude-code/sonnet", Session: "board:1", Owner: "alice",
			Messages: []llm.Message{{Role: "system", Content: "Be brief."}, {Role: "user", Content: "earlier"}, {Role: "assistant", Content: "ok"}, {Role: "user", Content: text}}}
	}
	// three turns of one conversation: one process; only the last message is sent (the session has the history)
	var session string
	for _, q := range []string{"one", "two", "use a tool"} {
		j, err := mgr.Submit(chat(q))
		if err != nil {
			t.Fatal(err)
		}
		j = wait(t, mgr, j.ID, jobs.Done)
		if q != "use a tool" && j.Text != "echo: "+q {
			t.Fatalf("%q: text %q", q, j.Text)
		}
		if s, _ := j.Stats["session"].(string); s == "" || session != "" && s != session {
			t.Fatalf("stats %v", j.Stats)
		} else {
			session = s
		}
		if j.Stats["tokens"] != 5 || j.Stats["driver"] != "json" || j.Stats["costUSD"] == nil {
			t.Fatalf("stats %v", j.Stats)
		}
		if q == "use a tool" {
			if j.Text != "Let me look.\n\nFound it." || len(j.Tools) != 1 || j.Tools[0]["id"] != "toolu_1" || j.Tools[0]["state"] != "done" ||
				j.Tools[0]["name"] != "Read" || j.Tools[0]["summary"] != "/x/notes.txt" || j.Tools[0]["result"] != "the notes" ||
				j.Tools[0]["arguments"] == nil {
				t.Fatalf("tool job: %q %v", j.Text, j.Tools)
			}
		}
	}
	b2, _ := os.ReadFile(log)
	if n := strings.Count(string(b2), "start "); n != 1 {
		t.Fatalf("%d processes:\n%s", n, b2)
	}

	// a slow turn is canceled like any job; the session stays
	j, _ := mgr.Submit(chat("be slow"))
	wait(t, mgr, j.ID, jobs.Running)
	time.Sleep(100 * time.Millisecond)
	mgr.Cancel(j.ID)
	wait(t, mgr, j.ID, jobs.Canceled)
	j, _ = mgr.Submit(chat("after"))
	if j = wait(t, mgr, j.ID, jobs.Done); j.Text != "echo: after" {
		t.Fatalf("after cancel %q", j.Text)
	}

	// a one-off turn (no session) gets the conversation as a transcript in front
	r := chat("alone")
	r.Session = ""
	j, _ = mgr.Submit(r)
	if j = wait(t, mgr, j.ID, jobs.Done); !strings.Contains(j.Text, "User: earlier") || !strings.Contains(j.Text, "Assistant: ok") || !strings.HasSuffix(j.Text, "alone") {
		t.Fatalf("one-off %q", j.Text)
	}

	// the worker API, as the hub calls it (with its route)
	api := httptest.NewServer((&web.Server{Jobs: mgr, Prompts: prompts.Open(dir), Agents: am}).Handler())
	defer api.Close()
	get := func(method, path string) *http.Response {
		req, _ := http.NewRequest(method, api.URL+path, nil)
		req.Header.Set(web.RouteHeader, "m3")
		req.Header.Set(web.WorkerIDHeader, "w1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := get("GET", "/api/agents/sessions")
	var list []map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 || list[0]["id"] != "m3."+session || list[0]["owner"] != "alice" || list[0]["conversation"] != "board:1" ||
		list[0]["worker"].(map[string]any)["id"] != "w1" {
		t.Fatalf("sessions %v", list)
	}
	if resp := get("GET", "/api/agents/sessions/m3."+session+"/tty"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("tty of a json session: %d", resp.StatusCode)
	}
	if resp := get("DELETE", "/api/agents/sessions/m3."+session); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("stop: %d", resp.StatusCode)
	}
	if resp := get("DELETE", "/api/agents/sessions/m3."+session); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stop again: %d", resp.StatusCode)
	}
	// the worker's state does not count Claude Code turns as busy
	j, _ = mgr.Submit(chat("be slow"))
	wait(t, mgr, j.ID, jobs.Running)
	resp = get("GET", "/api/info")
	var info struct {
		State struct {
			Running *string
		}
	}
	json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if info.State.Running != nil {
		t.Fatalf("running %v", *info.State.Running)
	}
	mgr.Cancel(j.ID)
	wait(t, mgr, j.ID, jobs.Canceled)
}
