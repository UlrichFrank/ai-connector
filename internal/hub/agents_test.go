package hub

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/philipparndt/ai-connector/internal/agents"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/web"
)

// signInAs signs a device in for a user.
func signInAs(t *testing.T, base, device, user string) string {
	t.Helper()
	r, out := loginWith(t, base, map[string]string{"password": password, "device": device, "user": user})
	if r.StatusCode != 200 {
		t.Fatalf("login %s: %d", user, r.StatusCode)
	}
	return out["token"].(string)
}

// startClaudeMac is a worker whose catalogue has a Claude Code model (run by the fake backend) and, when given, the
// agents manager whose sessions the API serves.
func startClaudeMac(t *testing.T, base, token string, am *agents.Manager) (*jobs.Manager, func()) {
	t.Helper()
	cat := &engine.Catalog{Models: []*engine.Model{
		{ID: "claude-code/sonnet", Name: "Claude Code · Sonnet", Kind: engine.Chat, Backend: "fake", Here: true},
		{ID: "small", Name: "Small", Kind: engine.Image, Backend: "fake", Size: 2 * gib, Here: true},
	}}
	dir := t.TempDir()
	mgr := jobs.New(cat, []engine.Backend{&fakeBackend{}}, dir)
	api := (&web.Server{Jobs: mgr, Prompts: prompts.Open(dir), Version: "test", Agents: am}).Handler()
	stop, _ := connectWorkerWith(t, base, token, api)
	t.Cleanup(func() {
		stop()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !mgr.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	return mgr, stop
}

func claudeJob(t *testing.T, base, device, extra string) (int, jobView, map[string]string) {
	t.Helper()
	return submitJob(t, base, device, `{"model":"claude-code/sonnet","messages":[{"role":"user","content":"hi"}]`+extra+`}`)
}

// Claude Code jobs go only to a worker of the signed-in person (the server sets the owner); elsewhere they fail fast.
func TestClaudeOwnerRouting(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	base := srv.URL
	alice := signInAs(t, base, "Alice's laptop", "Alice")
	bob := signInAs(t, base, "Bob's phone", "Bob")
	carol := signInAs(t, base, "Carol's phone", "Carol")

	// all offline: alice's job says whose worker it waits for
	if code, _, e := claudeJob(t, base, alice, ""); code != 409 || e["error"] != "no-worker" {
		t.Fatalf("no worker yet: %d %v", code, e)
	}
	aliceID, aliceTok := enroll(t, base, alice, "Alice's Mac")
	bobID, bobTok := enroll(t, base, bob, "Bob's Mac")
	ws := workerItems(t, base, alice)
	if ws[aliceID].Owner != "Alice" || ws[bobID].Owner != "Bob" {
		t.Fatalf("owners %+v", ws)
	}
	if code, _, e := claudeJob(t, base, alice, ""); code != 503 || e["error"] != "offline" || e["message"] != "Alice's Claude Code worker is offline" {
		t.Fatalf("all offline: %d %v", code, e)
	}

	_, stopAlice := startClaudeMac(t, base, aliceTok, nil)
	startClaudeMac(t, base, bobTok, nil)
	waitFor(t, "both known", func() bool {
		ws = workerItems(t, base, alice)
		return ws[aliceID].Specs != nil && ws[bobID].Specs != nil
	})
	aliceRoute, bobRoute := ws[aliceID].Route, ws[bobID].Route

	for i := 0; i < 4; i++ {
		// a caller's owner is replaced by the signed-in person
		code, j, e := claudeJob(t, base, alice, `,"owner":"Bob"`)
		if code != 200 || !strings.HasPrefix(j.ID, aliceRoute+".") || j.Request["owner"] != "Alice" {
			t.Fatalf("alice's job %d: %d %+v %v", i, code, j, e)
		}
		code, j, e = claudeJob(t, base, bob, "")
		if code != 200 || !strings.HasPrefix(j.ID, bobRoute+".") || j.Request["owner"] != "Bob" {
			t.Fatalf("bob's job %d: %d %+v %v", i, code, j, e)
		}
	}
	// a pin to someone else's worker is refused; to one's own it holds
	if code, _, e := claudeJob(t, base, alice, `,"worker":"`+bobID+`"`); code != 403 {
		t.Fatalf("alice pins bob's worker: %d %v", code, e)
	}
	if code, j, _ := claudeJob(t, base, alice, `,"worker":"`+aliceID+`"`); code != 200 || !strings.HasPrefix(j.ID, aliceRoute+".") {
		t.Fatalf("alice pins her worker: %d %+v", code, j)
	}
	// someone without a worker
	if code, _, e := claudeJob(t, base, carol, ""); code != 409 || e["error"] != "no-worker" || !strings.Contains(e["message"], "Carol") {
		t.Fatalf("carol: %d %v", code, e)
	}
	// other models route as before (to any worker)
	if code, _, e := submitJob(t, base, carol, `{"model":"small","prompt":"x"}`); code != 200 {
		t.Fatalf("an image job: %d %v", code, e)
	}

	// alice's worker goes offline: her job fails fast, never lands on bob's
	stopAlice()
	waitFor(t, "alice's worker gone", func() bool { return !workerItems(t, base, alice)[aliceID].Connected })
	code, _, e := claudeJob(t, base, alice, "")
	if code != 503 || e["message"] != "Alice's Claude Code worker is offline" {
		t.Fatalf("alice offline: %d %v", code, e)
	}
	if code, j, _ := claudeJob(t, base, bob, ""); code != 200 || !strings.HasPrefix(j.ID, bobRoute+".") {
		t.Fatalf("bob still: %d %+v", code, j)
	}

	// an owner device sets a worker's owner
	r := do(t, "PATCH", base+"/api/workers/"+bobID, alice, strings.NewReader(`{"owner":"Carol"}`))
	r.Body.Close()
	if r.StatusCode != 200 || workerItems(t, base, alice)[bobID].Owner != "Carol" {
		t.Fatalf("patch owner: %d", r.StatusCode)
	}
	if code, j, _ := claudeJob(t, base, carol, ""); code != 200 || !strings.HasPrefix(j.ID, bobRoute+".") {
		t.Fatalf("carol now: %d %+v", code, j)
	}
	if code, _, _ := claudeJob(t, base, bob, ""); code != 409 {
		t.Fatalf("bob has no worker now: %d", code)
	}
	if r := do(t, "PATCH", base+"/api/workers/nope", alice, strings.NewReader(`{"owner":"x"}`)); r.StatusCode != 404 {
		t.Fatalf("unknown worker: %d", r.StatusCode)
	}
}

// catPTY is a stand-in for claude in a terminal: it echoes what is typed (the terminal echoes too).
func catPTY(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexec cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A person's tty session on their worker: opened, listed, attached through the hub's tunnel (a WebSocket through
// the reverse proxy), resized, stopped — and nobody else's to see or touch.
func TestClaudeSessionsThroughHub(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no tty on Windows")
	}
	_, srv := newHub(t, t.TempDir())
	base := srv.URL
	alice := signInAs(t, base, "Alice's laptop", "Alice")
	bob := signInAs(t, base, "Bob's phone", "Bob")
	aliceID, aliceTok := enroll(t, base, alice, "Alice's Mac")
	am := &agents.Manager{Root: t.TempDir(), Command: catPTY(t), Version: "test"}
	t.Cleanup(am.Close)
	startClaudeMac(t, base, aliceTok, am)
	waitFor(t, "alice's worker known", func() bool { return workerItems(t, base, alice)[aliceID].Specs != nil })
	route := workerItems(t, base, alice)[aliceID].Route

	// bob cannot open one on alice's worker
	if r := do(t, "POST", base+"/api/agents/sessions?worker="+aliceID, bob, strings.NewReader(`{"model":"sonnet"}`)); r.StatusCode != 403 {
		t.Fatalf("bob opens: %d", r.StatusCode)
	}
	// the worker takes a cwd only inside its agents folder (or agents.json's folders)
	for _, cwd := range []string{t.TempDir(), am.Root} {
		r := do(t, "POST", base+"/api/agents/sessions", alice, strings.NewReader(`{"model":"opus","cwd":"`+cwd+`"}`))
		r.Body.Close()
		if r.StatusCode != 400 {
			t.Fatalf("cwd %s: %d", cwd, r.StatusCode)
		}
	}
	r := do(t, "POST", base+"/api/agents/sessions", alice, strings.NewReader(`{"model":"claude-code/opus","owner":"Bob"}`))
	if r.StatusCode != 201 {
		t.Fatalf("open: %d", r.StatusCode)
	}
	var sess struct {
		ID, Model, Driver, State, Owner, Cwd string
		Worker                               map[string]string
	}
	decode(t, r, &sess)
	if !strings.HasPrefix(sess.ID, route+".p") || sess.Driver != "tty" || sess.Model != "opus" || sess.Owner != "Alice" ||
		sess.Worker["id"] != aliceID {
		t.Fatalf("session %+v", sess)
	}
	var list []map[string]any
	decode(t, do(t, "GET", base+"/api/agents/sessions", alice, nil), &list)
	if len(list) != 1 || list[0]["id"] != sess.ID {
		t.Fatalf("alice's list %v", list)
	}
	decode(t, do(t, "GET", base+"/api/agents/sessions", bob, nil), &list)
	if len(list) != 0 {
		t.Fatalf("bob sees %v", list)
	}

	// bob may not attach
	ws := "ws" + strings.TrimPrefix(base, "http") + "/api/agents/sessions/" + sess.ID + "/tty"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, resp, err := websocket.Dial(ctx, ws, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + bob}}}); err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("bob attaches: %v %v", err, resp)
	}
	c, _, err := websocket.Dial(ctx, ws, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + alice}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":90,"rows":20}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte("hello through the tunnel\n")); err != nil {
		t.Fatal(err)
	}
	var seen strings.Builder
	for strings.Count(seen.String(), "hello through the tunnel") < 2 { // the terminal's echo, then cat's
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (seen %q)", err, seen.String())
		}
		if typ == websocket.MessageBinary {
			seen.Write(b)
		}
	}
	decode(t, do(t, "GET", base+"/api/agents/sessions", alice, nil), &list)
	if list[0]["viewers"] != float64(1) {
		t.Fatalf("viewers %v", list[0]["viewers"])
	}

	// stop: bob may not, alice may; the viewer's stream ends
	if r := do(t, "DELETE", base+"/api/agents/sessions/"+sess.ID, bob, nil); r.StatusCode != 403 {
		t.Fatalf("bob stops: %d", r.StatusCode)
	}
	if r := do(t, "DELETE", base+"/api/agents/sessions/"+sess.ID, alice, nil); r.StatusCode != 204 {
		t.Fatalf("alice stops: %d", r.StatusCode)
	}
	for {
		if _, _, err := c.Read(ctx); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Fatalf("closed with %v", err)
			}
			break
		}
	}
	decode(t, do(t, "GET", base+"/api/agents/sessions", alice, nil), &list)
	if len(list) != 0 {
		t.Fatalf("after stop %v", list)
	}
}

