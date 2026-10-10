package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/web"
)

// A fake vboard: the bridge endpoint of each protocol stage, as the vboard code of that commit reads and answers
// (backend/internal/api/bridge.go, chat.go). Nothing of a real vboard is needed.

type vbStage int

const (
	stageClassify vbStage = iota // d508560: hello {type,name,device,model}; classify → answers | error
	stageEmbed                   // 0992b68: + hello.embed; embed → vectors
	stageChat                    // f914d44: + hello.chat; chat/chatCancel → chatDelta (text, thinking) / chatDone
	stageHead                    // 175647c → main: + hello.instance/handover, {reconnect}, chatDelta.status
	stageAgents                  // ai-agents (e5a7292c): + hello.agents, {type:"agents"}, chat.agent/context
	stageImages                  // images in chats: + hello.images, agents[].images, chat messages[].images
)

func (s vbStage) String() string {
	return [...]string{"d508560 classify", "0992b68 embed", "f914d44 chat", "main handover", "ai-agents", "images"}[s]
}

// vbAgent is an agent entry as the ai-agents stage keeps it (api/bridge.go: id required, trimmed, at most 80
// characters, unique; unknown kind → model; at most 20).
type vbAgent struct {
	ID, Name, Kind, Worker string
	Images                 bool // images stage only
}

func vbAgents(raw []json.RawMessage, stage vbStage) []vbAgent {
	var out []vbAgent
	seen := map[string]bool{}
	for _, r := range raw {
		var a vbAgent
		if json.Unmarshal(r, &a) != nil {
			continue
		}
		a.ID = trimTo(strings.TrimSpace(a.ID), 80)
		if a.ID == "" || seen[a.ID] || len(out) == 20 {
			continue
		}
		seen[a.ID] = true
		if a.Kind != "agent" {
			a.Kind = "model"
		}
		if a.Name == "" {
			a.Name = a.ID
		}
		a.Name, a.Worker = trimTo(a.Name, 80), trimTo(a.Worker, 80)
		if stage < stageImages {
			a.Images = false
		}
		out = append(out, a)
	}
	return out
}

// vbHello is a hello as the stage's server kept it (fields it does not know stay empty), trimmed as it trims.
type vbHello struct {
	Name, Device, Model, Embed, Chat, Instance string
	Handover                                   bool
	Images                                     bool // images stage only
}

type vbResult struct {
	Answers []struct {
		Choice           string             `json:"choice"`
		Probabilities    map[string]float64 `json:"probabilities"`
		AnswerConfidence float64            `json:"answer_confidence"`
	}
	Vectors [][]float32
	Error   string
}

// vbChatEv is a chat event as the stage's chatMessage turns the bridge's messages into: delta, thinking, status (HEAD
// only), done, error.
type vbChatEv struct {
	Type, Text, Content, Error string
	ToolCalls                  []bridgeToolCall
}

type fakeVboard struct {
	t     *testing.T
	stage vbStage
	srv   *httptest.Server
	token string
	user  string
	board string // the one board there is ("" any)

	mu      sync.Mutex
	refuse  []int // answered to the next upgrade attempts instead of the WebSocket
	meCalls int
	dials   int
	conns   chan *fakeBridge
}

type fakeBridge struct {
	v       *fakeVboard
	c       *websocket.Conn
	hello   vbHello
	raw     []byte // the hello as sent
	board   string
	pending sync.Map // id → chan vbResult
	chats   sync.Map // id → chan vbChatEv
	ended   chan struct{}
	status  websocket.StatusCode // how it was closed (after ended)
	rawErr  string               // the last chat error as sent (before vboard cuts it)
	agents  []vbAgent            // ai-agents: hello.agents
	updates chan []vbAgent       // ai-agents: {type:"agents"} as they come
	setups  chan []byte          // setup: {setup}, {models}, {downloadStatus}, … (vboard_setup_test.go)
	n       int
	nmu     sync.Mutex
}

func newFakeVboard(t *testing.T, stage vbStage) *fakeVboard {
	v := &fakeVboard{t: t, stage: stage, token: "vbp_secret", user: "Anna", conns: make(chan *fakeBridge, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.meCalls++
		v.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+v.token {
			writeJSON(w, 401, map[string]string{"error": "Sign in first"})
			return
		}
		writeJSON(w, 200, map[string]any{"user": map[string]string{"name": v.user, "id": "u_1"}})
	})
	mux.HandleFunc("GET /api/v1/classifier/bridge", v.bridge)
	v.srv = httptest.NewServer(mux)
	t.Cleanup(v.srv.Close)
	return v
}

func trimTo(s string, n int) string { return trimRunes(s, n) }

