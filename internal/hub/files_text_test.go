package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A chat answer that mentions /files/ paths reaches an OpenAI client as the model wrote it, streamed and not: the
// worker's route layer maps file urls only, never text.
func TestChatTextKeepsFilesPaths(t *testing.T) {
	defer func(d time.Duration) { chatPoll = d }(chatPoll)
	chatPoll = 200 * time.Millisecond
	_, srv := newHub(t, t.TempDir())
	base := srv.URL
	device := signIn(t, base, "laptop")
	_, token := makeClient(t, base, device, "app")
	_, wtok := enroll(t, base, device, "Mac")
	startAPIMac(t, base, wtok)
	answer := "Put it in /files/x.png, see [the doc](/files/docs/y.pdf)."

	c, b := post(t, base+"/v1/chat/completions", token, `{"model":"qwen3.6","messages":[{"role":"user","content":"say:`+answer+`"}]}`)
	var res struct {
		ID      string
		Choices []struct{ Message struct{ Content string } }
	}
	json.Unmarshal(b, &res)
	if c != 200 || res.Choices[0].Message.Content != answer {
		t.Fatalf("not streamed %d %s", c, b)
	}
	// the job's text too
	if j := getJob(t, base, token, res.ID); !strings.Contains(string(mustJSON(j)), "/files/x.png") {
		t.Fatalf("job %+v", j)
	}

	r := do(t, "POST", base+"/v1/chat/completions", token, strings.NewReader(`{"model":"qwen3.6","stream":true,"messages":[{"role":"user","content":"say:`+answer+`"}]}`))
	_, data := sse(t, r)
	var content strings.Builder
	for _, d := range data[:len(data)-1] {
		var ch struct {
			Choices []struct{ Delta struct{ Content string } }
		}
		json.Unmarshal([]byte(d), &ch)
		if len(ch.Choices) > 0 {
			content.WriteString(ch.Choices[0].Delta.Content)
		}
	}
	if content.String() != answer {
		t.Fatalf("streamed %q", content.String())
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
