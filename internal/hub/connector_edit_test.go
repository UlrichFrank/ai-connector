package hub

import (
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/philipparndt/ai-connector/internal/jobs"
)

// Editing a connector never cancels a running chat: the agents go to the open connection ({type:"agents"}, no new
// connection); a new name, address or token gets a new connection by a handover while the chat finishes on the old
// one; a token vboard refuses stops the connector with that error once the chat is answered.
func TestConnectorEditKeepsChats(t *testing.T) {
	fastBridges(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	mac := startVboardMac(t, base, wtok, "chat")
	v := newFakeVboard(t, stageAgents)
	c := addConnector(t, base, device, `{"name":"Board AI","url":"`+v.srv.URL+`","token":"vbp_secret","agents":[{"model":"qwen3.6"}]}`)
	b := v.next(t)
	waitState(t, base, device, c.ID, "connected")

	running := func() bool {
		for _, j := range mac.mgr.Jobs() {
			if j.State == jobs.Running {
				return true
			}
		}
		return false
	}
	// a chat that runs while the connector is edited
	startChat := func(b *fakeBridge) (chan struct{}, chan vbChatEv) {
		t.Helper()
		hold := make(chan struct{})
		mac.chat.setHold(hold)
		_, ch := b.chat(`[{"role":"user","content":"hi"}]`, "")
		waitFor(t, "the job running", running)
		return hold, ch
	}
	finish := func(hold chan struct{}, ch chan vbChatEv) {
		t.Helper()
		close(hold)
		mac.chat.setHold(nil)
		if evs := until(t, ch); evs[len(evs)-1].Type != "done" || evs[len(evs)-1].Content != "Hello!" {
			t.Fatalf("the chat %+v", evs)
		}
	}
	noNewConnection := func() {
		t.Helper()
		select {
		case nb := <-v.conns:
			t.Fatalf("a new connection %+v", nb.hello)
		case <-time.After(5 * bridgeCapsEvery):
		}
	}

	// 1. the agents: on the open connection
	hold, ch := startChat(b)
	if code, out := patchConnector(t, base, device, c.ID, `{"agents":[{"model":"qwen3.6"},{"id":"second","model":"qwen3.6","name":"Second"}]}`); code != 200 {
		t.Fatalf("patch agents %d %v", code, out)
	}
	select {
	case as := <-b.updates:
		if len(as) != 2 || as[1].ID != "second" || as[1].Name != "Second" {
			t.Fatalf("agents %+v", as)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no {type:agents}")
	}
	noNewConnection()
	if got := connectorOf(t, base, device, c.ID); got.State != "connected" || len(got.Advertised) != 2 {
		t.Fatalf("after the agents edit %+v", got)
	}
	finish(hold, ch)

	// 2. the name (vboard learns it from hello): a handover, the chat ends on the old connection
	hold, ch = startChat(b)
	if code, _ := patchConnector(t, base, device, c.ID, `{"name":"Team AI"}`); code != 200 {
		t.Fatalf("patch name %d", code)
	}
	b2 := v.next(t)
	if b2.hello.Name != "Team AI" || b2.hello.Instance != b.hello.Instance || len(b2.agents) != 2 {
		t.Fatalf("hello after the rename %+v %+v", b2.hello, b2.agents)
	}
	time.Sleep(3 * bridgeRetireSettle)
	select {
	case <-b.ended:
		t.Fatal("the old connection closed with a chat running on it")
	default:
	}
	finish(hold, ch)
	select {
	case <-b.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the old connection stays open")
	}
	if b.status != websocket.StatusNormalClosure {
		t.Fatalf("old closed with %v", b.status)
	}

	// 3. the address: a new vboard, the chat ends on the old one
	v2 := newFakeVboard(t, stageAgents)
	hold, ch = startChat(b2)
	if code, _ := patchConnector(t, base, device, c.ID, `{"url":"`+v2.srv.URL+`"}`); code != 200 {
		t.Fatalf("patch url %d", code)
	}
	b3 := v2.next(t)
	if b3.hello.Name != "Team AI" {
		t.Fatalf("hello at the new address %+v", b3.hello)
	}
	finish(hold, ch)
	<-b2.ended
	if got := connectorOf(t, base, device, c.ID); got.State != "connected" {
		t.Fatalf("after the move %+v", got)
	}

	// 4. a token vboard refuses: the chat is answered, then the connector stops with the error
	hold, ch = startChat(b3)
	if code, _ := patchConnector(t, base, device, c.ID, `{"token":"vbp_wrong"}`); code != 200 {
		t.Fatalf("patch token %d", code)
	}
	time.Sleep(3 * bridgeRetireSettle)
	if got := connectorOf(t, base, device, c.ID); got.State != "connected" && got.State != "error" {
		t.Fatalf("while the chat runs %+v", got)
	}
	finish(hold, ch)
	<-b3.ended
	if got := waitState(t, base, device, c.ID, "error"); got.Error == "" {
		t.Fatalf("error state %+v", got)
	}
	// fixing it dials again
	if code, _ := patchConnector(t, base, device, c.ID, `{"token":"vbp_secret"}`); code != 200 {
		t.Fatalf("patch token back %d", code)
	}
	v2.next(t)
	waitState(t, base, device, c.ID, "connected")
}
