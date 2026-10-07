package llm

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// testdata/prompts.json: a conversation with tools, tool calls (one with two calls) and tool results, rendered by the
// models' own Jinja templates (Qwen3.6's chat_template.jinja, Qwen3-8B's) with transformers' renderer.
func TestRenderMatchesTemplates(t *testing.T) {
	raw, err := os.ReadFile("testdata/prompts.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Tools []struct {
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Rendered map[string]string `json:"rendered"`
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	var tools []Tool
	for _, x := range ref.Tools {
		tools = append(tools, Tool{Name: x.Function.Name, Description: x.Function.Description, Parameters: x.Function.Parameters})
	}
	var msgs []Message
	for _, m := range ref.Messages {
		msg := Message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, c := range m.ToolCalls {
			// vboard sends the arguments as a JSON string (compact)
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: compact(t, c.Function.Arguments)})
		}
		msgs = append(msgs, msg)
	}
	for _, c := range []struct {
		key, format string
		think       bool
		msgs        []Message
		tools       []Tool
	}{
		{"qwen35_nothink", "xml", false, msgs, tools},
		{"qwen35_think", "xml", true, msgs, tools},
		{"qwen35_notools", "xml", false, msgs[:2], nil},
		{"qwen3_nothink", "json", false, msgs, tools},
		{"qwen3_think", "json", true, msgs, tools},
		{"qwen3_notools", "json", false, msgs[:2], nil},
	} {
		got, want := Render(c.format, c.msgs, c.tools, c.think), ref.Rendered[c.key]
		if got != want {
			i := 0
			for i < len(got) && i < len(want) && got[i] == want[i] {
				i++
			}
			t.Errorf("%s differs at %d:\n got …%q\nwant …%q", c.key, i, clip(got, i), clip(want, i))
		}
	}
}

func compact(t *testing.T, raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func clip(s string, i int) string {
	from := max(0, i-40)
	to := min(len(s), i+80)
	return s[from:to]
}
