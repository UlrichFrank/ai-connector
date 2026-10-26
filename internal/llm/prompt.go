package llm

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/philipparndt/ai-connector/internal/pyjson"
)

// Message is one message of the conversation (as vboard sends it: backend/internal/api/chat.go).
type Message struct {
	Role       string // system | user | assistant | tool
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	// Images: images of a user message, named in Content by their tags ("[image-1]"; image.go). Only models with
	// the capability get them (engine.Model.Images).
	Images []Image `json:"images,omitempty"`
}

// ToolCall is a call of the model; Arguments is a JSON object.
type ToolCall struct {
	ID, Name, Arguments string
}

// Tool is a function the model may call; Parameters is its JSON schema.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Render writes the conversation the way the model's chat template does (Qwen3's with format "json", Qwen3.5/3.6's
// with "xml"), followed by the start of the assistant's answer. think=false opens the answer with an empty thought
// (enable_thinking=false): the model answers at once.
func Render(format string, msgs []Message, tools []Tool, think bool) string {
	if format == "xml" {
		return renderQwen35(msgs, tools, think)
	}
	return renderQwen3(msgs, tools, think)
}

// toolJSON is a tool as the template's `tool | tojson` writes it (the OpenAI form).
func toolJSON(t Tool) string {
	params := "{}"
	if len(bytes.TrimSpace(t.Parameters)) > 0 {
		if p, err := pyjson.Marshal(t.Parameters); err == nil {
			params = p
		}
	}
	return `{"type": "function", "function": {"name": ` + pyjson.String(t.Name) + `, "description": ` + pyjson.String(t.Description) +
		`, "parameters": ` + params + `}}`
}

// lastQuery is the index of the last user message that is not a tool response (the templates' ns.last_query_index).
func lastQuery(msgs []Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == "user" {
			c := strings.TrimSpace(m.Content)
			if !(strings.HasPrefix(c, "<tool_response>") && strings.HasSuffix(c, "</tool_response>")) {
				return i
			}
		}
	}
	return len(msgs) - 1
}

// splitThought takes a thought written into the content (<think>…</think>) out of it.
func splitThought(content string) (reasoning, rest string) {
	if i := strings.Index(content, "</think>"); i >= 0 {
		r := strings.TrimRight(content[:i], "\n")
		if j := strings.LastIndex(r, "<think>"); j >= 0 {
			r = r[j+len("<think>"):]
		}
		return strings.TrimLeft(r, "\n"), strings.TrimLeft(content[i+len("</think>"):], "\n")
	}
	return "", content
}

// argsInOrder reads a JSON object's members in their order (nil: not an object).
func argsInOrder(args string) [][2]json.RawMessage {
	dec := json.NewDecoder(strings.NewReader(args))
	dec.UseNumber()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var out [][2]json.RawMessage
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil
		}
		ks, _ := k.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil
		}
		kb, _ := json.Marshal(ks)
		out = append(out, [2]json.RawMessage{kb, v})
	}
	return out
}

