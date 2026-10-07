package engine

import (
	"encoding/json"
	"testing"

	"github.com/philipparndt/ai-connector/internal/llm"
)

// A chat takes the model's settings for thinking on or off; what the request sets wins.
func TestChatOptions(t *testing.T) {
	d := llm.ModelDefaults(&llm.Config{ModelType: "qwen3_5_moe"}, llm.GenerationConfig{})
	if o := chatOptions(d, Request{Think: true}); o != d.Think || o.PresencePenalty != 1.5 || o.MaxTokens != llm.ThinkMaxTokens {
		t.Errorf("thinking %+v", o)
	}
	if o := chatOptions(d, Request{}); o != d.NoThink {
		t.Errorf("no thinking %+v", o)
	}
	// the iOS app and older clients send a temperature and a budget: only those change
	o := chatOptions(d, Request{Think: true, Temperature: 0.3, MaxTokens: 4096})
	if o.Temperature != 0.3 || o.MaxTokens != 4096 || o.TopP != 0.95 || o.PresencePenalty != 1.5 {
		t.Errorf("old client %+v", o)
	}
	// JSON: 0 is not set; greedy and a penalty of 0 are explicit
	var r Request
	if err := json.Unmarshal([]byte(`{"temperature": 0, "greedy": true, "presencePenalty": 0, "topP": 1, "topK": 40, "think": true}`), &r); err != nil {
		t.Fatal(err)
	}
	o = chatOptions(d, r)
	if o.Temperature != 0 || o.PresencePenalty != 0 || o.TopP != 1 || o.TopK != 40 {
		t.Errorf("explicit %+v", o)
	}
}
