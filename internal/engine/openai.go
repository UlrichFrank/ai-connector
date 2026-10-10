package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/llm"
)

// OpenAIBackend runs chat and embedding models of local OpenAI-compatible servers — Ollama, LM Studio, llama.cpp,
// vLLM (Runtimes): the way a Linux or Windows machine (or a Mac with LM Studio) serves models the built-in engines do
// not have. The servers keep the models in memory themselves, so this backend is not heavy and unloads nothing.
// Its models are found by Discover, again and again while the worker runs (Watch).
type OpenAIBackend struct {
	Catalog  *Catalog
	Runtimes []Runtime
	Client   *http.Client // nil: http.DefaultClient

	state status
}

// BackendOpenAI is the backend name of the runtime models.
const BackendOpenAI = "openai"

func (b *OpenAIBackend) Name() string   { return BackendOpenAI }
func (b *OpenAIBackend) Heavy() bool    { return false }
func (b *OpenAIBackend) Loaded() string { return b.state.get() }
func (b *OpenAIBackend) Unload()        { b.state.set("") }

func (b *OpenAIBackend) client() *http.Client {
	if b.Client != nil {
		return b.Client
	}
	return http.DefaultClient
}

// Discover asks every runtime for its models now and puts them into the catalogue.
func (b *OpenAIBackend) Discover(ctx context.Context) []*Model {
	ms := discover(ctx, b.client(), b.Runtimes)
	b.Catalog.SetRuntime(BackendOpenAI, ms)
	return ms
}

// DiscoverNew asks every runtime for its models now and adds those that are new (a model pulled a moment ago),
// keeping the others: a runtime that does not answer within ctx (a short wait) takes nothing away; the next full
// scan (Watch) also drops what is gone. It returns how many were added.
func (b *OpenAIBackend) DiscoverNew(ctx context.Context) int {
	return b.Catalog.AddRuntime(BackendOpenAI, discover(ctx, b.client(), b.Runtimes))
}

// Watch discovers the models every interval until ctx ends (a runtime started or a model pulled after the worker
// shows up without a restart).
func (b *OpenAIBackend) Watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.Discover(ctx)
		}
	}
}

func (b *OpenAIBackend) Run(ctx context.Context, m *Model, r Request, _ string, emit Emit) error {
	if m.served == nil {
		return fmt.Errorf("%s: not a model of a local runtime", m.ID)
	}
	b.state.set(m.ID)
	switch m.Kind {
	case Chat:
		return b.chat(ctx, m, r, emit)
	case Embed:
		return b.embed(ctx, m, r, emit)
	}
	return fmt.Errorf("%s: %s models are not run through %s", m.ID, m.Kind, m.served.Name)
}

// ---------- chat ----------

type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func toOpenAI(msgs []llm.Message) []oaMessage {
	out := make([]oaMessage, len(msgs))
	for i, m := range msgs {
		o := oaMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, c := range m.ToolCalls {
			tc := oaToolCall{ID: c.ID, Type: "function"}
			tc.Function.Name, tc.Function.Arguments = c.Name, c.Arguments
			o.ToolCalls = append(o.ToolCalls, tc)
		}
		out[i] = o
	}
	return out
}

