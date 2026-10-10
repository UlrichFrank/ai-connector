package hub

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/llm"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/web"
)

func httptestRequest(method, path string) *http.Request {
	return httptest.NewRequest(method, path, nil)
}

// fakeChat streams a thought and an answer in pieces that split the tags; with the caller's tools it calls one.
// While hold is set, a job waits for it (or its cancel).
type fakeChat struct {
	mu   sync.Mutex
	got  []engine.Request
	hold chan struct{}
}

func (b *fakeChat) Name() string   { return "fakechat" }
func (b *fakeChat) Heavy() bool    { return false }
func (b *fakeChat) Loaded() string { return "" }
func (b *fakeChat) Unload()        {}
func (b *fakeChat) Run(ctx context.Context, _ *engine.Model, r engine.Request, _ string, emit engine.Emit) error {
	b.mu.Lock()
	b.got = append(b.got, r)
	hold := b.hold
	b.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	pieces := []string{"Hel", "lo!"}
	if r.Think {
		pieces = []string{"<think>", "Let me ", "think.</th", "ink>\n\nHel", "lo!"}
	}
	for _, p := range pieces {
		emit(engine.Event{Type: "text", Text: p})
	}
	emit(engine.Event{Type: "stats", Data: map[string]any{"promptTokens": 7, "tokens": 5}})
	if len(r.Tools) > 0 {
		emit(engine.Event{Type: "result", Data: map[string]any{"toolCalls": []map[string]any{
			{"id": "call_1", "name": r.Tools[0].Name, "arguments": `{"city":"Berlin"}`}}}})
	}
	return nil
}

func (b *fakeChat) last() engine.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.got[len(b.got)-1]
}

// fakeText embeds (every text to [3, 4], normalised by the worker) and classifies (the last option), and keeps
// what it was asked.
type fakeText struct {
	mu    sync.Mutex
	asked []string
}

func (b *fakeText) Name() string   { return "faketext" }
func (b *fakeText) Heavy() bool    { return false }
func (b *fakeText) Loaded() string { return "" }
func (b *fakeText) Unload()        {}
func (b *fakeText) Run(context.Context, *engine.Model, engine.Request, string, engine.Emit) error {
	return nil
}
func (b *fakeText) Embed(_ context.Context, _ *engine.Model, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{3, 4}
	}
	return out, nil
}
func (b *fakeText) Classify(_ context.Context, _ *engine.Model, states []string, instructions string, opts []engine.Option) ([]engine.Answer, error) {
	var labels []string
	for _, o := range opts {
		labels = append(labels, o.Label+"="+o.Description)
	}
	b.mu.Lock()
	b.asked = append(b.asked, states[0]+" | "+instructions+" | "+strings.Join(labels, ","))
	b.mu.Unlock()
	last := opts[len(opts)-1].Label
	return []engine.Answer{{Choice: last, Probabilities: map[string]float64{last: 0.75, opts[0].Label: 0.25}, Confidence: 0.75}}, nil
}

type apiMac struct {
	mgr  *jobs.Manager
	chat *fakeChat
	text *fakeText
}