func renderQwen35(msgs []Message, tools []Tool, think bool) string {
	var b strings.Builder
	sys := ""
	if len(msgs) > 0 && msgs[0].Role == "system" {
		sys = strings.TrimSpace(msgs[0].Content)
	}
	if len(tools) > 0 {
		b.WriteString("<|im_start|>system\n# Tools\n\nYou have access to the following functions:\n\n<tools>")
		for _, t := range tools {
			b.WriteString("\n" + toolJSON(t))
		}
		b.WriteString("\n</tools>")
		b.WriteString("\n\nIf you choose to call a function ONLY reply in the following format with NO suffix:\n\n<tool_call>\n<function=example_function_name>\n<parameter=example_parameter_1>\nvalue_1\n</parameter>\n<parameter=example_parameter_2>\nThis is the value for the second parameter\nthat can span\nmultiple lines\n</parameter>\n</function>\n</tool_call>\n\n<IMPORTANT>\nReminder:\n- Function calls MUST follow the specified format: an inner <function=...></function> block must be nested within <tool_call></tool_call> XML tags\n- Required parameters MUST be specified\n- You may provide optional reasoning for your function call in natural language BEFORE the function call, but NOT after\n- If there is no function call available, answer the question like normal with your current knowledge and do not tell the user about function calls\n</IMPORTANT>")
		if sys != "" {
			b.WriteString("\n\n" + sys)
		}
		b.WriteString("<|im_end|>\n")
	} else if len(msgs) > 0 && msgs[0].Role == "system" {
		b.WriteString("<|im_start|>system\n" + sys + "<|im_end|>\n")
	}
	lq := lastQuery(msgs)
	for i, m := range msgs {
		content := strings.TrimSpace(m.Content)
		switch m.Role {
		case "system":
			// only as the first message (above)
		case "user":
			// images: a placeholder after each tag (the engine runs them when the model has a vision tower)
			b.WriteString("<|im_start|>user\n" + strings.TrimSpace(userContent(m)) + "<|im_end|>\n")
		case "assistant":
			reasoning, rest := splitThought(content)
			reasoning = strings.TrimSpace(reasoning)
			if i > lq {
				b.WriteString("<|im_start|>assistant\n<think>\n" + reasoning + "\n</think>\n\n" + rest)
			} else {
				b.WriteString("<|im_start|>assistant\n" + rest)
			}
			for k, tc := range m.ToolCalls {
				switch {
				case k > 0:
					b.WriteString("\n<tool_call>\n<function=" + tc.Name + ">\n")
				case strings.TrimSpace(rest) != "":
					b.WriteString("\n\n<tool_call>\n<function=" + tc.Name + ">\n")
				default:
					b.WriteString("<tool_call>\n<function=" + tc.Name + ">\n")
				}
				for _, kv := range argsInOrder(tc.Arguments) {
					var name string
					_ = json.Unmarshal(kv[0], &name)
					b.WriteString("<parameter=" + name + ">\n")
					var s string
					if json.Unmarshal(kv[1], &s) == nil {
						b.WriteString(s)
					} else if v, err := pyjson.Marshal(kv[1]); err == nil {
						b.WriteString(v)
					}
					b.WriteString("\n</parameter>\n")
				}
				b.WriteString("</function>\n</tool_call>")
			}
			b.WriteString("<|im_end|>\n")
		case "tool":
			if i == 0 || msgs[i-1].Role != "tool" {
				b.WriteString("<|im_start|>user")
			}
			b.WriteString("\n<tool_response>\n" + content + "\n</tool_response>")
			if i == len(msgs)-1 || msgs[i+1].Role != "tool" {
				b.WriteString("<|im_end|>\n")
			}
		}
	}
	b.WriteString("<|im_start|>assistant\n")
	if !think {
		b.WriteString("<think>\n\n</think>\n\n")
	} else {
		b.WriteString("<think>\n")
	}
	return b.String()
}

func renderQwen3(msgs []Message, tools []Tool, think bool) string {
	var b strings.Builder
	if len(tools) > 0 {
		b.WriteString("<|im_start|>system\n")
		if len(msgs) > 0 && msgs[0].Role == "system" {
			b.WriteString(msgs[0].Content + "\n\n")
		}
		b.WriteString("# Tools\n\nYou may call one or more functions to assist with the user query.\n\nYou are provided with function signatures within <tools></tools> XML tags:\n<tools>")
		for _, t := range tools {
			b.WriteString("\n" + toolJSON(t))
		}
		b.WriteString("\n</tools>\n\nFor each function call, return a json object with function name and arguments within <tool_call></tool_call> XML tags:\n<tool_call>\n{\"name\": <function-name>, \"arguments\": <args-json-object>}\n</tool_call><|im_end|>\n")
	} else if len(msgs) > 0 && msgs[0].Role == "system" {
		b.WriteString("<|im_start|>system\n" + msgs[0].Content + "<|im_end|>\n")
	}
	lq := lastQuery(msgs)
	for i, m := range msgs {
		switch m.Role {
		case "system":
		case "user":
			b.WriteString("<|im_start|>user\n" + m.Content + "<|im_end|>\n")
		case "assistant":
			reasoning, content := splitThought(m.Content)
			if i > lq && (i == len(msgs)-1 || reasoning != "") {
				b.WriteString("<|im_start|>assistant\n<think>\n" + strings.Trim(reasoning, "\n") + "\n</think>\n\n" + strings.TrimLeft(content, "\n"))
			} else {
				b.WriteString("<|im_start|>assistant\n" + content)
			}
			for k, tc := range m.ToolCalls {
				if k > 0 || content != "" {
					b.WriteString("\n")
				}
				args := strings.TrimSpace(tc.Arguments)
				if v, err := pyjson.Marshal(json.RawMessage(args)); err == nil {
					args = v
				} else if args == "" {
					args = "{}"
				}
				b.WriteString("<tool_call>\n{\"name\": \"" + tc.Name + "\", \"arguments\": " + args + "}\n</tool_call>")
			}
			b.WriteString("<|im_end|>\n")
		case "tool":
			if i == 0 || msgs[i-1].Role != "tool" {
				b.WriteString("<|im_start|>user")
			}
			b.WriteString("\n<tool_response>\n" + m.Content + "\n</tool_response>")
			if i == len(msgs)-1 || msgs[i+1].Role != "tool" {
				b.WriteString("<|im_end|>\n")
			}
		}
	}
	b.WriteString("<|im_start|>assistant\n")
	if !think {
		b.WriteString("<think>\n\n</think>\n\n")
	}
	return b.String()
}
