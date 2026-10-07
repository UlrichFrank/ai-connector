//go:build darwin && arm64

package agent

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/llm"
)

// engineGen is a loaded model as a Generator.
type engineGen struct{ eng *llm.Engine }

func (g engineGen) Format() string { return g.eng.Format }
func (g engineGen) Generate(ctx context.Context, prompt string, on func(string)) error {
	_, err := g.eng.Generate(ctx, prompt, llm.Options{MaxTokens: 600}, on)
	return err
}

// A real model with a fake toolbox: MLXLAB_AGENT_E2E=<model folder> (e.g. Qwen3.6-35B-A3B-4bit).
func TestRealModel(t *testing.T) {
	dir := os.Getenv("MLXLAB_AGENT_E2E")
	if dir == "" {
		t.Skip("MLXLAB_AGENT_E2E not set")
	}
	eng, err := llm.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	b := &box{
		tools: []Tool{
			{Name: "add", Description: "Adds two integers.", Parameters: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`)},
			{Name: "get_time", Description: "The current local time.", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)},
			{Name: "switch_light", Description: "Switches a light on or off.", Changes: true, Parameters: json.RawMessage(`{"type":"object","properties":{"room":{"type":"string"},"on":{"type":"boolean"}},"required":["room","on"]}`)},
		},
		result: func(name string, args json.RawMessage) (Result, error) {
			switch name {
			case "add":
				var a struct{ A, B int }
				_ = json.Unmarshal(args, &a)
				return Result{Text: strconv.Itoa(a.A + a.B)}, nil
			case "get_time":
				return Result{Text: "14:05"}, nil
			}
			return Result{Text: "ok"}, nil
		},
	}
	cfg := Config{Home: func(context.Context) ([]Toolbox, error) { return []Toolbox{b}, nil }, ApprovalTimeout: 2 * time.Second}
	var r rec
	start := time.Now()
	msgs, err := Run(context.Background(), engineGen{eng}, cfg, Request{Zone: ZoneHome,
		Messages: user("What is 123456 + 654321? Use the add tool. Also, what time is it?")}, r.emit)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%.1f s, tool events %v\nanswer: %s", time.Since(start).Seconds(), r.states(), msgs[len(msgs)-1].Content)
	if len(b.called) == 0 || !strings.Contains(strings.Join(b.called, ";"), "add") {
		t.Fatalf("add not called: %v", b.called)
	}
	if ans := msgs[len(msgs)-1].Content; !strings.Contains(strings.ReplaceAll(ans, ",", ""), "777777") {
		t.Fatalf("answer %q", ans)
	}

	// a follow-up turn on the returned history; the light needs approval, nobody gives it in time
	b.called = nil
	r = rec{}
	next := append(msgs, llm.Message{Role: "user", Content: "Switch on the kitchen light."})
	msgs2, err := Run(context.Background(), engineGen{eng}, cfg, Request{Zone: ZoneHome, Messages: next, Gate: NewGate()}, r.emit)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tool events %v\nanswer: %s", r.states(), msgs2[len(msgs2)-1].Content)
	if st := strings.Join(r.states(), ","); !strings.Contains(st, "switch_light:approval,switch_light:denied") {
		t.Fatalf("states %s", st)
	}
	if len(b.called) != 0 {
		t.Fatal("a denied call ran")
	}
}
