package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coder/websocket"

	"github.com/philipparndt/ai-connector/internal/pyjson"
)

// The vboard connector: the server is vboard's "local AI", as vboard-ai is on a person's Mac. It dials
//
//	GET wss://<vboard>/api/v1/classifier/bridge?board=<id>&name=<name>   Authorization: Bearer <vboard token>
//
// says {hello} (what it can do: the models the connected workers offer now), gets {welcome}, and then answers
// vboard's requests through the workers: {classify} → {answers} and {embed} → {vectors} on a worker's direct path,
// {chat} → {chatDelta}… {chatDone} as a chat job in a worker's queue (with vboard's tools; the calls go back to vboard,
// which runs them). vboard is not changed for this: the protocol is vboard-ai's (vboard tools/vboard-ai/bridge.go,
// backend/internal/api/bridge.go, chat.go, drain.go), and what is sent here is understood by every vboard since the
// bridge exists — the classify-only bridge of d508560, embeddings (0992b68), chat (f914d44), handover and chat status
// (175647c). Fields an older vboard does not know are ignored by it (hello.embed/chat/instance/handover; a status-only
// chatDelta is an empty text delta there, not shown, and restarts its idle timer).
//
// Handover (make-before-break): on {"type":"reconnect"} — a vboard replica shuts down — and whenever what the workers
// offer changes (vboard learns that only from hello), a second connection with the same hello.instance is opened;
// new requests are answered there, the old one is closed normally once what runs on it is answered.

// Tunables (tests make them short).
var (
	bridgeBackoffMin   = time.Second      // the first pause after a failed connection; doubled up to bridgeBackoffMax
	bridgeBackoffMax   = 60 * time.Second //
	bridgeCapsEvery    = 30 * time.Second // what the workers offer is checked this often (and on every fleet change)
	bridgeRetireSettle = 2 * time.Second  // an old connection stays open at least this long after a handover
	bridgeKeepAlive    = 60 * time.Second // a chat says something at least this often (vboard gives up after 3 min)
	bridgeEmbedBatch   = 256              // texts per call to a worker
	bridgeDialTimeout  = 30 * time.Second
)

const (
	bridgeDevice     = "ai-connector" // hello.device
	bridgeMaxMessage = 8<<20 - 64<<10 // vboard reads at most 8 MB a message
	bridgeErrorChars = 300            // vboard keeps 300 characters of an error
	bridgeDirectMax  = 4096           // states or texts per direct call (the worker's limit)
	bridgeInbound    = 32 << 20       // what vboard may send (a chat's history)
	bridgePath       = "/api/v1/classifier/bridge"
)

// bridgeCaps is what the connector answers with now: the models (catalogue ids; "" = it cannot do that now) and the
// agents it offers (the first is the default; chat is its model, for vboards before agents).
type bridgeCaps struct {
	classify, embed, chat string
	agents                []AdvertisedAgent
}

func (c bridgeCaps) empty() bool {
	return c.classify == "" && c.embed == "" && c.chat == "" && len(c.agents) == 0
}

// sameHello: the hello's top-level fields are the same (vboards before agents learn only these, and only from hello).
func (c bridgeCaps) sameHello(o bridgeCaps) bool {
	return c.classify == o.classify && c.embed == o.embed && c.chat == o.chat && c.images() == o.images()
}

// images: the default (the first agent, a model) takes images (hello.images: for a vboard that does not read the
// agents' own flag).
func (c bridgeCaps) images() bool {
	return len(c.agents) > 0 && c.agents[0].Kind == kindModel && c.agents[0].Images
}

