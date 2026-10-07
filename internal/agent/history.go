package agent

import (
	"fmt"

	"github.com/philipparndt/ai-connector/internal/llm"
)

// Valid says the zone is one of the known ones.
func (z Zone) Valid() bool {
	switch z {
	case ZoneNone, ZoneHome, ZoneWeb, ZoneFiles:
		return true
	}
	return false
}

func (z Zone) words() string {
	if z == ZoneNone {
		return "chat without tools"
	}
	return string(z) + " chat"
}

// Parent is the turn a follow-up continues: its zone and the history the worker returned (result.messages).
type Parent struct {
	Zone     Zone
	Messages []llm.Message
}

// FollowUp checks a turn before it is accepted and says which zone it runs in. The zone of a follow-up is its
// parent's (an empty zone takes it, another one is refused: a web chat stays a web chat), and every tool turn in its
// history must be one the worker itself returned with the parent — a client cannot carry tool results (web
// content) into a chat, it can only continue a conversation that already had them in the same zone.
// parent nil: a new conversation (no tool turns allowed).
func FollowUp(zone Zone, msgs []llm.Message, parent *Parent) (Zone, error) {
	if !zone.Valid() {
		return zone, fmt.Errorf("unknown zone %q (home, web, files)", zone)
	}
	results := map[string]string{} // tool call id → the result the worker gave the model
	calls := map[string]bool{}
	if parent != nil {
		if zone == ZoneNone {
			zone = parent.Zone
		} else if zone != parent.Zone {
			return zone, fmt.Errorf("a %s stays a %s", parent.Zone.words(), parent.Zone.words())
		}
		for _, m := range parent.Messages {
			if m.Role == "tool" && m.ToolCallID != "" {
				results[m.ToolCallID] = m.Content
			}
			for _, c := range m.ToolCalls {
				if c.ID != "" {
					calls[c.ID] = true
				}
			}
		}
	}
	for _, m := range msgs {
		if m.Role == "tool" {
			r, ok := results[m.ToolCallID]
			if !ok || r != m.Content {
				return zone, fmt.Errorf("a tool result the worker did not give in this conversation (%q): tool results only come from the chat that made them", m.ToolCallID)
			}
		}
		for _, c := range m.ToolCalls {
			if !calls[c.ID] {
				return zone, fmt.Errorf("a tool call the worker did not make in this conversation (%q)", c.Name)
			}
		}
	}
	return zone, nil
}
