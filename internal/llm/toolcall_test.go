package llm

import (
	"encoding/json"
	"testing"
)

func TestParseToolCall(t *testing.T) {
	tools := []Tool{
		{Name: "stack_items", Parameters: json.RawMessage(`{"type":"object","properties":{"ids":{"type":"array"},"gap":{"type":"number"},"label":{"type":"string"},"dry":{"type":"boolean"},"note":{"type":["string","null"]}}}`)},
	}
	for _, c := range []struct{ body, name, args string }{
		// Qwen3.5/3.6 XML
		{"\n<function=stack_items>\n<parameter=ids>\n[\"a\", \"b\"]\n</parameter>\n<parameter=gap>\n10\n</parameter>\n</function>\n",
			"stack_items", `{"ids":["a","b"],"gap":10}`},
		// a string parameter stays a string, even when it looks like JSON; multi-line values keep their lines
		{"<function=stack_items>\n<parameter=label>\n42\n</parameter>\n<parameter=note>\nline 1\nline 2\n</parameter>\n</function>",
			"stack_items", `{"label":"42","note":"line 1\nline 2"}`},
		// booleans written loosely, an array one item per line
		{"<function=stack_items>\n<parameter=dry>\nTrue\n</parameter>\n<parameter=ids>\nn1\nn2\n</parameter>\n</function>",
			"stack_items", `{"dry":true,"ids":["n1","n2"]}`},
		// an unknown tool: JSON values are read as JSON, the rest as text
		{"<function=other>\n<parameter=n>\n3\n</parameter>\n<parameter=s>\nhello\n</parameter>\n</function>",
			"other", `{"n":3,"s":"hello"}`},
		// cut off before </function>
		{"<function=stack_items>\n<parameter=gap>\n5\n</parameter>\n", "stack_items", `{"gap":5}`},
		// Qwen3 JSON
		{"\n{\"name\": \"stack_items\", \"arguments\": {\"ids\": [\"a\"]}}\n", "stack_items", `{"ids": ["a"]}`},
		{`{"name": "stack_items", "arguments": "{\"ids\":[\"a\"]}"}`, "stack_items", `{"ids":["a"]}`},
		{`{"name": "stack_items"}`, "stack_items", `{}`},
	} {
		name, args, ok := ParseToolCall(c.body, tools)
		if !ok || name != c.name || args != c.args {
			t.Errorf("%q: %q %s %v, want %q %s", c.body, name, args, ok, c.name, c.args)
		}
		if !json.Valid([]byte(args)) {
			t.Errorf("%q: invalid JSON %s", c.body, args)
		}
	}
	for _, bad := range []string{"", "no call", `{"arguments": {}}`} {
		if _, _, ok := ParseToolCall(bad, tools); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}
