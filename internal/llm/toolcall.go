package llm

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

var (
	jsonObject = regexp.MustCompile(`(?s)\{.*\}`)
	xmlFunc    = regexp.MustCompile(`(?s)<function=([^>\n]+)>(.*?)(?:</function>|$)`)
	xmlParam   = regexp.MustCompile(`(?s)<parameter=([^>\n]+)>\n?(.*?)\n?</parameter>`)
)

// ParseToolCall reads the body of a <tool_call> block: Qwen3's JSON ({"name": …, "arguments": {…}}) or Qwen3.5's
// XML (<function=name><parameter=p>value</parameter></function>). The arguments come back as a JSON object; XML
// values are typed by the tool's schema (a string stays a string, a number/array/object/boolean is read as JSON).
func ParseToolCall(body string, tools []Tool) (name, args string, ok bool) {
	if m := xmlFunc.FindStringSubmatch(body); m != nil {
		name = strings.TrimSpace(m[1])
		types := paramTypes(tools, name)
		var b bytes.Buffer
		b.WriteByte('{')
		for i, p := range xmlParam.FindAllStringSubmatch(m[2], -1) {
			if i > 0 {
				b.WriteByte(',')
			}
			key, _ := json.Marshal(strings.TrimSpace(p[1]))
			b.Write(key)
			b.WriteByte(':')
			b.Write(typedValue(p[2], types[strings.TrimSpace(p[1])]))
		}
		b.WriteByte('}')
		return name, b.String(), name != ""
	}
	var c struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal([]byte(jsonObject.FindString(body)), &c) != nil || c.Name == "" {
		// no name to read (e.g. "```json {updates: …}" or a broken <function=…>): the tool whose parameters the
		// object's keys are
		return guessTool(body, tools)
	}
	args = strings.TrimSpace(string(c.Arguments))
	var s string
	if json.Unmarshal(c.Arguments, &s) == nil {
		args = s // arguments written as a JSON string
	}
	if args == "" || args == "null" {
		args = "{}"
	}
	return c.Name, args, true
}

// paramTypes maps a tool's parameters to their JSON-schema types.
func paramTypes(tools []Tool, name string) map[string]string {
	out := map[string]string{}
	for _, t := range tools {
		if t.Name != name {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Type any `json:"type"`
			} `json:"properties"`
		}
		if json.Unmarshal(t.Parameters, &schema) != nil {
			break
		}
		for p, d := range schema.Properties {
			switch v := d.Type.(type) {
			case string:
				out[p] = v
			case []any: // ["string", "null"]: the first that is not null
				for _, x := range v {
					if s, ok := x.(string); ok && s != "null" {
						out[p] = s
						break
					}
				}
			}
		}
	}
	return out
}

// typedValue is an XML parameter value as JSON.
func typedValue(v, typ string) []byte {
	trimmed := strings.TrimSpace(v)
	if typ != "string" {
		var x any
		if trimmed != "" && json.Unmarshal([]byte(trimmed), &x) == nil {
			// a value that is JSON: a number, array, object, true/false/null (for an untyped parameter only the
			// non-string ones: "hello" without quotes is not JSON anyway)
			if _, isStr := x.(string); !isStr || typ == "" {
				var b bytes.Buffer
				if json.Compact(&b, []byte(trimmed)) == nil {
					return b.Bytes()
				}
			}
		}
		switch typ {
		case "boolean":
			switch strings.ToLower(trimmed) {
			case "true", "yes":
				return []byte("true")
			case "false", "no":
				return []byte("false")
			}
		case "array":
			// a list written one item per line
			var items []string
			for _, l := range strings.Split(trimmed, "\n") {
				if l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "- ")); l != "" {
					items = append(items, l)
				}
			}
			if len(items) > 0 {
				b, _ := json.Marshal(items)
				return b
			}
		}
	}
	b, _ := json.Marshal(v)
	return b
}

// guessTool takes a bare JSON object as the arguments of the one tool that fits its keys: all its required parameters
// are there and every key is one of its parameters. Two tools that fit (or none): not guessed.
func guessTool(body string, tools []Tool) (string, string, bool) {
	var obj map[string]json.RawMessage
	raw := jsonObject.FindString(body)
	if raw == "" {
		return "", "", false
	}
	// a trailing "</parameter></function>" after the object: the regex took up to the last brace, which is right
	if json.Unmarshal([]byte(raw), &obj) != nil || len(obj) == 0 {
		return "", "", false
	}
	found := ""
	for _, t := range tools {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if json.Unmarshal(t.Parameters, &schema) != nil || len(schema.Properties) == 0 {
			continue
		}
		fits := true
		for k := range obj {
			if _, ok := schema.Properties[k]; !ok {
				fits = false
			}
		}
		for _, r := range schema.Required {
			if _, ok := obj[r]; !ok {
				fits = false
			}
		}
		if !fits {
			continue
		}
		if found != "" {
			return "", "", false // ambiguous
		}
		found = t.Name
	}
	if found == "" {
		return "", "", false
	}
	return found, raw, true
}
