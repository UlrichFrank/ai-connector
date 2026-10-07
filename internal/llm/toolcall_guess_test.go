package llm

import (
	"encoding/json"
	"testing"
)

// A call without a readable name is taken as the one tool its keys fit (the log: "json\n{updates: …}</parameter>").
func TestGuessTool(t *testing.T) {
	tools := []Tool{
		{Name: "update_items", Parameters: json.RawMessage(`{"type":"object","properties":{"updates":{"type":"array"}},"required":["updates"]}`)},
		{Name: "delete_items", Parameters: json.RawMessage(`{"type":"object","properties":{"ids":{"type":"array"},"withContent":{"type":"boolean"}},"required":["ids"]}`)},
		{Name: "list_items", Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"type":{"type":"string"}}}`)},
	}
	name, args, ok := ParseToolCall("json\n{\n  \"updates\": [{\"id\": \"i_1\", \"text\": \"a}b\"}]\n}\n</parameter>\n</function>", tools)
	if !ok || name != "update_items" || args != "{\n  \"updates\": [{\"id\": \"i_1\", \"text\": \"a}b\"}]\n}" {
		t.Fatalf("%v %q %q", ok, name, args)
	}
	if _, _, ok := ParseToolCall(`{"foo": 1}`, tools); ok {
		t.Fatal("unknown keys guessed")
	}
	// {} fits list_items only if no other tool without required params exists; {"text": "x"} fits list_items alone
	if n, _, ok := ParseToolCall(`{"text": "x"}`, tools); !ok || n != "list_items" {
		t.Fatalf("list_items: %v %q", ok, n)
	}
}