func (v *fakeVboard) bridge(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	v.dials++
	var refuse int
	if len(v.refuse) > 0 {
		refuse, v.refuse = v.refuse[0], v.refuse[1:]
	}
	v.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+v.token {
		writeJSON(w, 401, map[string]string{"error": "Sign in first"})
		return
	}
	if refuse != 0 {
		writeJSON(w, refuse, map[string]string{"error": "server.draining"})
		return
	}
	board := r.URL.Query().Get("board")
	if board != "" && v.board != "" && board != v.board {
		writeJSON(w, 404, map[string]string{"error": "board.notFound"})
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c.SetReadLimit(8 << 20)
	defer c.CloseNow()
	ctx := r.Context()
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	_, raw, err := c.Read(hctx)
	cancel()
	b := &fakeBridge{v: v, c: c, raw: raw, board: board, ended: make(chan struct{})}
	// each stage reads the hello into its own struct: what it does not know is dropped
	var typ string
	switch v.stage {
	case stageClassify:
		var h struct{ Type, Name, Device, Model string }
		err = errOr(err, json.Unmarshal(raw, &h))
		typ, b.hello = h.Type, vbHello{Name: trimTo(h.Name, 80), Device: trimTo(h.Device, 20), Model: trimTo(h.Model, 60)}
	case stageEmbed:
		var h struct{ Type, Name, Device, Model, Embed string }
		err = errOr(err, json.Unmarshal(raw, &h))
		typ, b.hello = h.Type, vbHello{Name: trimTo(h.Name, 80), Device: trimTo(h.Device, 20), Model: trimTo(h.Model, 60), Embed: trimTo(h.Embed, 80)}
	case stageChat:
		var h struct{ Type, Name, Device, Model, Embed, Chat string }
		err = errOr(err, json.Unmarshal(raw, &h))
		typ, b.hello = h.Type, vbHello{Name: trimTo(h.Name, 80), Device: trimTo(h.Device, 20), Model: trimTo(h.Model, 60), Embed: trimTo(h.Embed, 80), Chat: trimTo(h.Chat, 80)}
	default:
		var h struct {
			Type, Name, Device, Model, Embed, Chat, Instance string
			Handover, Images                                 bool
			Agents                                           []json.RawMessage
		}
		err = errOr(err, json.Unmarshal(raw, &h))
		typ, b.hello = h.Type, vbHello{Name: trimTo(h.Name, 80), Device: trimTo(h.Device, 20), Model: trimTo(h.Model, 60), Embed: trimTo(h.Embed, 80),
			Chat: trimTo(h.Chat, 80), Instance: trimTo(h.Instance, 64), Handover: h.Handover}
		if v.stage >= stageAgents {
			b.agents = vbAgents(h.Agents, v.stage)
		}
		if v.stage >= stageImages {
			b.hello.Images = h.Images
		}
	}
	b.updates = make(chan []vbAgent, 16)
	b.setups = make(chan []byte, 256)
	if err != nil || typ != "hello" {
		c.Close(websocket.StatusPolicyViolation, "hello expected")
		return
	}
	welcome, _ := json.Marshal(map[string]any{"type": "welcome", "id": "br_1", "board": board, "user": v.user})
	if err := c.Write(ctx, websocket.MessageText, welcome); err != nil {
		return
	}
	v.conns <- b
	// vboard pings every 30 s: here often, so a bridge that stops reading is noticed
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				err := c.Ping(pctx)
				cancel()
				if err != nil {
					c.CloseNow()
					return
				}
			}
		}
	}()
	defer close(b.ended)
	for {
		_, raw, err := c.Read(ctx)
		if err != nil {
			b.status = websocket.CloseStatus(err)
			b.failChats("the local AI went away")
			return
		}
		var m struct {
			Type    string            `json:"type"`
			ID      string            `json:"id"`
			Answers []json.RawMessage `json:"answers"`
			Vectors [][]float32       `json:"vectors"`
			Message string            `json:"message"`
			Agents  []json.RawMessage `json:"agents"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if v.stage >= stageHead && setupAnswer(m.Type, m.ID, b) {
			b.setups <- raw
			continue
		}
		if v.stage >= stageAgents && m.Type == "agents" {
			b.updates <- vbAgents(m.Agents, v.stage)
			continue
		}
		if v.stage >= stageChat && b.chatMessage(m.Type, m.ID, raw) {
			continue
		}
		ch, ok := b.pending.Load(m.ID)
		if !ok {
			continue
		}
		var res vbResult
		switch {
		case m.Type == "error":
			res.Error = trimTo(m.Message, 300)
		case v.stage >= stageEmbed && m.Type == "vectors":
			res.Vectors = m.Vectors
		default: // d508560 takes anything else for answers
			for _, a := range m.Answers {
				var x struct {
					Choice           string             `json:"choice"`
					Probabilities    map[string]float64 `json:"probabilities"`
					AnswerConfidence float64            `json:"answer_confidence"`
				}
				_ = json.Unmarshal(a, &x)
				res.Answers = append(res.Answers, x)
			}
		}
		select {
		case ch.(chan vbResult) <- res:
		default:
		}
	}
}

func errOr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// chatMessage is chat.go's (f914d44: no status; HEAD: status).
func (b *fakeBridge) chatMessage(typ, id string, raw []byte) bool {
	switch typ {
	case "chatDelta", "chatDone":
	case "error":
		if _, ok := b.chats.Load(id); !ok {
			return false
		}
	default:
		return false
	}
	v, ok := b.chats.Load(id)
	if !ok {
		return true
	}
	var m struct {
		Text      string           `json:"text"`
		Thinking  bool             `json:"thinking"`
		Status    string           `json:"status"`
		Content   string           `json:"content"`
		ToolCalls []bridgeToolCall `json:"toolCalls"`
		Message   string           `json:"message"`
	}
	_ = json.Unmarshal(raw, &m)
	ev := vbChatEv{Type: "delta", Text: m.Text}
	switch {
	case typ == "error":
		b.nmu.Lock()
		b.rawErr = m.Message
		b.nmu.Unlock()
		ev = vbChatEv{Type: "error", Error: trimTo(m.Message, 300)}
		b.chats.Delete(id)
	case typ == "chatDone":
		ev = vbChatEv{Type: "done", Content: m.Content, ToolCalls: m.ToolCalls}
		b.chats.Delete(id)
	case m.Thinking:
		ev = vbChatEv{Type: "thinking"}
	case b.v.stage >= stageHead && m.Status != "":
		ev = vbChatEv{Type: "status", Text: trimTo(m.Status, 300)}
	}
	v.(chan vbChatEv) <- ev
	return true
}

func (b *fakeBridge) failChats(msg string) {
	b.chats.Range(func(k, v any) bool {
		b.chats.Delete(k)
		v.(chan vbChatEv) <- vbChatEv{Type: "error", Error: msg}
		return true
	})
}

func (b *fakeBridge) id() string {
	b.nmu.Lock()
	defer b.nmu.Unlock()
	b.n++
	return "q" + strconv.Itoa(b.n)
}

func (b *fakeBridge) write(v any) {
	msg, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.c.Write(ctx, websocket.MessageText, msg); err != nil {
		b.v.t.Errorf("write to the bridge: %v", err)
	}
}

// ask sends a request (a JSON object written as it is: its key order counts) and waits for the result.
func (b *fakeBridge) ask(t *testing.T, typ, body string) vbResult {
	t.Helper()
	id := b.id()
	ch := make(chan vbResult, 1)
	b.pending.Store(id, ch)
	defer b.pending.Delete(id)
	msg := `{"type":"` + typ + `","id":"` + id + `",` + body + `}`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("no answer to %s", msg)
	}
	return vbResult{}
}

// chat starts a model round; its events arrive on the channel.
func (b *fakeBridge) chat(messages, tools string) (string, chan vbChatEv) {
	return b.chatWith(messages, tools, "")
}

// chatWith is a chat with more fields (ai-agents: `"agent":"…","context":{…}`).
func (b *fakeBridge) chatWith(messages, tools, extra string) (string, chan vbChatEv) {
	id := b.id()
	ch := make(chan vbChatEv, 256)
	b.chats.Store(id, ch)
	msg := `{"type":"chat","id":"` + id + `","messages":` + messages
	if tools != "" {
		msg += `,"tools":` + tools
	}
	if extra != "" {
		msg += "," + extra
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.c.Write(ctx, websocket.MessageText, []byte(msg+"}")); err != nil {
		b.v.t.Fatal(err)
	}
	return id, ch
}

// until reads a chat's events up to its end (done or error).
func until(t *testing.T, ch chan vbChatEv) []vbChatEv {
	t.Helper()
	var evs []vbChatEv
	for {
		select {
		case ev := <-ch:
			evs = append(evs, ev)
			if ev.Type == "done" || ev.Type == "error" {
				return evs
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the chat did not end: %+v", evs)
		}
	}
}

func (v *fakeVboard) next(t *testing.T) *fakeBridge {
	t.Helper()
	select {
	case b := <-v.conns:
		return b
	case <-time.After(10 * time.Second):
		t.Fatal("the connector did not connect")
	}
	return nil
}

func (v *fakeVboard) count() (me, dials int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.meCalls, v.dials
}

// ---------- the worker side ----------

// vbChat thinks, then answers "Hello!"; with tools it calls the first one. While hold is set, it waits for it.
type vbChat struct {
	mu   sync.Mutex
	got  []engine.Request
	hold chan struct{}
	fail string // the error a job ends with
}

func (b *vbChat) Name() string   { return "vbchat" }
func (b *vbChat) Heavy() bool    { return false }
func (b *vbChat) Loaded() string { return "" }
func (b *vbChat) Unload()        {}
func (b *vbChat) Run(ctx context.Context, _ *engine.Model, r engine.Request, _ string, emit engine.Emit) error {
	b.mu.Lock()
	b.got = append(b.got, r)
	hold, fail := b.hold, b.fail
	b.mu.Unlock()
	if fail != "" {
		return errors.New(fail)
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, p := range []string{"<think>", "Let me ", "think.</th", "ink>\n\nHel", "lo!"} {
		emit(engine.Event{Type: "text", Text: p})
	}
	if len(r.Tools) > 0 {
		emit(engine.Event{Type: "result", Data: map[string]any{"toolCalls": []map[string]any{
			{"id": "call_1", "name": r.Tools[0].Name, "arguments": `{"city":"Berlin"}`}}}})
	}
	return nil
}

func (b *vbChat) setHold(h chan struct{}) {
	b.mu.Lock()
	b.hold = h
	b.mu.Unlock()
}

func (b *vbChat) last() engine.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.got[len(b.got)-1]
}

// vbText embeds (every text to [3, 4], normalised by the worker) and classifies (the last option for every state),
// and keeps the states it was asked.
type vbText struct {
	mu    sync.Mutex
	asked []string
}

func (b *vbText) Name() string   { return "vbtext" }
func (b *vbText) Heavy() bool    { return false }
func (b *vbText) Loaded() string { return "" }
func (b *vbText) Unload()        {}
func (b *vbText) Run(context.Context, *engine.Model, engine.Request, string, engine.Emit) error {
	return nil
}
func (b *vbText) Embed(_ context.Context, _ *engine.Model, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{3, 4}
	}
	return out, nil
}
func (b *vbText) Classify(_ context.Context, _ *engine.Model, states []string, instructions string, opts []engine.Option) ([]engine.Answer, error) {
	var labels []string
	for _, o := range opts {
		labels = append(labels, o.Label+"="+o.Description)
	}
	last := opts[len(opts)-1].Label
	var out []engine.Answer
	b.mu.Lock()
	for _, s := range states {
		b.asked = append(b.asked, s+" | "+instructions+" | "+strings.Join(labels, ","))
		out = append(out, engine.Answer{Choice: last, Probabilities: map[string]float64{last: 0.75, opts[0].Label: 0.25}, Confidence: 0.75})
	}
	b.mu.Unlock()
	return out, nil
}

type vbMac struct {
	mgr  *jobs.Manager
	chat *vbChat
	text *vbText
	stop func()
}

// startVboardMac connects a worker with the models of the kinds given (chat, embed, classify).
func startVboardMac(t *testing.T, base, token string, kinds ...string) *vbMac {
	t.Helper()
	all := map[string]*engine.Model{
		"chat":     {ID: "qwen3.6-35b-a3b", Name: "Qwen3.6", Kind: engine.Chat, Backend: "vbchat", Size: gib, Here: true},
		"embed":    {ID: "qwen3-embedding-0.6b", Name: "Embedder", Kind: engine.Embed, Backend: "vbtext", Size: gib, Here: true},
		"classify": {ID: "laya-typed-decisions", Name: "Laya", Kind: engine.Classify, Backend: "vbtext", Size: gib, Here: true},
		"vision":   {ID: "lmstudio/qwen3.6-27b", Name: "Qwen3.6 27B · lmstudio", Kind: engine.Chat, Backend: "vbchat", Size: gib, Here: true, Images: true},
	}
	cat := &engine.Catalog{}
	for _, k := range kinds {
		cat.Models = append(cat.Models, all[k])
	}
	m := &vbMac{chat: &vbChat{}, text: &vbText{}}
	dir := t.TempDir()
	m.mgr = jobs.New(cat, []engine.Backend{m.chat, m.text}, dir)
	api := (&web.Server{Jobs: m.mgr, Prompts: prompts.Open(dir), Version: "test"}).Handler()
	stop, _ := connectWorkerWith(t, base, token, withRAM(api, 64))
	m.stop = func() {
		stop()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !m.mgr.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	}
	t.Cleanup(m.stop)
	return m
}

// ---------- helpers ----------

// fastBridges makes the connector's timings short for a test.
func fastBridges(t *testing.T) {
	t.Helper()
	b1, b2, b3, b4, poll := bridgeCapsEvery, bridgeRetireSettle, bridgeKeepAlive, bridgeBackoffMin, chatPoll
	bridgeCapsEvery, bridgeRetireSettle, bridgeKeepAlive, bridgeBackoffMin, chatPoll = 100*time.Millisecond, 200*time.Millisecond, time.Hour, 10*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() {
		bridgeCapsEvery, bridgeRetireSettle, bridgeKeepAlive, bridgeBackoffMin, chatPoll = b1, b2, b3, b4, poll
	})
}

// vbHub is a server for the connector tests: signed in, with the connectors stopped at the end.
func vbHub(t *testing.T) (*Server, string, string) {
	t.Helper()
	s, srv := newHub(t, t.TempDir())
	t.Cleanup(s.Close)
	return s, srv.URL, signIn(t, srv.URL, "laptop")
}

func addConnector(t *testing.T, base, device, body string) Connector {
	t.Helper()
	c, b := post(t, base+"/api/connectors", device, body)
	if c != 201 {
		t.Fatalf("add connector: %d %s", c, b)
	}
	var out Connector
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func connectorOf(t *testing.T, base, device, id string) Connector {
	t.Helper()
	var list []Connector
	decode(t, do(t, "GET", base+"/api/connectors", device, nil), &list)
	for _, c := range list {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no connector %s", id)
	return Connector{}
}

func waitState(t *testing.T, base, device, id, state string) Connector {
	t.Helper()
	var c Connector
	waitFor(t, "connector "+state, func() bool {
		c = connectorOf(t, base, device, id)
		return c.State == state
	})
	return c
}

// ---------- tests ----------

// Every protocol stage: the hello it reads, classify (order, Python JSON, one answer per state), embed, chat (thinking,
// text, tool calls, the job's client), errors.
func TestVboardStages(t *testing.T) {
	for _, stage := range []vbStage{stageClassify, stageEmbed, stageChat, stageHead, stageAgents, stageImages} {
		t.Run(stage.String(), func(t *testing.T) {
			fastBridges(t)
			_, base, device := vbHub(t)
			_, wtok := enroll(t, base, device, "Mac")
			mac := startVboardMac(t, base, wtok, "chat", "embed", "classify")
			v := newFakeVboard(t, stage)
			c := addConnector(t, base, device, `{"kind":"vboard","url":"`+v.srv.URL+`/","token":"vbp_secret","board":"b_1"}`)
			b := v.next(t)

			// hello: what this stage keeps of it
			want := vbHello{Name: "ai-connector", Device: "ai-connector", Model: "laya-typed-decisions"}
			if stage >= stageEmbed {
				want.Embed = "qwen3-embedding-0.6b"
			}
			if stage >= stageChat {
				want.Chat = "qwen3.6-35b-a3b"
			}
			if stage >= stageHead {
				want.Instance, want.Handover = b.hello.Instance, true
				if !strings.HasPrefix(b.hello.Instance, "aic_") || len(b.hello.Instance) > 64 {
					t.Fatalf("instance %q", b.hello.Instance)
				}
			}
			if b.hello != want || b.board != "b_1" {
				t.Fatalf("hello %+v (board %q), sent %s", b.hello, b.board, b.raw)
			}
			// ai-agents: the default list, the chat model alone (older stages ignore the field)
			wantAgents := []vbAgent(nil)
			if stage >= stageAgents {
				wantAgents = []vbAgent{{ID: "default", Name: "Qwen3.6", Kind: "model", Worker: "Mac"}}
			}
			if !reflect.DeepEqual(b.agents, wantAgents) {
				t.Fatalf("agents %+v, sent %s", b.agents, b.raw)
			}
			got := waitState(t, base, device, c.ID, "connected")
			if got.User != "Anna" || got.Since == "" || got.Error != "" || len(got.Advertised) != 1 || got.Advertised[0].Model != "qwen3.6-35b-a3b" {
				t.Fatalf("connected %+v", got)
			}

			// classify: the states as Python's json.dumps, the options in their order, one answer per state
			res := b.ask(t, "classify", `"states":[{"title":"Grüße","n":1.0},{"b":"x","a":"y"}],"instructions":"Where?",
				"options":{"todo":"open","done":null,"doing":"started"}`)
			if res.Error != "" || len(res.Answers) != 2 || res.Answers[0].Choice != "doing" || res.Answers[1].AnswerConfidence != 0.75 ||
				res.Answers[0].Probabilities["todo"] != 0.25 {
				t.Fatalf("classify %+v", res)
			}
			mac.text.mu.Lock()
			asked := strings.Join(mac.text.asked, "\n")
			mac.text.mu.Unlock()
			if asked != `{"title": "Grüße", "n": 1.0} | Where? | todo=open,done=,doing=started`+"\n"+
				`{"b": "x", "a": "y"} | Where? | todo=open,done=,doing=started` {
				t.Fatalf("asked %q", asked)
			}
			if res := b.ask(t, "classify", `"states":[{"a":"b"}],"instructions":"x","options":{}`); res.Error == "" {
				t.Fatalf("no options: %+v", res)
			}

			if stage >= stageEmbed {
				res := b.ask(t, "embed", `"texts":["a","b","c"]`)
				if res.Error != "" || len(res.Vectors) != 3 || res.Vectors[2][0] != 0.6 || res.Vectors[2][1] != 0.8 {
					t.Fatalf("embed %+v", res)
				}
			}

			if stage >= stageChat {
				_, ch := b.chat(`[{"role":"system","content":"You are on a board."},{"role":"user","content":"time?"},
					{"role":"assistant","content":"","toolCalls":[{"id":"c0","name":"get_time","arguments":""}]},
					{"role":"tool","content":"12:00","toolCallId":"c0"},{"role":"user","content":"weather?"}]`,
					`[{"name":"get_weather","description":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]`)
				evs := until(t, ch)
				var text strings.Builder
				thinking := 0
				for _, ev := range evs {
					switch ev.Type {
					case "delta":
						text.WriteString(ev.Text)
					case "thinking":
						thinking++
					case "status":
						if stage < stageHead {
							t.Fatalf("a status event at %s", stage)
						}
					}
				}
				done := evs[len(evs)-1]
				if thinking != 1 || text.String() != "Hello!" || done.Type != "done" || done.Content != "Hello!" ||
					len(done.ToolCalls) != 1 || done.ToolCalls[0] != (bridgeToolCall{"call_1", "get_weather", `{"city":"Berlin"}`}) {
					t.Fatalf("chat %+v", evs)
				}
				r := mac.chat.last()
				if r.Client != "vboard: ai-connector" || r.Think || len(r.Tools) != 1 || len(r.Messages) != 5 ||
					r.Messages[2].ToolCalls[0].Arguments != "{}" || r.Messages[3].ToolCallID != "c0" || r.Messages[0].Role != "system" {
					t.Fatalf("job request %+v", r)
				}
				// without tools: no calls, an empty list
				_, ch = b.chat(`[{"role":"user","content":"hi"}]`, "")
				evs = until(t, ch)
				if done := evs[len(evs)-1]; done.Type != "done" || done.ToolCalls == nil || len(done.ToolCalls) != 0 {
					t.Fatalf("chat without tools %+v", evs)
				}
				// a bad request is an error
				_, ch = b.chat(`[{"role":"wizard","content":"hi"}]`, "")
				if evs := until(t, ch); evs[0].Type != "error" || !strings.Contains(evs[0].Error, "wizard") {
					t.Fatalf("bad chat %+v", evs)
				}
			}
		})
	}
}

// The raw chatDelta messages: thinking once, status (sent to every stage: f914d44 takes it for an empty text) while
// the job waits, keep-alives while it runs quietly, and a cancel stops the job without an answer.
func TestVboardChatStatusAndCancel(t *testing.T) {
	fastBridges(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	mac := startVboardMac(t, base, wtok, "chat")
	for _, stage := range []vbStage{stageChat, stageHead, stageAgents, stageImages} {
		v := newFakeVboard(t, stage)
		c := addConnector(t, base, device, `{"name":"Board AI","url":"`+v.srv.URL+`","token":"vbp_secret"}`)
		b := v.next(t)
		if b.hello.Model != "" || b.hello.Embed != "" || b.hello.Chat != "qwen3.6-35b-a3b" {
			t.Fatalf("chat-only hello %+v", b.hello)
		}
		hold := make(chan struct{})
		mac.chat.setHold(hold)
		id, ch := b.chat(`[{"role":"user","content":"hi"}]`, "")
		var statuses []string
		waitFor(t, "a status", func() bool {
			for {
				select {
				case ev := <-ch:
					if ev.Type == "status" {
						statuses = append(statuses, ev.Text)
					} else if ev.Type != "delta" || ev.Text != "" {
						t.Fatalf("before the answer: %+v", ev)
					}
				default:
					return stage < stageHead || len(statuses) > 0
				}
			}
		})
		if stage == stageHead && !strings.HasPrefix(statuses[0], "Running on Mac") && !strings.HasPrefix(statuses[0], "Waiting for Mac") {
			t.Fatalf("statuses %q", statuses)
		}
		close(hold)
		mac.chat.setHold(nil)
		if evs := until(t, ch); evs[len(evs)-1].Content != "Hello!" {
			t.Fatalf("chat %+v", evs)
		}
		_ = id

		// cancel: the job ends on the worker, vboard hears nothing more of it
		hold = make(chan struct{})
		mac.chat.setHold(hold)
		id, ch = b.chat(`[{"role":"user","content":"long"}]`, "")
		waitFor(t, "the job running", func() bool {
			for _, j := range mac.mgr.Jobs() {
				if j.State == jobs.Running {
					return true
				}
			}
			return false
		})
		b.write(map[string]string{"type": "chatCancel", "id": id})
		waitFor(t, "the job canceled", func() bool {
			for _, j := range mac.mgr.Jobs() {
				if j.State == jobs.Running || j.State == jobs.Queued {
					return false
				}
			}
			return true
		})
		close(hold)
		mac.chat.setHold(nil)
		time.Sleep(300 * time.Millisecond)
		for len(ch) > 0 {
			if ev := <-ch; ev.Type == "done" || ev.Type == "error" || ev.Type == "delta" && ev.Text != "" { // (its status lines came before)
				t.Fatalf("after the cancel: %+v", ev)
			}
		}
		do(t, "DELETE", base+"/api/connectors/"+c.ID, device, nil).Body.Close()
		<-b.ended
	}
}

// What the connector cannot do now is an error (an old vboard sends classify to any bridge), and errors stay short.
func TestVboardErrors(t *testing.T) {
	fastBridges(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	startVboardMac(t, base, wtok, "embed")
	v := newFakeVboard(t, stageClassify)
	addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret","models":{"classify":"nope"}}`)
	b := v.next(t)
	if b.hello.Model != "" {
		t.Fatalf("hello %+v", b.hello)
	}
	res := b.ask(t, "classify", `"states":[{"a":"b"}],"instructions":"x","options":{"a":"b"}`)
	if !strings.Contains(res.Error, "cannot classify") || len([]rune(res.Error)) > 300 {
		t.Fatalf("classify without a model: %+v", res)
	}
	res = b.ask(t, "something", `"x":1`)
	if !strings.Contains(res.Error, "unknown request") {
		t.Fatalf("unknown: %+v", res)
	}
	res = b.ask(t, "embed", `"texts":["x"]`) // d508560 takes the vectors for answers: an embed never comes from it
	if res.Error != "" {
		t.Fatalf("embed %+v", res)
	}

	// a failed chat job: its error, cut to 300 characters
	_, w2 := enroll(t, base, device, "Mac 2")
	mac := startVboardMac(t, base, w2, "chat")
	mac.chat.mu.Lock()
	mac.chat.fail = "out of memory: " + strings.Repeat("ä", 400)
	mac.chat.mu.Unlock()
	v2 := newFakeVboard(t, stageHead)
	addConnector(t, base, device, `{"url":"`+v2.srv.URL+`","token":"vbp_secret"}`)
	b2 := v2.next(t)
	_, ch := b2.chat(`[{"role":"user","content":"hi"}]`, "")
	evs := until(t, ch)
	b2.nmu.Lock()
	raw := b2.rawErr
	b2.nmu.Unlock()
	if last := evs[len(evs)-1]; last.Type != "error" || !strings.HasPrefix(last.Error, "out of memory: ää") || len([]rune(raw)) != 300 {
		t.Fatalf("failed chat %+v (%d characters sent)", evs, len([]rune(raw)))
	}
}

// 401, 403 and 404 stop the connector (until it is edited); 503 and network errors are retried with a doubling pause.
func TestVboardRefusedAndRetried(t *testing.T) {
	fastBridges(t)
	s, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	startVboardMac(t, base, wtok, "embed")
	var mu sync.Mutex
	var pauses []time.Duration
	s.connectors.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		pauses = append(pauses, d)
		mu.Unlock()
		return ctx.Err()
	}

	// a wrong token: /me says 401, nothing is dialled
	v := newFakeVboard(t, stageHead)
	c := addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_wrong"}`)
	got := waitState(t, base, device, c.ID, "error")
	if me, dials := v.count(); !strings.Contains(got.Error, "HTTP 401") || me != 1 || dials != 0 || len(pauses) != 0 {
		t.Fatalf("401: %+v me %d dials %d pauses %v", got, me, dials, pauses)
	}
	// editing it (the right token) tries again
	if c, b := patchConnector(t, base, device, c.ID, `{"token":"vbp_secret"}`); c != 200 {
		t.Fatalf("patch %d %v", c, b)
	}
	v.next(t)
	waitState(t, base, device, c.ID, "connected")
	do(t, "DELETE", base+"/api/connectors/"+c.ID, device, nil).Body.Close()

	// a board the token cannot edit: 404, final
	v2 := newFakeVboard(t, stageHead)
	v2.board = "b_mine"
	c = addConnector(t, base, device, `{"url":"`+v2.srv.URL+`","token":"vbp_secret","board":"b_other"}`)
	got = waitState(t, base, device, c.ID, "error")
	if _, dials := v2.count(); !strings.Contains(got.Error, "b_other") || dials != 1 || len(pauses) != 0 {
		t.Fatalf("404: %+v dials %d", got, dials)
	}
	do(t, "DELETE", base+"/api/connectors/"+c.ID, device, nil).Body.Close()

	// 403 (a read-only or another board's token): final
	v3 := newFakeVboard(t, stageHead)
	v3.refuse = []int{403}
	c = addConnector(t, base, device, `{"url":"`+v3.srv.URL+`","token":"vbp_secret"}`)
	if got = waitState(t, base, device, c.ID, "error"); !strings.Contains(got.Error, "HTTP 403") {
		t.Fatalf("403: %+v", got)
	}
	do(t, "DELETE", base+"/api/connectors/"+c.ID, device, nil).Body.Close()

	// 503 (vboard drains), a 502 and a 500: again after 10 ms, 20 ms, 40 ms (here; 1 s, 2 s, 4 s … 60 s for real)
	v4 := newFakeVboard(t, stageHead)
	v4.refuse = []int{503, 502, 500}
	c = addConnector(t, base, device, `{"url":"`+v4.srv.URL+`","token":"vbp_secret"}`)
	b4 := v4.next(t)
	waitState(t, base, device, c.ID, "connected")
	mu.Lock()
	if len(pauses) != 3 || pauses[0] != 10*time.Millisecond || pauses[1] != 20*time.Millisecond || pauses[2] != 40*time.Millisecond {
		t.Fatalf("pauses %v", pauses)
	}
	pauses = nil
	mu.Unlock()
	// a lost connection: dialled again after the shortest pause (it worked for a while)
	b4.c.CloseNow()
	v4.next(t)
	mu.Lock()
	if len(pauses) != 1 || pauses[0] != 10*time.Millisecond {
		t.Fatalf("pauses after a lost connection %v", pauses)
	}
	pauses = nil
	mu.Unlock()
	waitState(t, base, device, c.ID, "connected")
	do(t, "DELETE", base+"/api/connectors/"+c.ID, device, nil).Body.Close()

	// the pause doubles up to the maximum
	defer func(d time.Duration) { bridgeBackoffMax = d }(bridgeBackoffMax)
	bridgeBackoffMax = 30 * time.Millisecond
	v5 := newFakeVboard(t, stageHead)
	v5.refuse = []int{503, 503, 503, 503}
	c = addConnector(t, base, device, `{"url":"`+v5.srv.URL+`","token":"vbp_secret"}`)
	v5.next(t)
	mu.Lock()
	if len(pauses) != 4 || pauses[3] != 30*time.Millisecond {
		t.Fatalf("capped pauses %v", pauses)
	}
	mu.Unlock()
	waitState(t, base, device, c.ID, "connected")
}

// {reconnect}: a new connection with the same instance; new requests are answered there, the chat running on the old
// one finishes there, then the old one is closed normally.
func TestVboardHandover(t *testing.T) {
	fastBridges(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	mac := startVboardMac(t, base, wtok, "chat", "embed")
	v := newFakeVboard(t, stageHead)
	c := addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret"}`)
	old := v.next(t)
	since := waitState(t, base, device, c.ID, "connected").Since

	hold := make(chan struct{})
	mac.chat.setHold(hold)
	_, ch := old.chat(`[{"role":"user","content":"hi"}]`, "")
	waitFor(t, "the job running", func() bool {
		js := mac.mgr.Jobs()
		return len(js) == 1 && js[0].State == jobs.Running
	})
	old.write(map[string]string{"type": "reconnect", "reason": "server shutting down"})
	nu := v.next(t)
	if nu.hello.Instance != old.hello.Instance || !nu.hello.Handover {
		t.Fatalf("instances %q %q", old.hello.Instance, nu.hello.Instance)
	}
	if res := nu.ask(t, "embed", `"texts":["x"]`); len(res.Vectors) != 1 {
		t.Fatalf("embed on the new connection %+v", res)
	}
	time.Sleep(3 * bridgeRetireSettle)
	select {
	case <-old.ended:
		t.Fatal("the old connection closed with a chat running on it")
	default:
	}
	close(hold)
	mac.chat.setHold(nil)
	if evs := until(t, ch); evs[len(evs)-1].Type != "done" || evs[len(evs)-1].Content != "Hello!" {
		t.Fatalf("the chat on the old connection %+v", evs)
	}
	select {
	case <-old.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the old connection stays open")
	}
	if old.status != websocket.StatusNormalClosure {
		t.Fatalf("old closed with %v", old.status)
	}
	if got := connectorOf(t, base, device, c.ID); got.State != "connected" || got.Since != since {
		t.Fatalf("after the handover %+v (since %s)", got, since)
	}
	if res := nu.ask(t, "embed", `"texts":["x"]`); len(res.Vectors) != 1 {
		t.Fatalf("embed after %+v", res)
	}
}

// What the workers offer changes: a new hello (vboard learns it only from hello), the old connection closed; nothing
// offered: disconnected until a worker comes.
func TestVboardCapabilities(t *testing.T) {
	fastBridges(t)
	_, base, device := vbHub(t)
	_, w1 := enroll(t, base, device, "Mac 1")
	_, w2 := enroll(t, base, device, "Mac 2")
	v := newFakeVboard(t, stageClassify) // the oldest: no instance, the new connection is just newer
	c := addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret"}`)
	got := waitState(t, base, device, c.ID, "connecting")
	waitFor(t, "the wait explained", func() bool {
		got = connectorOf(t, base, device, c.ID)
		return strings.Contains(got.Error, "No connected worker offers")
	})
	if _, dials := v.count(); dials != 0 {
		t.Fatalf("dialled with nothing to offer")
	}

	startVboardMac(t, base, w1, "embed")
	b1 := v.next(t)
	if b1.hello.Model != "" {
		t.Fatalf("hello 1 %+v %s", b1.hello, b1.raw)
	}
	mac2 := startVboardMac(t, base, w2, "classify", "chat")
	b2 := v.next(t)
	if b2.hello.Model != "laya-typed-decisions" || !strings.Contains(string(b2.raw), `"chat":"qwen3.6-35b-a3b"`) {
		t.Fatalf("hello 2 %+v %s", b2.hello, b2.raw)
	}
	select {
	case <-b1.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the first connection stays open")
	}
	if res := b2.ask(t, "classify", `"states":[{"a":"b"}],"instructions":"x","options":{"a":"b","c":"d"}`); len(res.Answers) != 1 {
		t.Fatalf("classify %+v", res)
	}
	mac2.stop()
	b3 := v.next(t)
	if b3.hello.Model != "" || strings.Contains(string(b3.raw), `"chat":"qwen`) {
		t.Fatalf("hello 3 %s", b3.raw)
	}
}

// The connector API: made with the device's user as owner, the token never shown, edited, disabled, deleted, the
// list streamed.
func TestConnectorAPI(t *testing.T) {
	fastBridges(t)
	s, base, device := vbHub(t)
	_, client := makeClient(t, base, device, "docscanner")
	v := newFakeVboard(t, stageHead)
	_, wtok := enroll(t, base, device, "Mac")
	startVboardMac(t, base, wtok, "embed")

	for _, x := range []struct{ body, field string }{
		{`{"url":"ftp://x","token":"vbp_secret"}`, "url"},
		{`{"url":"","token":"vbp_secret"}`, "url"},
		{`{"url":"https://v.example","token":""}`, "token"},
		{`{"url":"https://v.example","token":"vbp_ x"}`, "token"},
		{`{"kind":"slack","url":"https://v.example","token":"vbp_secret"}`, "kind"},
		{`{"url":"https://v.example","token":"vbp_secret","board":"a/b"}`, "board"},
	} {
		c, b := post(t, base+"/api/connectors", device, x.body)
		var e map[string]string
		json.Unmarshal(b, &e)
		if c != 400 || e["error"] != x.field || e["message"] == "" {
			t.Errorf("%s: %d %s", x.body, c, b)
		}
	}
	for _, tok := range []string{"", client} { // signed in devices only
		if c, _ := post(t, base+"/api/connectors", tok, `{"url":"https://v.example","token":"vbp_secret"}`); c != 401 {
			t.Fatalf("token %q: %d", tok, c)
		}
		if r := do(t, "GET", base+"/api/connectors", tok, nil); r.StatusCode != 401 {
			t.Fatalf("list with %q: %d", tok, r.StatusCode)
		}
	}

	// the stream: the list at once, and again on every change
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/connectors/events", nil)
	req.Header.Set("Authorization", "Bearer "+device)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("events %v", err)
	}
	defer resp.Body.Close()
	events := make(chan string, 64)
	go func() {
		buf := make([]byte, 1<<16)
		var acc string
		for {
			n, err := resp.Body.Read(buf)
			acc += string(buf[:n])
			for {
				i := strings.Index(acc, "\n\n")
				if i < 0 {
					break
				}
				if d, ok := strings.CutPrefix(acc[:i], "data: "); ok {
					events <- d
				}
				acc = acc[i+2:]
			}
			if err != nil {
				close(events)
				return
			}
		}
	}()
	nextEvent := func(cond func([]Connector) bool) []Connector {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case d := <-events:
				if strings.Contains(d, "vbp_") {
					t.Fatalf("a token in the stream: %s", d)
				}
				var list []Connector
				if err := json.Unmarshal([]byte(d), &list); err != nil {
					t.Fatalf("event %s", d)
				}
				if cond(list) {
					return list
				}
			case <-deadline:
				t.Fatal("no such event")
			}
		}
	}
	nextEvent(func(l []Connector) bool { return l != nil && len(l) == 0 })

	c := addConnector(t, base, device, `{"kind":"vboard","name":"  ","url":" `+v.srv.URL+`/ ","token":" vbp_secret ","models":{"embed":" qwen3-embedding "}}`)
	if c.Name != "ai-connector" || c.Kind != "vboard" || c.URL != v.srv.URL || !c.Enabled || c.Owner != "Philipp" || c.Created.IsZero() ||
		c.Models != (ConnectorModels{Embed: "qwen3-embedding"}) || (c.State != "connecting" && c.State != "connected") {
		t.Fatalf("made %+v", c)
	}
	nextEvent(func(l []Connector) bool { return len(l) == 1 && l[0].State == "connected" && l[0].User == "Anna" })
	b := v.next(t)
	if b.hello.Embed != "qwen3-embedding-0.6b" || b.hello.Name != "ai-connector" {
		t.Fatalf("hello %+v", b.hello)
	}
	// never the token
	for _, path := range []string{"/api/connectors"} {
		r := do(t, "GET", base+path, device, nil)
		var raw json.RawMessage
		decode(t, r, &raw)
		if strings.Contains(string(raw), "vbp_") || strings.Contains(string(raw), `"token"`) {
			t.Fatalf("GET %s: %s", path, raw)
		}
	}
	st, err := os.Stat(filepath.Join(s.cfg.Data, "connectors.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("connectors.json %v %v", st, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(s.cfg.Data, "connectors.json")); !strings.Contains(string(raw), `"token": "vbp_secret"`) {
		t.Fatalf("stored %s", raw)
	}

	// PATCH: the name (a new hello with it), a blank token keeps the stored one, one model of three
	code, out := patchConnector(t, base, device, c.ID, `{"name":"Team board","token":"","models":{"chat":"qwen3.6"}}`)
	if code != 200 || out["name"] != "Team board" || out["token"] != nil || strings.Contains(fmtJSON(out), "vbp_") ||
		out["models"].(map[string]any)["embed"] != "qwen3-embedding" || out["models"].(map[string]any)["chat"] != "qwen3.6" {
		t.Fatalf("patch %d %v", code, out)
	}
	b2 := v.next(t)
	if b2.hello.Name != "Team board" {
		t.Fatalf("hello after the rename %+v", b2.hello)
	}
	<-b.ended
	if code, _ := patchConnector(t, base, device, c.ID, `{"url":"mailto:x"}`); code != 400 {
		t.Fatalf("bad url patch %d", code)
	}
	if code, _ := patchConnector(t, base, device, "nope", `{"name":"x"}`); code != 404 {
		t.Fatalf("unknown id %d", code)
	}
	// disabled: disconnected
	code, out = patchConnector(t, base, device, c.ID, `{"enabled":false}`)
	if code != 200 || out["enabled"] != false || out["state"] != "disabled" {
		t.Fatalf("disable %d %v", code, out)
	}
	<-b2.ended
	nextEvent(func(l []Connector) bool { return len(l) == 1 && !l[0].Enabled && l[0].State == "disabled" })

	// it stays across a restart (disabled), with its owner
	s.Close()
	s2, srv2 := newHub(t, s.cfg.Data)
	t.Cleanup(s2.Close)
	device2 := signIn(t, srv2.URL, "laptop")
	if got := connectorOf(t, srv2.URL, device2, c.ID); got.Name != "Team board" || got.Owner != "Philipp" || got.Enabled || got.State != "disabled" {
		t.Fatalf("after a restart %+v", got)
	}

	// deleted
	if r := do(t, "DELETE", base+"/api/connectors/"+c.ID, device, nil); r.StatusCode != 204 {
		t.Fatalf("delete %d", r.StatusCode)
	}
	if r := do(t, "DELETE", base+"/api/connectors/"+c.ID, device, nil); r.StatusCode != 404 {
		t.Fatalf("delete again %d", r.StatusCode)
	}
	nextEvent(func(l []Connector) bool { return len(l) == 0 })
}

