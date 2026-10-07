package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/llm"
)

// The model learns the date: added to the system prompt, replaced (not repeated) on the next turn.
func TestWithNow(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	d1 := time.Date(2026, 10, 4, 8, 15, 0, 0, berlin)
	got := withNow([]llm.Message{{Role: "user", Content: "Wann fährt morgen ein Zug?"}}, d1)
	if len(got) != 2 || got[0].Role != "system" || !strings.Contains(got[0].Content, "Sunday, 4 October 2026, 08:15 (Europe/Berlin, UTC+02:00)") {
		t.Fatalf("no date: %+v", got)
	}
	// the next turn carries the history: one date line, the new one
	next := withNow(append(got, llm.Message{Role: "assistant", Content: "…"}, llm.Message{Role: "user", Content: "und übermorgen?"}), d1.Add(26*time.Hour))
	if c := next[0].Content; strings.Count(c, nowPrefix) != 1 || !strings.Contains(c, "Monday, 5 October 2026, 10:15") {
		t.Fatalf("old date kept: %q", c)
	}
	// a system prompt of its own keeps its text
	sys := withNow([]llm.Message{{Role: "system", Content: "Be brief."}, {Role: "user", Content: "hi"}}, d1)
	if !strings.HasPrefix(sys[0].Content, "Be brief.\n\n"+nowPrefix) || len(sys) != 2 {
		t.Fatalf("system prompt: %q", sys[0].Content)
	}
}
