package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

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

// bridgeCaps are the models the connector answers with now (catalogue ids; "" = it cannot do that now).
type bridgeCaps struct{ classify, embed, chat string }

func (c bridgeCaps) empty() bool { return c == bridgeCaps{} }

func (c bridgeCaps) String() string {
	var parts []string
	for _, p := range []struct{ k, v string }{{"classify", c.classify}, {"embed", c.embed}, {"chat", c.chat}} {
		if p.v != "" {
			parts = append(parts, p.k+" "+p.v)
		}
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

// connectorCaps resolves a connector's models against what the connected workers list now: the chosen model or the
// server's default (MLXLAB_CLASSIFY_MODEL, MLXLAB_EMBED_MODEL; chat: the first chat model a worker lists that is not
// Claude Code).
func (s *Server) connectorCaps(ctx context.Context, want ConnectorModels) bridgeCaps {
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
	c := bridgeCaps{classify: find(or(want.Classify, s.cfg.ClassifyModel), "classify"), embed: find(or(want.Embed, s.cfg.EmbedModel), "embed")}
	if want.Chat != "" {
		c.chat = find(want.Chat, "chat")
	} else {
		for _, m := range models {
			// not Claude Code: it uses its owner's subscription, so a connector uses it only when chosen
			if m.Kind == "chat" && m.Backend != "claude-code" && !strings.HasPrefix(m.ID, claudePrefix) {
				c.chat = m.ID
				break
			}
		}
	}
	return c
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
	conf     storedConnector
	gen      int
	instance string // hello.instance: the old and the new connection of a handover share it
}

func (r *vboardRun) logf(format string, args ...any) {
	log.Printf("connector %q: "+format, append([]any{r.conf.Name}, args...)...)
}

func (r *vboardRun) set(update func(st *connectorStatus)) { r.m.report(r.conf.ID, r.gen, update) }

func (r *vboardRun) connecting(msg string) {
	r.set(func(st *connectorStatus) { st.State, st.Error, st.Since = "connecting", msg, time.Time{} })
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
			if user, err = r.me(ctx); err == nil {
				if c, err = r.dial(ctx, caps); err == nil && c.user == "" {
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
			})
			if moved {
				r.logf("moved to a new connection — answers %s", c.caps)
			} else {
				r.logf("connected to %s as %s — answers %s", r.conf.URL, c.user, c.caps)
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
			r.set(func(st *connectorStatus) { st.State, st.Error, st.Since = "error", err.Error(), time.Time{} })
			return
		}
		if served {
			backoff = bridgeBackoffMin // it worked for a while: start over with short pauses
		}
		r.logf("connection lost (%v); retrying in %s", err, backoff)
		r.connecting(fmt.Sprintf("%v (retrying in %s)", err, backoff))
		if r.m.sleep(ctx, backoff) != nil {
			return
		}
		backoff = min(backoff*2, bridgeBackoffMax)
	}
}

// waitCaps waits until a connected worker offers one of the connector's models (false: ctx ended).
func (r *vboardRun) waitCaps(ctx context.Context) (bridgeCaps, bool) {
	for {
		changed := r.s.fleet.Changed()
		caps := r.s.connectorCaps(ctx, r.conf.Models)
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
		case <-time.After(bridgeCapsEvery):
		}
	}
}

// me asks vboard whose the token is (GET /api/v1/me, as vboard-ai does before it dials): a refused token stops here.
func (r *vboardRun) me(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, bridgeDialTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.conf.URL+"/api/v1/me", nil)
	if err != nil {
		return "", &bridgeFatal{fmt.Sprintf("%q is not a vboard address", r.conf.URL)}
	}
	req.Header.Set("Authorization", "Bearer "+r.conf.Token)
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
func (r *vboardRun) wsURL() (string, error) {
	u, err := url.Parse(r.conf.URL)
	if err != nil || u.Host == "" {
		return "", &bridgeFatal{fmt.Sprintf("%q is not a vboard address", r.conf.URL)}
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", &bridgeFatal{fmt.Sprintf("%q: the address starts with https:// (or http://)", r.conf.URL)}
	}
	u.Path = strings.TrimRight(u.Path, "/") + bridgePath
	q := url.Values{}
	if r.conf.Board != "" {
		q.Set("board", r.conf.Board)
	}
	q.Set("name", r.conf.Name)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// bridgeConn is one WebSocket to vboard.
type bridgeConn struct {
	ws   *websocket.Conn
	caps bridgeCaps // what its hello said
	user string     // welcome.user

	wmu sync.Mutex
}

// dial opens a connection: the WebSocket, {hello} with what the workers offer, vboard's {welcome}.
func (r *vboardRun) dial(ctx context.Context, caps bridgeCaps) (*bridgeConn, error) {
	u, err := r.wsURL()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, bridgeDialTimeout)
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+r.conf.Token)
	ws, res, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if res != nil && res.StatusCode != http.StatusSwitchingProtocols {
			return nil, bridgeRefused(res, r.conf.Board)
		}
		return nil, err
	}
	ws.SetReadLimit(bridgeInbound)
	// the caps are vboard's (bridge.go trimTo); instance + handover: this bridge understands {reconnect}
	hello, _ := json.Marshal(map[string]any{"type": "hello", "name": trimRunes(r.conf.Name, 80), "device": bridgeDevice,
		"model": trimRunes(caps.classify, 60), "embed": trimRunes(caps.embed, 80), "chat": trimRunes(caps.chat, 80),
		"instance": r.instance, "handover": true})
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
	return &bridgeConn{ws: ws, caps: caps, user: welcome.User}, nil
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
}