func patchConnector(t *testing.T, base, device, id, body string) (int, map[string]any) {
	t.Helper()
	r := do(t, "PATCH", base+"/api/connectors/"+id, device, strings.NewReader(body))
	var out map[string]any
	decode(t, r, &out)
	return r.StatusCode, out
}

func fmtJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A connector chats with Claude Code only when chosen, and then on its owner's worker, with them as the job's owner.
func TestVboardClaudeOwner(t *testing.T) {
	fastBridges(t)
	s, srv := newHub(t, t.TempDir())
	t.Cleanup(s.Close)
	base := srv.URL
	alice := signInAs(t, base, "Alice's laptop", "Alice")
	bob := signInAs(t, base, "Bob's phone", "Bob")
	carol := signInAs(t, base, "Carol's phone", "Carol")
	aliceID, aliceTok := enroll(t, base, alice, "Alice's Mac")
	bobID, bobTok := enroll(t, base, bob, "Bob's Mac")
	_, qwenTok := enroll(t, base, bob, "Bob's PC")
	aliceChat, _ := startClaudeChatMac(t, base, aliceTok)
	bobChat, _ := startClaudeChatMac(t, base, bobTok)
	startVboardMac(t, base, qwenTok, "chat")
	waitFor(t, "the workers known", func() bool {
		ws := workerItems(t, base, alice)
		return ws[aliceID].Specs != nil && ws[bobID].Specs != nil
	})

	// the default chat model is never Claude Code (it would spend someone's subscription unasked)
	v := newFakeVboard(t, stageHead)
	addConnector(t, base, bob, `{"name":"Default","url":"`+v.srv.URL+`","token":"vbp_secret"}`)
	if b := v.next(t); b.hello.Chat != "qwen3.6-35b-a3b" {
		t.Fatalf("default chat %q", b.hello.Chat)
	}

	// alice's connector with Claude Code: her worker, her job
	v = newFakeVboard(t, stageHead)
	c := addConnector(t, base, alice, `{"name":"Alice AI","url":"`+v.srv.URL+`","token":"vbp_secret","models":{"chat":"claude-code/sonnet"}}`)
	if c.Owner != "Alice" {
		t.Fatalf("owner %q", c.Owner)
	}
	b := v.next(t)
	if b.hello.Chat != "claude-code/sonnet" {
		t.Fatalf("hello %+v", b.hello)
	}
	for i := 0; i < 3; i++ {
		_, ch := b.chat(`[{"role":"user","content":"hi"}]`, "")
		evs := until(t, ch)
		if last := evs[len(evs)-1]; last.Type != "done" || last.Content != "Hello!" {
			t.Fatalf("chat %d: %+v", i, evs)
		}
		if got := aliceChat.last(); got.Owner != "Alice" || got.Client != "vboard: Alice AI" {
			t.Fatalf("chat %d job: owner %q client %q", i, got.Owner, got.Client)
		}
	}
	bobChat.mu.Lock()
	if len(bobChat.got) != 0 {
		t.Fatalf("bob's worker ran %d of alice's connector's chats", len(bobChat.got))
	}
	bobChat.mu.Unlock()

	// carol has no worker: Claude Code is not offered for her connector (it would only fail), so with nothing else to
	// offer it does not dial
	v = newFakeVboard(t, stageHead)
	cc := addConnector(t, base, carol, `{"name":"Carol AI","url":"`+v.srv.URL+`","token":"vbp_secret","models":{"chat":"claude-code/sonnet"}}`)
	waitFor(t, "carol's connector waits", func() bool {
		return strings.Contains(connectorOf(t, base, carol, cc.ID).Error, "No connected worker offers")
	})
	if _, dials := v.count(); dials != 0 {
		t.Fatalf("carol's connector dialled")
	}

	// a plain chat model of a connector carries no owner
	v = newFakeVboard(t, stageHead)
	addConnector(t, base, alice, `{"name":"Qwen AI","url":"`+v.srv.URL+`","token":"vbp_secret","models":{"chat":"qwen3.6-35b-a3b"}}`)
	b = v.next(t)
	_, ch := b.chat(`[{"role":"user","content":"hi"}]`, "")
	if evs := until(t, ch); evs[len(evs)-1].Type != "done" {
		t.Fatalf("qwen: %+v", evs)
	}
}