func (c bridgeCaps) String() string {
	var parts []string
	for _, p := range []struct{ k, v string }{{"classify", c.classify}, {"embed", c.embed}, {"chat", c.chat}, {"agents", agentIDs(c.agents)}} {
		if p.v != "" {
			parts = append(parts, p.k+" "+p.v)
		}
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

func agentIDs(as []AdvertisedAgent) string {
	ids := make([]string, len(as))
	for i, a := range as {
		ids[i] = a.ID
	}
	return strings.Join(ids, " ")
}

// wireAgents is the agents list as vboard reads it (hello.agents, {type:"agents"}): id, name, kind, worker, and
// images: true when it takes images (absent: it does not).
func wireAgents(as []AdvertisedAgent) []map[string]any {
	out := make([]map[string]any, len(as))
	for i, a := range as {
		out[i] = map[string]any{"id": a.ID, "name": a.Name, "kind": a.Kind}
		if a.Worker != "" {
			out[i]["worker"] = a.Worker
		}
		if a.Images {
			out[i]["images"] = true
		}
	}
	return out
}

// connectorCaps resolves a connector's models and agents against what the connected workers list now: the chosen
// model or the server's default (MLXLAB_CLASSIFY_MODEL, MLXLAB_EMBED_MODEL; chat: the first chat model a worker lists
// that is not Claude Code), and the agents whose model a connected worker offers — a claude-code one only while a
// worker of the connector's owner with Claude Code is connected (it runs there, on their subscription).
func (s *Server) connectorCaps(ctx context.Context, conf storedConnector) bridgeCaps {
	routed, _ := s.fleet.pick(ctx)
	models := fleetModels(routed)
	find := func(name, kind string) string {
		if m, ok := findModel(models, name); ok && m.Kind == kind {
			return m.ID
		}
		return ""
	}
	or := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	c := bridgeCaps{classify: find(or(conf.Models.Classify, s.cfg.ClassifyModel), "classify"), embed: find(or(conf.Models.Embed, s.cfg.EmbedModel), "embed")}
	for _, a := range conf.agentList() {
		if adv, ok := s.advertise(routed, models, conf, a); ok {
			c.agents = append(c.agents, adv)
		}
	}
	// the default must be a model (older vboards and search ask it with tools): when the first chosen one is not
	// offered now, the first model that is takes its place
	if len(c.agents) > 0 && c.agents[0].Kind != kindModel {
		for i, a := range c.agents {
			if a.Kind == kindModel {
				c.agents = append(append([]AdvertisedAgent{a}, c.agents[:i]...), c.agents[i+1:]...)
				break
			}
		}
	}
	if len(c.agents) > 0 && c.agents[0].Kind == kindModel {
		c.chat = c.agents[0].Model
	}
	return c
}

// advertise is an agent as offered now (false: no connected worker can run it).
func (s *Server) advertise(routed []*node, models []fleetModel, conf storedConnector, a ConnectorAgent) (AdvertisedAgent, bool) {
	var fm fleetModel
	ok := false
	if a.Model == "" { // the default chat model
		for _, m := range models {
			// not Claude Code: it uses its owner's subscription, so a connector uses it only when chosen
			if m.Kind == "chat" && m.Backend != "claude-code" && !strings.HasPrefix(m.ID, claudePrefix) {
				fm, ok = m, true
				break
			}
		}
	} else {
		fm, ok = findModel(models, a.Model)
	}
	if !ok || fm.Kind != "chat" {
		return AdvertisedAgent{}, false
	}
	kind := a.Kind
	if kind == "" {
		kind = kindModel
	}
	var n *node
	if fm.Backend == "claude-code" || strings.HasPrefix(fm.ID, claudePrefix) {
		if n, _ = s.claudeNode(conf.Owner, routed, "", viaConnector); n == nil {
			return AdvertisedAgent{}, false
		}
		if len(conf.Agents) > 0 {
			kind = kindAgent // chosen as an agent (a name that resolves to Claude Code, too)
		}
	} else if n, ok = choose(routed, s.fleet.primary(routed), fm.catalogModel, true, nil); !ok {
		return AdvertisedAgent{}, false
	}
	name := a.Name
	if name == "" {
		name = fm.Name
	}
	if name == "" {
		name = fm.ID
	}
	images := fm.Images || fm.Backend == "claude-code" || strings.HasPrefix(fm.ID, claudePrefix)
	return AdvertisedAgent{ID: trimRunes(a.ID, agentChars), Name: trimRunes(name, agentChars), Kind: kind,
		Worker: trimRunes(n.name, agentChars), Model: fm.ID, Images: images}, true
}

// ---------- errors ----------

// bridgeFatal ends a connector until it is edited: retrying cannot help (the token is refused, no such board).
type bridgeFatal struct{ msg string }

func (e *bridgeFatal) Error() string { return e.msg }

func isBridgeFatal(err error) bool {
	var f *bridgeFatal
	return errors.As(err, &f)
}

// errNothingOffered: no connected worker offers any model the connector answers with (it waits for one).
var errNothingOffered = errors.New("no connected worker offers a classify, embed or chat model")

// bridgeRefused explains an HTTP answer instead of the WebSocket (or to /api/v1/me): 401, 403 and 404 are final,
// anything else (503 server.draining while vboard restarts, a proxy's 502, 429) is worth another try.
func bridgeRefused(res *http.Response, board string) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	reason := e.Error
	if reason == "" {
		if r := strings.TrimSpace(string(body)); len(r) <= 120 && !strings.HasPrefix(r, "<") {
			reason = r
		}
	}
	status := fmt.Sprintf("HTTP %d", res.StatusCode)
	if reason != "" {
		status += ": " + reason
	}
	switch res.StatusCode {
	case http.StatusUnauthorized:
		return &bridgeFatal{"vboard does not accept the token (" + status + "): it is invalid, expired or revoked. " +
			"Create a new one in vboard (Set up AI → Create token) and edit the connector."}
	case http.StatusForbidden:
		return &bridgeFatal{"vboard refused the connection (" + status + "): a board token works only on its own board " +
			"(leave the board empty or give that board), and a read-only token cannot connect."}
	case http.StatusNotFound:
		if board != "" {
			return &bridgeFatal{fmt.Sprintf("vboard refused the connection (%s): board %s does not exist or the token cannot edit it.", status, board)}
		}
		return &bridgeFatal{"vboard refused the connection (" + status + "): is this a vboard address, and a vboard with the local AI?"}
	}
	return errors.New(status)
}

// ---------- the runner ----------

// vboardRun is one enabled vboard connector: it connects, serves, reconnects.
type vboardRun struct {
	m        *connectors
	s        *Server
	id       string // the connector's
	gen      int
	instance string // hello.instance: the old and the new connection of a handover share it

	cmu     sync.Mutex
	conf    storedConnector // the settings now (an edit replaces them: reconfigure)
	updated chan struct{}   // an edit came (one pending at most)
}

func newVboardRun(m *connectors, conf storedConnector, gen int) *vboardRun {
	return &vboardRun{m: m, s: m.s, id: conf.ID, conf: conf, gen: gen, instance: "aic_" + randomString(12),
		updated: make(chan struct{}, 1)}
}

// config is the connector's settings now. A connection keeps those it was dialled with (bridgeConn.conf): its
// requests are answered with them.
func (r *vboardRun) config() storedConnector {
	r.cmu.Lock()
	defer r.cmu.Unlock()
	return r.conf
}

// reconfigure takes edited settings without stopping what runs: the open connection offers the new agents and
// models (serve), other settings get a new connection by a handover.
func (r *vboardRun) reconfigure(conf storedConnector) {
	r.cmu.Lock()
	r.conf = conf
	r.cmu.Unlock()
	select {
	case r.updated <- struct{}{}:
	default:
	}
}

// sameLink: the settings a connection is dialled with are the same (vboard's address, the token, the board, and
// the name, which vboard learns only from the URL and hello).
func sameLink(a, b storedConnector) bool {
	return a.URL == b.URL && a.Token == b.Token && a.Board == b.Board && a.Name == b.Name
}

func (r *vboardRun) logf(format string, args ...any) {
	log.Printf("connector %q: "+format, append([]any{r.config().Name}, args...)...)
}

func (r *vboardRun) set(update func(st *connectorStatus)) { r.m.report(r.id, r.gen, update) }

// pause waits between attempts; an edit ends it early (the next attempt uses the new settings). Not nil: ctx ended.
func (r *vboardRun) pause(ctx context.Context, d time.Duration) error {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-r.updated:
			cancel()
		case <-pctx.Done():
		}
	}()
	_ = r.m.sleep(pctx, d)
	return ctx.Err()
}

func (r *vboardRun) connecting(msg string) {
	r.set(func(st *connectorStatus) {
		st.State, st.Error, st.Since, st.Advertised = "connecting", msg, time.Time{}, nil
	})
}

// advertised reports the agents vboard was told of.
func (r *vboardRun) advertised(as []AdvertisedAgent) {
	r.set(func(st *connectorStatus) { st.Advertised = append([]AdvertisedAgent(nil), as...) })
}