// startAPIMac connects a worker with a chat model, the embedder and the classifier.
func startAPIMac(t *testing.T, base, token string) *apiMac {
	t.Helper()
	cat := &engine.Catalog{Models: []*engine.Model{
		{ID: "qwen3.6-35b-a3b", Name: "Qwen3.6", Kind: engine.Chat, Backend: "fakechat", Size: gib},
		{ID: "qwen3-embedding-0.6b", Name: "Embedder", Kind: engine.Embed, Backend: "faketext", Size: gib},
		{ID: "laya-typed-decisions", Name: "Laya", Kind: engine.Classify, Backend: "faketext", Size: gib},
		{ID: "small", Name: "Small", Kind: engine.Image, Backend: "faketext", Size: gib},
	}}
	m := &apiMac{chat: &fakeChat{}, text: &fakeText{}}
	dir := t.TempDir()
	m.mgr = jobs.New(cat, []engine.Backend{m.chat, m.text}, dir)
	api := (&web.Server{Jobs: m.mgr, Prompts: prompts.Open(dir), Version: "test"}).Handler()
	stop, _ := connectWorkerWith(t, base, token, withRAM(api, 64)) // a 64 GB Mac, whatever runs the test
	t.Cleanup(func() {
		stop()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !m.mgr.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	return m
}

func makeClient(t *testing.T, base, device, name string) (Client, string) {
	t.Helper()
	r := do(t, "POST", base+"/api/clients", device, strings.NewReader(`{"name":"`+name+`"}`))
	if r.StatusCode != 200 {
		t.Fatalf("make client: %d", r.StatusCode)
	}
	var out struct {
		Client Client `json:"client"`
		Token  string `json:"token"`
	}
	decode(t, r, &out)
	return out.Client, out.Token
}

func post(t *testing.T, url, token, body string) (int, []byte) {
	t.Helper()
	r := do(t, "POST", url, token, strings.NewReader(body))
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, b
}

// openAIError reads {"error":{"message","type","code"}}.
func openAIError(t *testing.T, b []byte) (typ, code string) {
	t.Helper()
	var e struct {
		Error struct {
			Message string
			Type    string
			Code    *string
		}
	}
	if err := json.Unmarshal(b, &e); err != nil || e.Error.Message == "" || e.Error.Type == "" {
		t.Fatalf("not an OpenAI error: %s", b)
	}
	if e.Error.Code != nil {
		code = *e.Error.Code
	}
	return e.Error.Type, code
}

// Client tokens: made, listed and revoked by a device; they reach the client APIs and the native jobs API only.
func TestClientTokens(t *testing.T) {
	dir := t.TempDir()
	_, srv := newHub(t, dir)
	base := srv.URL
	device := signIn(t, base, "laptop")

	if c, _ := post(t, base+"/api/clients", device, `{"name":"  "}`); c != 400 {
		t.Fatalf("blank name: %d", c)
	}
	if c, _ := post(t, base+"/api/clients", "", `{"name":"x"}`); c != 401 {
		t.Fatalf("without sign-in: %d", c)
	}
	cl, token := makeClient(t, base, device, "docscanner")
	if !strings.HasPrefix(token, "aic_") || len(token) != 4+43 || cl.ID == "" || cl.Name != "docscanner" || cl.Created.IsZero() {
		t.Fatalf("client %+v %q", cl, token)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "clients.json")); strings.Contains(string(b), token) || !strings.Contains(string(b), "docscanner") {
		t.Fatalf("clients.json: %s", b)
	}

	// not the console's endpoints
	for _, req := range []struct{ method, path string }{
		{"GET", "/api/workers"}, {"POST", "/api/workers"}, {"GET", "/api/clients"}, {"POST", "/api/clients"},
		{"DELETE", "/api/clients/" + cl.ID}, {"GET", "/api/devices"}, {"POST", "/api/invites"}, {"GET", "/api/worker/events"},
		{"POST", "/api/upload"}, {"POST", "/api/jobs/m1.1-1/title"}, {"GET", "/api/prompts"},
	} {
		if r := do(t, req.method, base+req.path, token, nil); r.StatusCode != 401 {
			t.Errorf("client token on %s %s: %d", req.method, req.path, r.StatusCode)
		}
	}
	// the native jobs API: past the sign-in (no worker yet: offline)
	for _, path := range []string{"/api/jobs", "/api/models"} {
		if r := do(t, "GET", base+path, token, nil); r.StatusCode != 503 {
			t.Errorf("client token on %s: %d", path, r.StatusCode)
		}
	}
	// the client APIs: a client or a device token, else an OpenAI 401
	for _, tok := range []string{token, device} {
		if r := do(t, "GET", base+"/v1/models", tok, nil); r.StatusCode != 200 {
			t.Errorf("/v1/models: %d", r.StatusCode)
		}
	}
	for _, tok := range []string{"", "aic_nope", workerToken} {
		c, b := post(t, base+"/v1/embeddings", tok, `{"input":"x"}`)
		if typ, code := openAIError(t, b); c != 401 || typ != "invalid_request_error" || code != "invalid_api_key" {
			t.Errorf("%q: %d %s", tok, c, b)
		}
	}
	// a client token in the cookie or the query is no client token
	req, _ := http.NewRequest("GET", base+"/api/jobs?token="+token, nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: token})
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 401 {
		t.Errorf("client token as cookie/query: %d", r.StatusCode)
	}

	var list []Client
	decode(t, do(t, "GET", base+"/api/clients", device, nil), &list)
	if len(list) != 1 || list[0].ID != cl.ID || list[0].LastSeen.IsZero() {
		t.Fatalf("clients %+v", list)
	}
	if r := do(t, "DELETE", base+"/api/clients/nope", device, nil); r.StatusCode != 404 {
		t.Fatalf("revoke unknown: %d", r.StatusCode)
	}
	if r := do(t, "DELETE", base+"/api/clients/"+cl.ID, device, nil); r.StatusCode != 204 {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	if r := do(t, "GET", base+"/v1/models", token, nil); r.StatusCode != 401 {
		t.Fatalf("revoked token: %d", r.StatusCode)
	}
	// the store survives a restart
	_, tok2 := makeClient(t, base, device, "vboard")
	_, srv2 := newHub(t, dir)
	if r := do(t, "GET", srv2.URL+"/v1/models", tok2, nil); r.StatusCode != 200 {
		t.Fatalf("after a restart: %d", r.StatusCode)
	}
}

// Embeddings (OpenAI and TEI) and Laya through a worker's direct path.
func TestEmbedClassifyAPIs(t *testing.T) {
	_, srv := newHub(t, t.TempDir())
	base := srv.URL
	device := signIn(t, base, "laptop")
	_, token := makeClient(t, base, device, "docscanner")

	// no worker: 503
	c, b := post(t, base+"/v1/embeddings", token, `{"input":"x"}`)
	if _, code := openAIError(t, b); c != 503 || code != "no_worker" {
		t.Fatalf("no worker: %d %s", c, b)
	}
	_, wtok := enroll(t, base, device, "Mac")
	mac := startAPIMac(t, base, wtok)

	// models: the chat, embed and classify ones, with their workers
	var ms struct {
		Object string
		Data   []struct {
			ID, Object, Kind string
			Workers          []string
		}
	}
	decode(t, do(t, "GET", base+"/v1/models", token, nil), &ms)
	var ids []string
	for _, m := range ms.Data {
		ids = append(ids, m.ID+":"+m.Kind+":"+strings.Join(m.Workers, ","))
	}
	if ms.Object != "list" || strings.Join(ids, " ") != "qwen3.6-35b-a3b:chat:Mac qwen3-embedding-0.6b:embed:Mac laya-typed-decisions:classify:Mac" {
		t.Fatalf("models %v", ids)
	}

	type embeddings struct {
		Object string
		Model  string
		Data   []struct {
			Object    string
			Index     int
			Embedding json.RawMessage
		}
		Usage map[string]int
	}
	// floats; the model by its Hugging Face name, answered with its catalogue id
	c, b = post(t, base+"/v1/embeddings", token, `{"model":"Qwen/Qwen3-Embedding-0.6B","input":["a","b"]}`)
	var e embeddings
	json.Unmarshal(b, &e)
	if c != 200 || e.Object != "list" || e.Model != "qwen3-embedding-0.6b" || len(e.Data) != 2 || e.Data[1].Index != 1 ||
		e.Data[0].Object != "embedding" || string(e.Data[0].Embedding) != "[0.6,0.8]" {
		t.Fatalf("embeddings %d %s", c, b)
	}
	// base64 (the OpenAI SDKs' default), the default model, one text
	c, b = post(t, base+"/v1/embeddings", token, `{"input":"a","encoding_format":"base64"}`)
	e = embeddings{}
	json.Unmarshal(b, &e)
	var s string
	if c != 200 || e.Model != "qwen3-embedding-0.6b" || len(e.Data) != 1 || json.Unmarshal(e.Data[0].Embedding, &s) != nil {
		t.Fatalf("base64 %d %s", c, b)
	}
	raw, _ := base64.StdEncoding.DecodeString(s)
	if len(raw) != 8 || math.Float32frombits(binary.LittleEndian.Uint32(raw)) != 0.6 || math.Float32frombits(binary.LittleEndian.Uint32(raw[4:])) != 0.8 {
		t.Fatalf("base64 vector %v", raw)
	}
	for _, x := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"input":[[1,2,3]]}`, 400, ""},
		{`{"input":[]}`, 400, ""},
		{`{}`, 400, ""},
		{`{"input":"a","encoding_format":"int8"}`, 400, ""},
		{`{"input":"a","model":"nope"}`, 404, "model_not_found"},
		{`{"input":"a","model":"qwen3.6"}`, 400, ""}, // a chat model
		{`{"input":` + `[` + strings.Repeat(`"a",`, 2048) + `"a"]}`, 400, ""},
	} {
		c, b := post(t, base+"/v1/embeddings", token, x.body)
		if _, code := openAIError(t, b); c != x.status || code != x.code {
			t.Errorf("%.60s: %d %s", x.body, c, b)
		}
	}

	// TEI
	c, b = post(t, base+"/embed", token, `{"inputs":["a","b"],"normalize":true,"truncate":true}`)
	if c != 200 || string(b) != "[[0.6,0.8],[0.6,0.8]]\n" {
		t.Fatalf("TEI %d %s", c, b)
	}
	c, b = post(t, base+"/embed", token, `{"inputs":"a"}`)
	if c != 200 || string(b) != "[[0.6,0.8]]\n" {
		t.Fatalf("TEI one %d %s", c, b)
	}
	c, b = post(t, base+"/embed", token, `{"inputs":[]}`)
	var te map[string]string
	if json.Unmarshal(b, &te); c != 400 || te["error"] == "" || te["error_type"] != "Validation" {
		t.Fatalf("TEI error %d %s", c, b)
	}

	// Laya: the state as Python's json.dumps, the criteria in their order
	c, b = post(t, base+"/v1/systemone", token, `{"model":"typed-decisions","state":{"title":"Grüße","n":1.0,"tags":["x"]},
		"questions":{"frame":{"type":"choice","instructions":"Where?","criteria":{"todo":"open","done":"finished","doing":"started"}}}}`)
	var la struct {
		Answers map[string]struct {
			Choice           string
			Probabilities    map[string]float64
			AnswerConfidence float64 `json:"answer_confidence"`
		}
	}
	json.Unmarshal(b, &la)
	if a := la.Answers["frame"]; c != 200 || a.Choice != "doing" || a.AnswerConfidence != 0.75 || a.Probabilities["todo"] != 0.25 {
		t.Fatalf("laya %d %s", c, b)
	}
	if got := mac.text.asked[0]; got != `{"title": "Grüße", "n": 1.0, "tags": ["x"]} | Where? | todo=open,done=finished,doing=started` {
		t.Fatalf("asked %q", got)
	}
	c, b = post(t, base+"/v1/systemone", token, `{"state":{},"questions":{"q":{"type":"text","criteria":{"a":""}}}}`)
	if c != 400 {
		t.Fatalf("laya text question %d %s", c, b)
	}
	if len(mac.mgr.Jobs()) != 0 {
		t.Fatalf("the direct path made jobs: %d", len(mac.mgr.Jobs()))
	}
}

// sse reads an event stream: its data lines, and whether a ": ping" came first.
func sse(t *testing.T, r *http.Response) (pingFirst bool, data []string) {
	t.Helper()
	defer r.Body.Close()
	sc := bufio.NewScanner(r.Body)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if first {
			pingFirst, first = line == ": ping", false
		}
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			data = append(data, d)
		}
	}
	return pingFirst, data
}

// Chat completions: a job on the worker (with the client's name), answered whole or streamed, thought and answer
// apart, the caller's tools called.
func TestChatCompletions(t *testing.T) {
	defer func(d time.Duration) { chatPoll = d }(chatPoll)
	chatPoll = 200 * time.Millisecond
	_, srv := newHub(t, t.TempDir())
	base := srv.URL
	device := signIn(t, base, "laptop")
	_, token := makeClient(t, base, device, "docscanner")
	_, wtok := enroll(t, base, device, "Mac")
	mac := startAPIMac(t, base, wtok)

	// not streamed, thinking asked for
	c, b := post(t, base+"/v1/chat/completions", token, `{"model":"qwen3.6","reasoning_effort":"low","max_completion_tokens":100,
		"messages":[{"role":"developer","content":"be brief"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	var res struct {
		ID, Object, Model string
		Choices           []struct {
			Message struct {
				Role, Content    string
				ReasoningContent string `json:"reasoning_content"`
			}
			FinishReason string `json:"finish_reason"`
		}
		Usage map[string]int
	}
	json.Unmarshal(b, &res)
	if c != 200 || res.Object != "chat.completion" || res.Model != "qwen3.6-35b-a3b" || len(res.Choices) != 1 ||
		res.Choices[0].Message.Content != "Hello!" || res.Choices[0].Message.ReasoningContent != "Let me think." ||
		res.Choices[0].Message.Role != "assistant" || res.Choices[0].FinishReason != "stop" ||
		res.Usage["prompt_tokens"] != 7 || res.Usage["completion_tokens"] != 5 || res.Usage["total_tokens"] != 12 {
		t.Fatalf("completion %d %s", c, b)
	}
	got := mac.chat.last()
	if !got.Think || got.Client != "docscanner" || got.MaxTokens != 100 || got.Messages[0].Role != "system" || got.Messages[1].Content != "hi" {
		t.Fatalf("job request %+v", got)
	}
	// it is a job like any other, with the client's name
	j := getJob(t, base, token, res.ID)
	if j.State != "done" || j.Request["client"] != "docscanner" {
		t.Fatalf("job %+v", j)
	}

	// thinking off: "none", max_tokens, a device token (no client); the answer alone
	c, b = post(t, base+"/v1/chat/completions", device, `{"model":"qwen3.6-35b-a3b","reasoning_effort":"none","max_tokens":5,
		"messages":[{"role":"user","content":"hi"}]}`)
	res.Choices = nil
	json.Unmarshal(b, &res)
	if got := mac.chat.last(); c != 200 || got.Think || got.Client != "" || got.MaxTokens != 5 ||
		res.Choices[0].Message.Content != "Hello!" || res.Choices[0].FinishReason != "length" {
		t.Fatalf("thinking off %d %s %+v", c, b, got)
	}

	// streamed, with the caller's tools and a tool turn in the history
	r := do(t, "POST", base+"/v1/chat/completions", token, strings.NewReader(`{"model":"qwen3.6-35b-a3b","stream":true,"think":true,
		"tools":[{"type":"function","function":{"name":"get_weather","description":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
		"messages":[{"role":"user","content":"time?"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"c0","type":"function","function":{"name":"get_time","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c0","content":"12:00"},{"role":"user","content":"weather?"}]}`))
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d", r.StatusCode)
	}
	pingFirst, data := sse(t, r)
	if !pingFirst || len(data) < 4 || data[len(data)-1] != "[DONE]" {
		t.Fatalf("stream %v %v", pingFirst, data)
	}
	var reasoning, content strings.Builder
	type chunk struct {
		ID, Object string
		Choices    []struct {
			Delta struct {
				Role             string
				Content          string
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int
					ID, Type string
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			}
			FinishReason *string `json:"finish_reason"`
		}
		Usage map[string]int
	}
	var first, last chunk
	json.Unmarshal([]byte(data[0]), &first)
	json.Unmarshal([]byte(data[len(data)-2]), &last)
	for _, d := range data[:len(data)-1] {
		var ch chunk
		if err := json.Unmarshal([]byte(d), &ch); err != nil || ch.Object != "chat.completion.chunk" || len(ch.Choices) != 1 {
			t.Fatalf("chunk %s", d)
		}
		reasoning.WriteString(ch.Choices[0].Delta.ReasoningContent)
		content.WriteString(ch.Choices[0].Delta.Content)
	}
	if first.Choices[0].Delta.Role != "assistant" || reasoning.String() != "Let me think." || content.String() != "Hello!" {
		t.Fatalf("streamed %q %q %s", reasoning.String(), content.String(), data)
	}
	tc := last.Choices[0].Delta.ToolCalls
	if len(tc) != 1 || tc[0].Index != 0 || tc[0].ID != "call_1" || tc[0].Type != "function" || tc[0].Function.Name != "get_weather" ||
		tc[0].Function.Arguments != `{"city":"Berlin"}` || *last.Choices[0].FinishReason != "tool_calls" || last.Usage["total_tokens"] != 12 {
		t.Fatalf("last chunk %s", data[len(data)-2])
	}
	got = mac.chat.last()
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" || !strings.Contains(string(got.Tools[0].Parameters), "city") ||
		len(got.Messages) != 4 || got.Messages[1].ToolCalls[0] != (llm.ToolCall{ID: "c0", Name: "get_time", Arguments: "{}"}) ||
		got.Messages[2].ToolCallID != "c0" || got.Messages[2].Content != "12:00" {
		t.Fatalf("job request %+v", got)
	}

	// not streamed with tools: tool_calls without the index, content null
	c, b = post(t, base+"/v1/chat/completions", token, `{"model":"qwen3.6-35b-a3b","messages":[{"role":"user","content":"w?"}],
		"tools":[{"type":"function","function":{"name":"get_weather"}}]}`)
	var withTools map[string]any
	json.Unmarshal(b, &withTools)
	msg := withTools["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls, _ := msg["tool_calls"].([]any)
	if c != 200 || len(calls) != 1 || calls[0].(map[string]any)["index"] != nil || msg["content"] != "Hello!" {
		t.Fatalf("tools, not streamed %d %s", c, b)
	}

	// bad requests
	for _, x := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"messages":[{"role":"user","content":"x"}]}`, 400, ""},
		{`{"model":"qwen3.6-35b-a3b","messages":[]}`, 400, ""},
		{`{"model":"qwen3.6-35b-a3b","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, 400, ""},
		{`{"model":"qwen3.6-35b-a3b","messages":[{"role":"wizard","content":"x"}]}`, 400, ""},
		{`{"model":"gpt-4o","messages":[{"role":"user","content":"x"}]}`, 404, "model_not_found"},
		{`{"model":"qwen3-embedding-0.6b","messages":[{"role":"user","content":"x"}]}`, 400, ""},
	} {
		c, b := post(t, base+"/v1/chat/completions", token, x.body)
		if _, code := openAIError(t, b); c != x.status || code != x.code {
			t.Errorf("%s: %d %s", x.body, c, b)
		}
	}

	// the native jobs API: a client's name replaces what it says; a device cannot claim one
	code, j1, _ := submitJob(t, base, token, `{"model":"qwen3.6-35b-a3b","client":"someone else","messages":[{"role":"user","content":"x"}]}`)
	if code != 200 || j1.Request["client"] != "docscanner" {
		t.Fatalf("client job %d %+v", code, j1)
	}
	code, j2, _ := submitJob(t, base, device, `{"model":"qwen3.6-35b-a3b","client":"docscanner","messages":[{"role":"user","content":"x"}]}`)
	if code != 200 || j2.Request["client"] != nil {
		t.Fatalf("device job %d %+v", code, j2)
	}
	waitDone(t, base, token, j1.ID)
	waitDone(t, base, device, j2.ID)
	if r := do(t, "POST", base+"/api/jobs/"+j2.ID+"/cancel", token, nil); r.StatusCode == 401 {
		t.Fatalf("cancel with a client token: %d", r.StatusCode)
	}
}

// The client going away cancels its job; a waiting stream is open (a ping) before the job runs.
func TestChatCanceledWhenClientGoes(t *testing.T) {
	defer func(d time.Duration) { chatPoll = d }(chatPoll)
	chatPoll = 100 * time.Millisecond
	_, srv := newHub(t, t.TempDir())
	base := srv.URL
	device := signIn(t, base, "laptop")
	_, token := makeClient(t, base, device, "docscanner")
	_, wtok := enroll(t, base, device, "Mac")
	mac := startAPIMac(t, base, wtok)
	hold := make(chan struct{})
	defer close(hold)
	mac.chat.mu.Lock()
	mac.chat.hold = hold
	mac.chat.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3.6-35b-a3b","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(r.Body)
	if line, _ := br.ReadString('\n'); line != ": ping\n" {
		t.Fatalf("first line %q", line)
	}
	waitFor(t, "the job running", func() bool {
		js := mac.mgr.Jobs()
		return len(js) == 1 && js[0].State == jobs.Running
	})
	cancel()
	r.Body.Close()
	waitFor(t, "the job canceled", func() bool { return mac.mgr.Jobs()[0].State == jobs.Canceled })
}