// startClaudeChatMac is a worker with a Claude Code model run by fakeChat (it keeps the requests it ran).
func startClaudeChatMac(t *testing.T, base, token string) (*fakeChat, func()) {
	t.Helper()
	cat := &engine.Catalog{Models: []*engine.Model{
		{ID: "claude-code/sonnet", Name: "Claude Code · Sonnet", Kind: engine.Chat, Backend: "fakechat", Here: true},
	}}
	dir := t.TempDir()
	chat := &fakeChat{}
	mgr := jobs.New(cat, []engine.Backend{chat}, dir)
	api := (&web.Server{Jobs: mgr, Prompts: prompts.Open(dir), Version: "test"}).Handler()
	stop, _ := connectWorkerWith(t, base, token, api)
	t.Cleanup(func() {
		stop()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !mgr.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	return chat, stop
}

// A client token acts for the person who made it: its claude-code jobs (native and OpenAI API) run only on that
// person's workers, with them as the owner.
func TestClaudeClientOwner(t *testing.T) {
	dir := t.TempDir()
	s, srv := newHub(t, dir)
	base := srv.URL
	alice := signInAs(t, base, "Alice's laptop", "Alice")
	bob := signInAs(t, base, "Bob's phone", "Bob")
	carol := signInAs(t, base, "Carol's phone", "Carol")
	aliceID, aliceTok := enroll(t, base, alice, "Alice's Mac")
	bobID, bobTok := enroll(t, base, bob, "Bob's Mac")

	aliceCl, aliceApp := makeClient(t, base, alice, "alice-app")
	_, carolApp := makeClient(t, base, carol, "carol-app")
	if aliceCl.Owner != "Alice" {
		t.Fatalf("client owner %+v", aliceCl)
	}
	var listed []Client
	decode(t, do(t, "GET", base+"/api/clients", alice, nil), &listed)
	if len(listed) != 2 || listed[0].Owner != "Alice" || listed[1].Owner != "Carol" {
		t.Fatalf("GET /api/clients %+v", listed)
	}
	// a token from before owners (clients.json without "owner")
	oldApp, _, err := s.clients.add("old-app", "")
	if err != nil {
		t.Fatal(err)
	}

	chatBody := `{"model":"claude-code/sonnet","messages":[{"role":"user","content":"hi"}]}`
	// alice's worker is offline: 503 in both APIs (never bob's worker)
	if code, b := post(t, base+"/v1/chat/completions", aliceApp, chatBody); code != 503 {
		t.Fatalf("offline v1: %d %s", code, b)
	}
	aliceChat, stopAlice := startClaudeChatMac(t, base, aliceTok)
	bobChat, _ := startClaudeChatMac(t, base, bobTok)
	var ws map[string]workerItem
	waitFor(t, "both known", func() bool {
		ws = workerItems(t, base, alice)
		return ws[aliceID].Specs != nil && ws[bobID].Specs != nil
	})
	aliceRoute := ws[aliceID].Route

	// /v1/models lists the claude-code models like any other chat model
	var models struct {
		Data []struct {
			ID, Kind string
		}
	}
	decode(t, do(t, "GET", base+"/v1/models", aliceApp, nil), &models)
	found := false
	for _, m := range models.Data {
		found = found || m.ID == "claude-code/sonnet" && m.Kind == "chat"
	}
	if !found {
		t.Fatalf("/v1/models %+v", models)
	}

	for i := 0; i < 3; i++ {
		// native jobs API: the owner comes from the token, a caller's is dropped
		code, j, e := submitJob(t, base, aliceApp, `{"model":"claude-code/sonnet","messages":[{"role":"user","content":"hi"}],"owner":"Bob"}`)
		if code != 200 || !strings.HasPrefix(j.ID, aliceRoute+".") || j.Request["owner"] != "Alice" || j.Request["client"] != "alice-app" {
			t.Fatalf("native %d: %d %+v %v", i, code, j, e)
		}
		// OpenAI API
		code, b := post(t, base+"/v1/chat/completions", aliceApp, chatBody)
		if code != 200 || !strings.Contains(string(b), "Hello!") || !strings.Contains(string(b), `"id":"`+aliceRoute+".") {
			t.Fatalf("v1 %d: %d %s", i, code, b)
		}
		if got := aliceChat.last(); got.Owner != "Alice" || got.Client != "alice-app" {
			t.Fatalf("v1 job %d: owner %q client %q", i, got.Owner, got.Client)
		}
	}
	bobChat.mu.Lock()
	if len(bobChat.got) != 0 {
		t.Fatalf("bob's worker ran %d of alice's jobs", len(bobChat.got))
	}
	bobChat.mu.Unlock()
	// streamed, too
	if code, b := post(t, base+"/v1/chat/completions", aliceApp, `{"model":"claude-code/sonnet","stream":true,"messages":[{"role":"user","content":"hi"}]}`); code != 200 || !strings.Contains(string(b), "[DONE]") || strings.Contains(string(b), `"error"`) {
		t.Fatalf("stream: %d %s", code, b)
	}

	// a person without a worker: 409; a token without an owner: 403 (both APIs)
	if code, b := post(t, base+"/v1/chat/completions", carolApp, chatBody); code != 409 {
		t.Fatalf("carol v1: %d %s", code, b)
	} else if _, c := openAIError(t, b); c != "no_worker" {
		t.Fatalf("carol v1 code %q", c)
	}
	if code, _, e := submitJob(t, base, carolApp, chatBody); code != 409 || e["error"] != "no-worker" {
		t.Fatalf("carol native: %d %v", code, e)
	}
	if code, b := post(t, base+"/v1/chat/completions", oldApp, chatBody); code != 403 {
		t.Fatalf("ownerless v1: %d %s", code, b)
	}
	if code, _, e := submitJob(t, base, oldApp, chatBody); code != 403 || e["error"] != "forbidden" {
		t.Fatalf("ownerless native: %d %v", code, e)
	}

	// alice's worker goes away: 503, not bob's
	stopAlice()
	waitFor(t, "alice's worker gone", func() bool { return !workerItems(t, base, alice)[aliceID].Connected })
	if code, b := post(t, base+"/v1/chat/completions", aliceApp, chatBody); code != 503 {
		t.Fatalf("alice offline v1: %d %s", code, b)
	}
	if code, _, e := submitJob(t, base, aliceApp, chatBody); code != 503 {
		t.Fatalf("alice offline native: %d %v", code, e)
	}
}