// run connects and serves until ctx ends or vboard refuses the connector for good. Lost connections and network
// errors are retried with a growing pause (1 s, 2 s, … 60 s); a handover continues on its new connection at once.
func (r *vboardRun) run(ctx context.Context) {
	backoff := bridgeBackoffMin
	var next *bridgeConn
	for ctx.Err() == nil {
		c, served, err := next, next != nil, error(nil)
		moved := next != nil
		next = nil
		if c == nil {
			caps, ok := r.waitCaps(ctx)
			if !ok {
				return
			}
			var user string
			conf := r.config()
			if user, err = r.me(ctx, conf); err == nil {
				if c, err = r.dial(ctx, conf, caps); err == nil && c.user == "" {
					c.user = user
				}
			}
			served = err == nil
		}
		if err == nil {
			r.set(func(st *connectorStatus) {
				if st.State != "connected" {
					st.Since = time.Now()
				}
				st.State, st.Error, st.User = "connected", "", c.user
				st.Advertised = append([]AdvertisedAgent(nil), c.capsNow().agents...)
			})
			if moved {
				r.logf("moved to a new connection — answers %s", c.capsNow())
			} else {
				r.logf("connected to %s as %s — answers %s", c.conf.URL, c.user, c.capsNow())
			}
			if next, err = r.serve(ctx, c); next != nil {
				backoff = bridgeBackoffMin
				continue
			}
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errNothingOffered) {
			r.logf("%v: disconnected until one does", err)
			continue
		}
		if isBridgeFatal(err) {
			r.logf("stopped: %v", err)
			r.set(func(st *connectorStatus) {
				st.State, st.Error, st.Since, st.Advertised = "error", err.Error(), time.Time{}, nil
			})
			return
		}
		if served {
			backoff = bridgeBackoffMin // it worked for a while: start over with short pauses
		}
		r.logf("connection lost (%v); retrying in %s", err, backoff)
		r.connecting(fmt.Sprintf("%v (retrying in %s)", err, backoff))
		if r.pause(ctx, backoff) != nil {
			return
		}
		backoff = min(backoff*2, bridgeBackoffMax)
	}
}

// waitCaps waits until a connected worker offers one of the connector's models (false: ctx ended).
func (r *vboardRun) waitCaps(ctx context.Context) (bridgeCaps, bool) {
	for {
		changed := r.s.fleet.Changed()
		caps := r.s.connectorCaps(ctx, r.config())
		if ctx.Err() != nil {
			return caps, false
		}
		if !caps.empty() {
			return caps, true
		}
		r.connecting(strings.ToUpper(errNothingOffered.Error()[:1]) + errNothingOffered.Error()[1:] + ": waiting for one")
		select {
		case <-ctx.Done():
			return caps, false
		case <-changed:
		case <-r.updated:
		case <-time.After(bridgeCapsEvery):
		}
	}
}