// chunk is one streamed piece of /chat/completions. The thinking comes as reasoning_content (llama.cpp, vLLM,
// LM Studio) or reasoning (Ollama).
type chunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (b *OpenAIBackend) chat(ctx context.Context, m *Model, r Request, emit Emit) error {
	if len(r.Messages) == 0 {
		return fmt.Errorf("no messages")
	}
	if r.Zone != "" && len(r.Tools) > 0 {
		return errClientTools
	}
	if r.Zone != "" {
		return fmt.Errorf("a %s chat (tools) runs on the built-in chat models only (MLX on a Mac); %s answers without tools", r.Zone, m.Name)
	}
	body := map[string]any{
		"model": m.served.Model, "messages": toOpenAI(withNow(r.Messages, time.Now())), "stream": true,
		"stream_options": map[string]any{"include_usage": true},
		// thinking on or off: Qwen-style templates (llama.cpp, vLLM; others ignore it)
		"chat_template_kwargs": map[string]any{"enable_thinking": r.Think},
	}
	if r.Temperature > 0 {
		body["temperature"] = r.Temperature
	}
	if r.Greedy {
		body["temperature"] = 0
	}
	if r.TopP > 0 {
		body["top_p"] = r.TopP
	}
	if r.PresencePenalty != nil {
		body["presence_penalty"] = *r.PresencePenalty
	}
	if r.MaxTokens > 0 {
		body["max_tokens"] = r.MaxTokens
	}
	if len(r.Tools) > 0 {
		tools := make([]map[string]any, len(r.Tools))
		for i, t := range r.Tools {
			params := t.Parameters
			if len(bytes.TrimSpace(params)) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools[i] = map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": params}}
		}
		body["tools"] = tools
	}
	emit.Status("asking " + m.served.Name + " …")
	start := time.Now()
	resp, err := b.post(ctx, m.served.Runtime, "/chat/completions", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var first time.Duration
	thinking := false
	tokens, promptTokens := 0, 0
	var calls []llm.ToolCall // the caller's tools: streamed in pieces, put together by index
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var c chunk
		if json.Unmarshal([]byte(data), &c) != nil {
			continue
		}
		if c.Error != nil {
			return fmt.Errorf("%s: %s", m.served.Name, c.Error.Message)
		}
		if c.Usage != nil {
			promptTokens, tokens = c.Usage.PromptTokens, c.Usage.CompletionTokens
		}
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Index < 0 || tc.Index > 255 {
					continue
				}
				for len(calls) <= tc.Index {
					calls = append(calls, llm.ToolCall{})
				}
				call := &calls[tc.Index]
				if tc.ID != "" {
					call.ID = tc.ID
				}
				call.Name += tc.Function.Name
				call.Arguments += tc.Function.Arguments
			}
			think := ch.Delta.ReasoningContent + ch.Delta.Reasoning
			if think == "" && ch.Delta.Content == "" {
				continue
			}
			if first == 0 {
				first = time.Since(start)
				emit.Status("answering …")
			}
			if think != "" {
				if !thinking {
					emit(Event{Type: "text", Text: "<think>"})
					thinking = true
				}
				emit(Event{Type: "text", Text: think})
			}
			if ch.Delta.Content != "" {
				if thinking {
					emit(Event{Type: "text", Text: "</think>\n\n"})
					thinking = false
				}
				emit(Event{Type: "text", Text: ch.Delta.Content})
			}
		}
	}
	if thinking {
		emit(Event{Type: "text", Text: "</think>"})
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", m.served.Name, err)
	}
	total := time.Since(start)
	st := map[string]any{"promptTokens": promptTokens, "tokens": tokens, "firstTokenSeconds": first.Seconds(), "runtime": m.served.Name}
	if gen := (total - first).Seconds(); tokens > 0 && gen > 0 {
		st["tokensPerSecond"] = float64(tokens) / gen
	}
	emit(Event{Type: "stats", Data: st})
	emit.Log(fmt.Sprintf("%s: %d prompt tokens, %d tokens in %.1f s", m.served.Name, promptTokens, tokens, total.Seconds()))
	if len(r.Tools) > 0 {
		var made []llm.ToolCall
		for i, c := range calls {
			if c.Name == "" {
				continue
			}
			if c.ID == "" {
				c.ID = fmt.Sprintf("call_%d", i)
			}
			if strings.TrimSpace(c.Arguments) == "" {
				c.Arguments = "{}"
			}
			made = append(made, c)
		}
		emit(toolCallsResult(made))
	}
	return nil
}

// ---------- embed ----------

// Embed returns one vector per text.
func (b *OpenAIBackend) Embed(ctx context.Context, m *Model, texts []string) ([][]float32, error) {
	if m.served == nil || m.Kind != Embed {
		return nil, fmt.Errorf("%s: not an embedding model of a local runtime", m.ID)
	}
	resp, err := b.post(ctx, m.served.Runtime, "/embeddings", map[string]any{"model": m.served.Model, "input": texts})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: %w", m.served.Name, err)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("%s: %d vectors for %d texts", m.served.Name, len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for i, d := range out.Data {
		j := d.Index
		if j < 0 || j >= len(vecs) || vecs[j] != nil {
			j = i
		}
		vecs[j] = d.Embedding
	}
	return vecs, nil
}

func (b *OpenAIBackend) embed(ctx context.Context, m *Model, r Request, emit Emit) error {
	if len(r.Texts) == 0 {
		return fmt.Errorf("no texts")
	}
	emit.Status(fmt.Sprintf("embedding %d texts with %s …", len(r.Texts), m.served.Name))
	start := time.Now()
	vecs, err := b.Embed(ctx, m, r.Texts)
	if err != nil {
		return err
	}
	took := time.Since(start)
	emit(embedResult(vecs, r.Texts))
	emit(Event{Type: "stats", Data: map[string]any{"seconds": took.Seconds(), "textsPerSecond": float64(len(r.Texts)) / took.Seconds()}})
	emit.Log(fmt.Sprintf("embedded %d texts in %d ms", len(r.Texts), took.Milliseconds()))
	return nil
}

// embedResult is an embed job's result: how alike every two texts are (cosine similarity) and the vectors' length.
func embedResult(vecs [][]float32, texts []string) Event {
	sim := make([][]float64, len(vecs))
	for i := range vecs {
		sim[i] = make([]float64, len(vecs))
		for j := range vecs {
			sim[i][j] = math.Round(cosine(vecs[i], vecs[j])*1e4) / 1e4
		}
	}
	dim := 0
	if len(vecs) > 0 {
		dim = len(vecs[0])
	}
	return Event{Type: "result", Data: map[string]any{"similarity": sim, "dim": dim, "texts": texts}}
}

// ---------- HTTP ----------

func (b *OpenAIBackend) post(ctx context.Context, rt *Runtime, path string, body any) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rt.URL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if rt.Key != "" {
		req.Header.Set("Authorization", "Bearer "+rt.Key)
	}
	resp, err := b.client().Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, fmt.Errorf("%s (%s) does not answer: %w", rt.Name, rt.URL, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return nil, fmt.Errorf("%s: %s: %s", rt.Name, resp.Status, apiError(msg))
	}
	return resp, nil
}

// apiError is the message of an OpenAI-style error body ({"error":{"message"}} or {"error":"…"}), else the body.
func apiError(b []byte) string {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && len(e.Error) > 0 {
		var o struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e.Error, &o) == nil && o.Message != "" {
			return o.Message
		}
		var s string
		if json.Unmarshal(e.Error, &s) == nil && s != "" {
			return s
		}
	}
	return strings.TrimSpace(string(b))
}