type bridgeChatMsg struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []bridgeToolCall `json:"toolCalls"`
	ToolCallID string           `json:"toolCallId"`
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
			nc := r.handoverDial(cctx)
			if nc == nil {
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
	tick := time.NewTicker(bridgeCapsEvery)
	defer tick.Stop()
	for {
		changed := r.s.fleet.Changed()
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
			r.m.wg.Add(1)
			go func() {
				defer r.m.wg.Done()
				// the old connection: open until what runs on it is answered (and the settle time passed), then closed
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
			return next, nil
		case <-ctx.Done():
			stop(false)
			return nil, ctx.Err()
		case <-changed:
		case <-tick.C:
		}
		caps := r.s.connectorCaps(cctx, r.conf.Models)
		if cctx.Err() != nil || caps == c.caps {
			continue
		}
		if caps.empty() {
			_ = c.ws.Close(websocket.StatusNormalClosure, "nothing to offer")
			stop(false)
			return nil, errNothingOffered
		}
		handover(fmt.Sprintf("the workers offer %s now (was %s)", caps, c.caps))
	}
}

// handoverDial opens the new connection of a handover, with what the workers offer now (retrying: the new vboard
// replica may not be ready yet). nil: it gave up (the old connection ended, a final refusal, nothing to offer).
func (r *vboardRun) handoverDial(ctx context.Context) *bridgeConn {
	pause := bridgeBackoffMin
	for {
		caps := r.s.connectorCaps(ctx, r.conf.Models)
		if ctx.Err() != nil || caps.empty() {
			return nil
		}
		c, err := r.dial(ctx, caps)
		if err == nil {
			return c
		}
		if ctx.Err() != nil {
			return nil
		}
		if isBridgeFatal(err) {
			r.logf("new connection refused (%v); staying on the old one", err)
			return nil
		}
		r.logf("new connection failed (%v); retrying in %s", err, pause)
		if r.m.sleep(ctx, pause) != nil {
			return nil
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
		out, err = r.classify(ctx, c.caps, m)
	case "embed":
		out, err = r.embed(ctx, c.caps, m)
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
// the model thinks (the thought itself stays here), {chatDelta, status} while the job waits or its model loads, then
// {chatDone, content, toolCalls} — or {error}. Nothing is sent when it is canceled ({chatCancel}, the connection or the
// connector stopped): the job is canceled on the worker.
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
	if c.caps.chat == "" {
		fail("this connector cannot chat now: no connected worker offers a chat model")
		return
	}
	job, err := bridgeChatJob(m)
	if err != nil {
		fail(err.Error())
		return
	}
	// a claude-code model runs on the connector's owner's worker, with them as the job's owner (placeChat)
	run, e := r.s.placeChat(ctx, c.caps.chat, chatFor{r.conf.Owner, viaConnector})
	if e != nil {
		fail(e.message)
		return
	}
	n := run.n
	job["model"] = run.model
	job["client"] = "vboard: " + r.conf.Name
	events, unsubscribe := r.s.fleet.subscribe() // before the job exists: none of its events is missed
	defer unsubscribe()
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
	j, e := run.run(ctx, events, job, chatHooks{
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
				if st != status || quiet {
					status = st
					send(map[string]any{"type": "chatDelta", "status": trimRunes(st, bridgeErrorChars)})
				}
			}
		},
	})
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
	for _, tc := range j.Result.ToolCalls {
		args := tc.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		calls = append(calls, map[string]string{"id": tc.ID, "name": tc.Name, "arguments": args})
		names = append(names, tc.Name)
	}
	line := fmt.Sprintf("chat %s: answered in %.1f s on %s", id, time.Since(start).Seconds(), n.name)
	if len(names) > 0 {
		line += " → " + strings.Join(names, ", ")
	}
	r.logf("%s", line)
	send(map[string]any{"type": "chatDone", "content": content.String(), "toolCalls": calls})
}