// me asks vboard whose the token is (GET /api/v1/me, as vboard-ai does before it dials): a refused token stops here.
func (r *vboardRun) me(ctx context.Context, conf storedConnector) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, bridgeDialTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, conf.URL+"/api/v1/me", nil)
	if err != nil {
		return "", &bridgeFatal{fmt.Sprintf("%q is not a vboard address", conf.URL)}
	}
	req.Header.Set("Authorization", "Bearer "+conf.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return "", bridgeRefused(res, "")
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		return "", bridgeRefused(res, "")
	case res.StatusCode != http.StatusOK:
		return "", nil // the bridge's own answer decides
	}
	var me struct {
		User struct{ Name string } `json:"user"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&me)
	return me.User.Name, nil
}

// wsURL is the bridge's address: ws(s)://<vboard>/api/v1/classifier/bridge?board=&name=.
func wsURL(conf storedConnector) (string, error) {
	u, err := url.Parse(conf.URL)
	if err != nil || u.Host == "" {
		return "", &bridgeFatal{fmt.Sprintf("%q is not a vboard address", conf.URL)}
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", &bridgeFatal{fmt.Sprintf("%q: the address starts with https:// (or http://)", conf.URL)}
	}
	u.Path = strings.TrimRight(u.Path, "/") + bridgePath
	q := url.Values{}
	if conf.Board != "" {
		q.Set("board", conf.Board)
	}
	q.Set("name", conf.Name)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// bridgeConn is one WebSocket to vboard.
type bridgeConn struct {
	ws   *websocket.Conn
	user string          // welcome.user
	conf storedConnector // the settings it was dialled with (its requests are answered with them)

	cmu  sync.Mutex
	caps bridgeCaps // what its hello said, with the agents as last sent

	wmu sync.Mutex
}

func (c *bridgeConn) capsNow() bridgeCaps {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return c.caps
}

func (c *bridgeConn) setAgents(as []AdvertisedAgent) {
	c.cmu.Lock()
	c.caps.agents = as
	c.cmu.Unlock()
}

// agent is the agent a request names ("" the default: the first).
func (c *bridgeConn) agent(id string) (AdvertisedAgent, bool) {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	for i, a := range c.caps.agents {
		if id == "" && i == 0 || id != "" && a.ID == id {
			return a, true
		}
	}
	return AdvertisedAgent{}, false
}

// dial opens a connection: the WebSocket, {hello} with what the workers offer, vboard's {welcome}.
func (r *vboardRun) dial(ctx context.Context, conf storedConnector, caps bridgeCaps) (*bridgeConn, error) {
	u, err := wsURL(conf)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, bridgeDialTimeout)
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+conf.Token)
	ws, res, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if res != nil && res.StatusCode != http.StatusSwitchingProtocols {
			return nil, bridgeRefused(res, conf.Board)
		}
		return nil, err
	}
	ws.SetReadLimit(bridgeInbound)
	// the caps are vboard's (bridge.go trimTo); instance + handover: this bridge understands {reconnect}; agents: what
	// a vboard that knows them lets the person pick (older ones ignore the field)
	h0 := map[string]any{"type": "hello", "name": trimRunes(conf.Name, 80), "device": bridgeDevice,
		"model": trimRunes(caps.classify, 60), "embed": trimRunes(caps.embed, 80), "chat": trimRunes(caps.chat, 80),
		"instance": r.instance, "handover": true, "agents": wireAgents(caps.agents)}
	if caps.images() {
		h0["images"] = true // the default model takes images
	}
	hello, _ := json.Marshal(h0)
	if err := ws.Write(ctx, websocket.MessageText, hello); err != nil {
		ws.CloseNow()
		return nil, err
	}
	_, raw, err := ws.Read(ctx)
	if err != nil {
		ws.CloseNow()
		return nil, err
	}
	var welcome struct{ Type, User, Board string }
	if json.Unmarshal(raw, &welcome) != nil || welcome.Type != "welcome" {
		ws.CloseNow()
		return nil, fmt.Errorf("unexpected answer from vboard: %.100s", raw)
	}
	return &bridgeConn{ws: ws, caps: caps, user: welcome.User, conf: conf}, nil
}

// send writes a message (one at a time). An answer vboard could not read (over its 8 MB) becomes an error for its id.
func (c *bridgeConn) send(id string, v any) {
	b, _ := json.Marshal(v)
	if len(b) > bridgeMaxMessage {
		b, _ = json.Marshal(map[string]any{"type": "error", "id": id,
			"message": fmt.Sprintf("the answer is too large for vboard (%.1f MB, at most 8 MB): ask for fewer at once", float64(len(b))/(1<<20))})
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// not a request's context: a write whose context ends closes the connection
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = c.ws.Write(ctx, websocket.MessageText, b) // a failed write ends the read loop too
}

// running counts the requests a connection works on.
type running struct {
	mu sync.Mutex
	n  int
}

func (w *running) add(d int) {
	w.mu.Lock()
	w.n += d
	w.mu.Unlock()
}

func (w *running) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// wait waits until nothing runs (true) or stop delivers (false; nil: never).
func (w *running) wait(stop <-chan error) bool {
	for w.count() > 0 {
		select {
		case <-stop:
			return false
		case <-time.After(20 * time.Millisecond):
		}
	}
	return true
}

// chatRuns are the running chats of a connection ({chatCancel} stops one).
type chatRuns struct {
	mu sync.Mutex
	m  map[string]context.CancelFunc
}

func (c *chatRuns) start(ctx context.Context, id string) context.Context {
	cctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	if old := c.m[id]; old != nil {
		old()
	}
	c.m[id] = cancel
	c.mu.Unlock()
	return cctx
}

func (c *chatRuns) cancel(id string) {
	c.mu.Lock()
	cancel := c.m[id]
	delete(c.m, id)
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *chatRuns) cancelAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, cancel := range c.m {
		cancel()
		delete(c.m, id)
	}
}

// bridgeMsg is a message from vboard.
type bridgeMsg struct {
	Type         string            `json:"type"`
	ID           string            `json:"id"`
	Reason       string            `json:"reason"` // reconnect
	States       []json.RawMessage `json:"states"`
	Instructions string            `json:"instructions"`
	Options      json.RawMessage   `json:"options"`
	Texts        []string          `json:"texts"`
	Messages     []bridgeChatMsg   `json:"messages"`
	Tools        []bridgeTool      `json:"tools"`
	Agent        string            `json:"agent"`   // chat: the agent picked ("" the default)
	Context      *bridgeContext    `json:"context"` // chat of an agent that works on the board itself
}

// bridgeContext is where an agent of kind "agent" works (vboard's chatContext): the board, the person, what they
// selected and see.
type bridgeContext struct {
	BoardID   string   `json:"boardId"`
	BoardName string   `json:"boardName"`
	UserID    string   `json:"userId"`
	UserName  string   `json:"userName"`
	Role      string   `json:"role"`
	Lang      string   `json:"lang"`
	Selection []string `json:"selection"`
	View      *struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		W float64 `json:"w"`
		H float64 `json:"h"`
	} `json:"view"`
	Resume bool `json:"resume"`
	// ConversationID: the panel's id of this chat conversation (stable over its turns; vboards before it send none)
	ConversationID string `json:"conversationId"`
}

type bridgeChatMsg struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []bridgeToolCall `json:"toolCalls"`
	ToolCallID string           `json:"toolCallId"`
	// Images of a user message, named in Content by their tags ("[image-1]"); only sent to agents that advertised
	// images (images.go)
	Images []chatImage `json:"images"`
}

// withImages is the request as the agent gets it: the user messages' images checked (at most maxImages), or — for
// an agent whose model does not take images — their tags replaced by "(image omitted)" and the images left out.
func withImages(m *bridgeMsg, allowed bool) (*bridgeMsg, error) {
	c := *m
	c.Messages = make([]bridgeChatMsg, len(m.Messages))
	n := 0
	for i, x := range m.Messages {
		if len(x.Images) > 0 {
			if !allowed || x.Role != "user" {
				x.Content = omitImageTags(x.Content, x.Images)
				x.Images = nil
			} else {
				x.Images = append([]chatImage(nil), x.Images...)
				for k := range x.Images {
					if err := x.Images[k].check(); err != nil {
						return nil, fmt.Errorf("messages[%d]: %v", i, err)
					}
				}
				if n += len(x.Images); n > maxImages {
					return nil, fmt.Errorf("at most %d images a request", maxImages)
				}
			}
		}
		c.Messages[i] = x
	}
	return &c, nil
}

type bridgeToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type bridgeTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// serve answers the requests of one connection until it ends (nil, why), or until a handover's new connection is
// ready: then it returns that one, and the old connection keeps running in the background only until the requests
// running on it are answered.
func (r *vboardRun) serve(ctx context.Context, c *bridgeConn) (*bridgeConn, error) {
	cctx, cancel := context.WithCancel(ctx) // this connection's work
	work := &running{}
	chats := &chatRuns{m: map[string]context.CancelFunc{}}
	readErr := make(chan error, 1)
	moved := make(chan *bridgeConn, 1)
	refused := make(chan error, 1) // vboard refused the connection with edited settings for good
	var dialing sync.WaitGroup
	var hmu sync.Mutex
	handingOver := false
	handover := func(why string) {
		hmu.Lock()
		defer hmu.Unlock()
		if handingOver {
			return
		}
		handingOver = true
		r.logf("%s: opening a new connection, the old one finishes its work", why)
		dialing.Add(1)
		go func() {
			defer dialing.Done()
			nc, err := r.handoverDial(cctx)
			if nc == nil {
				if isBridgeFatal(err) && !sameLink(r.config(), c.conf) {
					// the edited address, token or board does not work: the connector stops with that error (as it
					// would after a restart), once what runs here is answered
					refused <- err
					return
				}
				hmu.Lock()
				handingOver = false
				hmu.Unlock()
				return
			}
			moved <- nc // one handover at a time: there is room
		}()
	}
	// read in one loop (it also answers vboard's pings) and work in goroutines: a long request must not hold up the
	// pong, or vboard drops the connection
	go func() {
		for {
			_, raw, err := c.ws.Read(cctx)
			if err != nil {
				readErr <- err
				return
			}
			var m bridgeMsg
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			if m.Type == "reconnect" { // no id: the vboard replica shuts down, move to another one
				reason := m.Reason
				if reason == "" {
					reason = "asked by vboard"
				}
				handover("vboard asks to reconnect (" + reason + ")")
				continue
			}
			if m.ID == "" {
				continue
			}
			switch m.Type {
			case "chat":
				rctx := chats.start(cctx, m.ID)
				work.add(1)
				go func() {
					defer work.add(-1)
					defer chats.cancel(m.ID)
					r.chat(rctx, c, &m)
				}()
			case "chatCancel":
				chats.cancel(m.ID)
			default:
				work.add(1)
				go func() {
					defer work.add(-1)
					r.answer(cctx, c, &m)
				}()
			}
		}
	}()
	end := func() {
		cancel()
		chats.cancelAll()
		work.wait(nil)
		c.ws.CloseNow()
	}
	// stop ends this connection here and now (the read loop has ended or ends with it); a handover's connection that
	// was dialled meanwhile is closed.
	stop := func(readDone bool) {
		end()
		if !readDone {
			<-readErr
		}
		dialing.Wait()
		select {
		case nc := <-moved:
			nc.ws.CloseNow()
		default:
		}
	}
	// retire keeps the old connection open until what runs on it is answered (and the settle time passed), then
	// closes it
	retire := func() {
		r.m.wg.Add(1)
		go func() {
			defer r.m.wg.Done()
			t := time.NewTimer(bridgeRetireSettle)
			defer t.Stop()
			select {
			case <-t.C:
				if work.wait(readErr) {
					_ = c.ws.Close(websocket.StatusNormalClosure, "handed over")
					r.logf("closed the old connection (its work is done)")
				}
			case <-readErr:
			}
			end()
		}()
	}
	tick := time.NewTicker(bridgeCapsEvery)
	defer tick.Stop()
	for {
		changed := r.s.fleet.Changed()
		if !sameLink(r.config(), c.conf) {
			// edited: vboard's address, the token, the board or the name (which vboard learns only from the URL and
			// hello) — a new connection with them, the requests running here are answered here
			handover("the connector's settings changed")
		}
		select {
		case err := <-readErr:
			end()
			dialing.Wait() // ended by end(): it either delivered its connection or gave up
			select {
			case next := <-moved: // the old connection ended just as the new one was ready
				return next, nil
			default:
			}
			return nil, err
		case next := <-moved:
			retire()
			return next, nil
		case err := <-refused:
			retire()
			return nil, err
		case <-ctx.Done():
			stop(false)
			return nil, ctx.Err()
		case <-changed:
		case <-r.updated: // agents or models edited: offered below
		case <-tick.C:
		}
		caps := r.s.connectorCaps(cctx, r.config())
		cur := c.capsNow()
		if cctx.Err() != nil || caps.sameHello(cur) && reflect.DeepEqual(caps.agents, cur.agents) {
			continue
		}
		if caps.empty() {
			_ = c.ws.Close(websocket.StatusNormalClosure, "nothing to offer")
			stop(false)
			return nil, errNothingOffered
		}
		if !caps.sameHello(cur) {
			// older vboards learn the models only from hello: a new connection (whose hello has the agents too)
			handover(fmt.Sprintf("the workers offer %s now (was %s)", caps, cur))
			continue
		}
		// only the agents changed: a vboard that knows them takes the new list on this connection (older ones ignore it)
		c.setAgents(caps.agents)
		c.send("", map[string]any{"type": "agents", "agents": wireAgents(caps.agents)})
		r.advertised(caps.agents)
		r.logf("agents now: %s (was %s)", agentIDs(caps.agents), agentIDs(cur.agents))
	}
}

// handoverDial opens the new connection of a handover, with what the workers offer now (retrying: the new vboard
// replica may not be ready yet), with the connector's settings now. nil: it gave up (the old connection ended,
// nothing to offer, or a final refusal: then the error).
func (r *vboardRun) handoverDial(ctx context.Context) (*bridgeConn, error) {
	pause := bridgeBackoffMin
	for {
		conf := r.config()
		caps := r.s.connectorCaps(ctx, conf)
		if ctx.Err() != nil || caps.empty() {
			return nil, nil
		}
		c, err := r.dial(ctx, conf, caps)
		if err == nil {
			if c.user == "" {
				c.user, _ = r.me(ctx, conf)
			}
			return c, nil
		}
		if ctx.Err() != nil {
			return nil, nil
		}
		if isBridgeFatal(err) {
			r.logf("new connection refused (%v); staying on the old one", err)
			return nil, err
		}
		r.logf("new connection failed (%v); retrying in %s", err, pause)
		if r.m.sleep(ctx, pause) != nil {
			return nil, nil
		}
		pause = min(pause*2, 10*time.Second)
	}
}

// ---------- classify, embed ----------

// answer answers a classify or embed request: {answers}, {vectors} or {error}.
func (r *vboardRun) answer(ctx context.Context, c *bridgeConn, m *bridgeMsg) {
	start := time.Now()
	var out map[string]any
	var err error
	switch m.Type {
	case "classify":
		out, err = r.classify(ctx, c.capsNow(), m)
	case "embed":
		out, err = r.embed(ctx, c.capsNow(), m)
	default:
		err = fmt.Errorf("unknown request %q", m.Type)
	}
	if ctx.Err() != nil {
		return // the connection or the connector stopped
	}
	if err != nil {
		msg := trimRunes(err.Error(), bridgeErrorChars)
		r.logf("%s %s failed: %s", m.Type, m.ID, msg)
		c.send(m.ID, map[string]any{"type": "error", "id": m.ID, "message": msg})
		return
	}
	r.logf("%s %s: answered in %d ms", m.Type, m.ID, time.Since(start).Milliseconds())
	c.send(m.ID, out)
}

func apiErr(e *apiError) error {
	if e == nil {
		return nil
	}
	return errors.New(e.message)
}

// classify answers {classify}: one answer per state, the states serialised as Python's json.dumps (vboard-ai's form),
// the options in their order.
func (r *vboardRun) classify(ctx context.Context, caps bridgeCaps, m *bridgeMsg) (map[string]any, error) {
	if caps.classify == "" {
		return nil, errors.New("this connector cannot classify now: no connected worker offers a classify model")
	}
	states := make([]string, len(m.States))
	for i, s := range m.States {
		txt, err := pyjson.Marshal(s)
		if err != nil {
			return nil, fmt.Errorf("state %d: %w", i, err)
		}
		states[i] = txt
	}
	opts, err := bridgeOptions(m.Options)
	if err != nil {
		return nil, err
	}
	answers := make([]map[string]any, 0, len(states))
	if len(states) > 0 {
		n, model, e := r.s.place(ctx, caps.classify, "classify")
		if e != nil {
			return nil, apiErr(e)
		}
		for i := 0; i < len(states); i += bridgeDirectMax {
			part := states[i:min(i+bridgeDirectMax, len(states))]
			var res struct {
				Answers []struct {
					Choice        string             `json:"choice"`
					Probabilities map[string]float64 `json:"probabilities"`
					Confidence    float64            `json:"confidence"`
				} `json:"answers"`
			}
			in := map[string]any{"model": model.ID, "states": part, "instructions": m.Instructions, "options": opts}
			if e := r.s.direct(ctx, n, "/api/direct/classify", in, &res); e != nil {
				return nil, apiErr(e)
			}
			if len(res.Answers) != len(part) {
				return nil, fmt.Errorf("%s: %d answers for %d states", n.name, len(res.Answers), len(part))
			}
			for _, a := range res.Answers {
				answers = append(answers, map[string]any{"choice": a.Choice, "probabilities": a.Probabilities, "answer_confidence": a.Confidence})
			}
		}
	}
	return map[string]any{"type": "answers", "id": m.ID, "answers": answers}, nil
}

// bridgeOptions reads the options object in its order, label → description (as vboard-ai does: null or "" is none,
// structured criteria become compact Python JSON).
func bridgeOptions(raw json.RawMessage) ([]map[string]string, error) {
	labels, vals, err := orderedObject(raw)
	if err != nil {
		return nil, errors.New("options: an object label → description expected")
	}
	if len(labels) == 0 {
		return nil, errors.New("options: at least one option needed")
	}
	out := make([]map[string]string, len(labels))
	for i, l := range labels {
		var v any
		if err := json.Unmarshal(vals[i], &v); err != nil {
			return nil, err
		}
		desc := ""
		switch x := v.(type) {
		case nil:
		case string:
			desc = x
		default:
			b, _ := json.Marshal(x)
			if desc, err = pyjson.Marshal(b); err != nil {
				return nil, err
			}
		}
		out[i] = map[string]string{"label": l, "description": desc}
	}
	return out, nil
}

// embed answers {embed}: one L2-normalised vector per text, asked of the worker in batches.
func (r *vboardRun) embed(ctx context.Context, caps bridgeCaps, m *bridgeMsg) (map[string]any, error) {
	if caps.embed == "" {
		return nil, errors.New("this connector cannot embed now: no connected worker offers an embedding model")
	}
	vectors := make([][]float32, 0, len(m.Texts))
	for i := 0; i < len(m.Texts); i += bridgeEmbedBatch {
		vecs, _, e := r.s.embed(ctx, caps.embed, m.Texts[i:min(i+bridgeEmbedBatch, len(m.Texts))])
		if e != nil {
			return nil, apiErr(e)
		}
		for _, v := range vecs {
			vectors = append(vectors, normalize(v))
		}
	}
	return map[string]any{"type": "vectors", "id": m.ID, "vectors": vectors}, nil
}

// normalize scales v to length 1 (the workers do already; a vector of zeros stays).
func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 || math.Abs(sum-1) < 1e-6 {
		return v
	}
	n := float32(1 / math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * n
	}
	return out
}

// ---------- chat ----------

// bridgeChatJob is the chat job of a {chat}: vboard's messages and tools (the model's calls go back to vboard).
func bridgeChatJob(m *bridgeMsg) (map[string]any, error) {
	if len(m.Messages) == 0 {
		return nil, errors.New("the chat has no messages")
	}
	msgs := make([]map[string]any, len(m.Messages))
	for i, x := range m.Messages {
		switch x.Role {
		case "system", "user", "assistant", "tool":
		default:
			return nil, fmt.Errorf("messages[%d]: unknown role %q", i, x.Role)
		}
		out := map[string]any{"role": x.Role, "content": x.Content}
		if x.Role == "tool" {
			out["toolCallId"] = x.ToolCallID
		}
		if x.Role == "user" && len(x.Images) > 0 {
			out["images"] = jobImages(x.Images)
		}
		if x.Role == "assistant" && len(x.ToolCalls) > 0 {
			calls := make([]map[string]string, len(x.ToolCalls))
			for j, tc := range x.ToolCalls {
				args := tc.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				calls[j] = map[string]string{"id": tc.ID, "name": tc.Name, "arguments": args}
			}
			out["toolCalls"] = calls
		}
		msgs[i] = out
	}
	job := map[string]any{"messages": msgs, "think": false} // as vboard-ai's built-in model: answers come at once
	var tools []map[string]any
	for i, t := range m.Tools {
		if t.Name == "" {
			return nil, fmt.Errorf("tools[%d]: a tool without a name", i)
		}
		tool := map[string]any{"name": t.Name, "description": t.Description}
		if p := strings.TrimSpace(string(t.Parameters)); p != "" && p != "null" {
			tool["parameters"] = t.Parameters
		}
		tools = append(tools, tool)
	}
	if len(tools) > 0 {
		job["tools"] = tools
	}
	return job, nil
}

// chat answers a {chat} with a chat job: {chatDelta, text} as the answer streams, {chatDelta, thinking:true} once when
// the model thinks (the thought itself stays here), {chatDelta, status} while the job waits or its model loads (and,
// for an agent, for its tool calls), then {chatDone, content, toolCalls} — or {error}. Nothing is sent when it is
// canceled ({chatCancel}, the connection or the connector stopped): the job is canceled on the worker.
//
// The request's agent (none: the default, the first) picks the model. A model (kind model) gets vboard's messages and
// tools and returns its calls; an agent (kind agent, or a request with context) works on the board itself through
// vboard's MCP server in a long-running Claude Code session of its own (agentJob) and returns no calls.
func (r *vboardRun) chat(ctx context.Context, c *bridgeConn, m *bridgeMsg) {
	start := time.Now()
	id := m.ID
	last := time.Now()
	send := func(v map[string]any) {
		v["id"] = id
		c.send(id, v)
		last = time.Now()
	}
	fail := func(err string) {
		if ctx.Err() != nil {
			r.logf("chat %s stopped (canceled)", id)
			return
		}
		err = trimRunes(err, bridgeErrorChars)
		r.logf("chat %s failed: %s", id, err)
		send(map[string]any{"type": "error", "message": err})
	}
	a, ok := c.agent(m.Agent)
	switch {
	case !ok && m.Agent == "":
		fail("this connector cannot chat now: no connected worker offers a chat model")
		return
	case !ok:
		fail(fmt.Sprintf("%q is not available now: no connected worker offers it", m.Agent))
		return
	}
	asAgent := a.Kind == kindAgent || m.Context != nil
	m, err := withImages(m, a.Images) // never images to an agent that cannot take them
	if err != nil {
		fail(err.Error())
		return
	}
	var job map[string]any
	if asAgent {
		job, err = agentJob(c.conf, m, r.m.conversations.seen)
		if e := c.conf.effortOf(a.ID); e != "" && err == nil {
			job["effort"] = e
		}
	} else {
		job, err = bridgeChatJob(m)
	}
	if err != nil {
		fail(err.Error())
		return
	}
	// a claude-code model runs on the connector's owner's worker, with them as the job's owner (placeChat)
	run, e := r.s.placeChat(ctx, a.Model, chatFor{c.conf.Owner, viaConnector})
	if e != nil {
		fail(e.message)
		return
	}
	n := run.n
	job["model"] = run.model
	job["client"] = "vboard: " + c.conf.Name
	feed := r.s.fleet.followJob() // before the job exists: none of its events is missed
	defer feed.close()
	var content strings.Builder
	thinking, said, status := false, false, ""
	sp := &thinkSplit{emit: func(thought bool, s string) {
		if thought {
			if !thinking {
				thinking = true
				send(map[string]any{"type": "chatDelta", "thinking": true})
			}
			return
		}
		said = true
		content.WriteString(s)
		send(map[string]any{"type": "chatDelta", "text": s})
	}}
	toolsSeen := map[string]bool{}
	var used []string
	agentSaid := false // an agent's status line stands (a tool call, or one being written): ticks repeat it
	hooks := chatHooks{
		text: sp.write,
		tick: func(state string) {
			quiet := time.Since(last) >= bridgeKeepAlive
			switch {
			case said:
				if quiet { // the answer pauses (a long tool call): an empty text keeps vboard waiting
					send(map[string]any{"type": "chatDelta", "text": ""})
				}
			case thinking:
				if quiet {
					send(map[string]any{"type": "chatDelta", "thinking": true})
				}
			default:
				st := "Running on " + n.name + " (" + run.model + ")"
				if state == "queued" {
					st = "Waiting for " + n.name + " (queued)"
				}
				if agentSaid { // an agent's tool call says more than "running": kept, repeated only to keep vboard waiting
					st = status
				}
				if st != status || quiet {
					status = st
					send(map[string]any{"type": "chatDelta", "status": trimRunes(st, bridgeErrorChars)})
				}
			}
		},
	}
	if asAgent {
		// the agent's tool calls as status lines ("create_items …"), one per call as it starts; before that, while
		// the model writes a large call's input, its size now and then ("writing create_items … 8 KB", at most every
		// 2 s from the worker)
		hooks.event = func(typ, text string, data map[string]any) {
			if typ == "status" && strings.HasPrefix(text, "writing ") {
				line := trimRunes(text, 120)
				status, agentSaid = line, true
				send(map[string]any{"type": "chatDelta", "status": line})
				return
			}
			if typ != "tool" {
				return
			}
			tid, _ := data["id"].(string)
			name, _ := data["name"].(string)
			if tid == "" || name == "" || toolsSeen[tid] {
				return
			}
			toolsSeen[tid] = true
			line := toolStatus(name, data)
			used = append(used, line)
			status, agentSaid = line, true
			send(map[string]any{"type": "chatDelta", "status": line})
		}
	}
	j, e := run.run(ctx, feed, job, hooks)
	if e != nil {
		fail(e.message)
		return
	}
	sp.flush()
	if f := j.failure(); f != nil {
		fail(f.message)
		return
	}
	calls := make([]map[string]string, 0, len(j.Result.ToolCalls))
	names := make([]string, 0, len(j.Result.ToolCalls))
	if !asAgent { // an agent changed the board itself: no calls for vboard to run
		for _, tc := range j.Result.ToolCalls {
			args := tc.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			calls = append(calls, map[string]string{"id": tc.ID, "name": tc.Name, "arguments": args})
			names = append(names, tc.Name)
		}
	}
	line := fmt.Sprintf("chat %s (%s): answered in %.1f s on %s", id, a.ID, time.Since(start).Seconds(), n.name)
	if len(names) > 0 {
		line += " → " + strings.Join(names, ", ")
	}
	if len(used) > 0 {
		line += " after " + strings.Join(used, ", ")
	}
	r.logf("%s", line)
	send(map[string]any{"type": "chatDone", "content": content.String(), "toolCalls": calls})
}

// toolStatus is the status line of an agent's tool call: the tool's own name (mcp__vboard__create_items →
// create_items …); a built-in tool with what it works on, short (WebSearch <query> …).
func toolStatus(name string, data map[string]any) string {
	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		if _, tool, ok := strings.Cut(rest, "__"); ok {
			return trimRunes(tool, 100) + " …" // its summary would be the arguments' JSON
		}
	}
	line := name + " …"
	if s, _ := data["summary"].(string); strings.TrimSpace(s) != "" && !strings.HasPrefix(s, "{") {
		line = name + " " + trimRunes(strings.Join(strings.Fields(s), " "), 60) + " …"
	}
	return trimRunes(line, 120)
}

// agentJob is the Claude Code job of an agent's {chat}: one turn in a long-running session per conversation (see
// agentSession), with vboard's MCP server (the connector's token, bound to the board) as its only MCP server, the
// board and the person in its system prompt, and only the newest message as the turn (the session has the history;
// a session new on the worker gets the history as a transcript first, engine claudePrompt). What the person selected
// and sees changes from turn to turn, so it is said in front of the message, not in the system prompt (which a
// running session read when it started).
//
// seen says whether a conversation key was asked for before (and remembers it); nil: never.
func agentJob(conf storedConnector, m *bridgeMsg, seen func(key string) bool) (map[string]any, error) {
	if len(m.Messages) == 0 {
		return nil, errors.New("the chat has no messages")
	}
	cx := bridgeContext{}
	if m.Context != nil {
		cx = *m.Context
	}
	board := cx.BoardID
	if board == "" {
		board = conf.Board
	}
	var system []string
	var msgs []map[string]any
	first, answered, lastUser := "", false, -1
	for i, x := range m.Messages {
		switch x.Role {
		case "system":
			system = append(system, x.Content)
		case "user":
			if first == "" {
				first = x.Content
			}
			lastUser = len(msgs)
			um := map[string]any{"role": "user", "content": x.Content}
			if len(x.Images) > 0 {
				um["images"] = jobImages(x.Images)
			}
			msgs = append(msgs, um)
		case "assistant":
			answered = true
			msgs = append(msgs, map[string]any{"role": "assistant", "content": x.Content})
		case "tool": // a model's tool turns: not an agent's
		default:
			return nil, fmt.Errorf("messages[%d]: unknown role %q", i, x.Role)
		}
	}
	if lastUser < 0 {
		return nil, errors.New("the chat has no message of the person")
	}
	if note := turnNote(cx); note != "" {
		msgs[lastUser]["content"] = note + "\n\n" + msgs[lastUser]["content"].(string)
	}
	system = append(system, agentSystem(cx, board))
	all := append([]map[string]any{{"role": "system", "content": strings.Join(system, "\n\n")}}, msgs...)
	mcpURL := conf.URL + "/mcp"
	if board != "" {
		mcpURL += "?board=" + url.QueryEscape(board) // vboard's MCP: the tools default to this board
	}
	key := agentSession(conf.ID, cx.UserID, board, cx.ConversationID, first)
	// a new conversation (nothing answered yet; not the "Continue" of a broken-off first answer) starts anew: with
	// vboard's conversation id only the first time the connector sees that id (the key is the conversation's own:
	// a retried first question keeps its session); without one (older vboards) even when an earlier conversation
	// began with the same words
	fresh := !answered && !cx.Resume
	if conversationID(cx.ConversationID) != "" && seen != nil && seen(key) {
		fresh = false
	}
	return map[string]any{
		"messages": all,
		"think":    false,
		"session":  key,
		"fresh":    fresh,
		"mcp": []map[string]any{{"name": "vboard", "url": mcpURL,
			"headers": map[string]string{"Authorization": "Bearer " + conf.Token}}},
	}, nil
}

// agentSession is the conversation key of an agent's chat: the connector, the person, the board and the
// conversation — vboard's conversation id (context.conversationId, one per chat conversation of its panel) when it
// sends one; else (vboards before it) the conversation's first message stands for it: vboard sends the whole
// conversation (its last 40 messages) every turn. Follow-up turns of a conversation land in its warm session; another
// conversation, board or person gets another one. Without an id two conversations that open with the same words at
// the same time share a session, and when vboard's window drops the first message (long chats) the key changes: a
// new session that gets the remaining history as a transcript.
func agentSession(connector, user, board, conversation, first string) string {
	prefix := "vboard:" + connector + ":" + user + ":" + board + ":"
	if id := conversationID(conversation); id != "" {
		return prefix + id
	}
	h := sha256.Sum256([]byte(first))
	return prefix + hex.EncodeToString(h[:6])
}

// conversationID is vboard's conversation id as a key part: as sent when it is short and plain (at most 128
// printable characters without spaces), else its hash ("c" + 24 hex digits); "" when there is none.
func conversationID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	plain := len(id) <= 128
	for _, r := range id {
		plain = plain && unicode.IsPrint(r) && !unicode.IsSpace(r)
	}
	if plain {
		return id
	}
	h := sha256.Sum256([]byte(id))
	return "c" + hex.EncodeToString(h[:12])
}

// seenConversations remembers the conversation keys the connectors were asked for (the newest 10000, for a day):
// a conversation is new (fresh) only the first time.
type seenConversations struct {
	mu sync.Mutex
	m  map[string]time.Time
}

const (
	seenMax = 10000
	seenAge = 24 * time.Hour
)

// seen says whether key was seen before, and remembers it.
func (s *seenConversations) seen(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.m == nil {
		s.m = map[string]time.Time{}
	}
	t, ok := s.m[key]
	ok = ok && now.Sub(t) < seenAge
	s.m[key] = now
	if len(s.m) > seenMax {
		var oldest string
		for k, t := range s.m {
			if now.Sub(t) >= seenAge {
				delete(s.m, k)
			} else if oldest == "" || t.Before(s.m[oldest]) {
				oldest = k
			}
		}
		if len(s.m) > seenMax {
			delete(s.m, oldest)
		}
	}
	return ok
}

// agentSystem is what the connector adds to vboard's system message for an agent: how it reaches the board.
func agentSystem(cx bridgeContext, board string) string {
	var b strings.Builder
	b.WriteString("You reach vboard through the MCP server \"vboard\" (tools mcp__vboard__…); its tools act as ")
	if cx.UserName != "" {
		b.WriteString(cx.UserName)
	} else {
		b.WriteString("the person you work for")
	}
	if cx.Role != "" {
		fmt.Fprintf(&b, " (role on the board: %s)", cx.Role)
	}
	b.WriteString(", with their permissions. ")
	if board != "" {
		name := ""
		if cx.BoardName != "" {
			name = fmt.Sprintf(" %q", cx.BoardName)
		}
		fmt.Fprintf(&b, "This conversation is about the board%s (id %s): the tools default to it; pass boardId %q where a tool takes one. ", name, board, board)
	}
	b.WriteString("Read and change the board only with the vboard tools, and say only what you did. ")
	lang := "the language of the person's message"
	switch strings.ToLower(cx.Lang) {
	case "de":
		lang = "German unless the person writes in another language"
	case "en":
		lang = "English unless the person writes in another language"
	}
	fmt.Fprintf(&b, "Answer briefly in %s (Markdown allowed).", lang)
	return b.String()
}

// turnNote is what vboard's context says about this turn (the selection, the view, a broken-off answer), in front of
// the person's message.
func turnNote(cx bridgeContext) string {
	if cx.BoardID == "" && len(cx.Selection) == 0 && cx.View == nil && !cx.Resume {
		return ""
	}
	var parts []string
	if len(cx.Selection) > 0 {
		parts = append(parts, fmt.Sprintf("selected item(s): %s", strings.Join(cx.Selection, ", ")))
	} else {
		parts = append(parts, "nothing is selected")
	}
	if v := cx.View; v != nil {
		parts = append(parts, fmt.Sprintf("the person sees x %.0f, y %.0f, %.0f × %.0f px", v.X, v.Y, v.W, v.H))
	}
	if cx.Resume {
		parts = append(parts, "your previous answer to this message broke off: the board may already hold part of it — do only what is still missing")
	}
	return "[vboard, now: " + strings.Join(parts, "; ") + "]"
}
