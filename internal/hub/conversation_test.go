package hub

import (
	"strings"
	"testing"
)

// vboard's conversation id keys an agent's session: two conversations that open with the same words at the same
// time get their own sessions, and fresh holds only the first time an id is seen. Without an id (older vboards) the
// first message's hash keys it, as before.
func TestAgentConversationID(t *testing.T) {
	conf := storedConnector{ID: "c1", URL: "https://vb", Token: "vbp_x"}
	var seen seenConversations
	job := func(conv string, msgs ...bridgeChatMsg) map[string]any {
		t.Helper()
		cx := &bridgeContext{BoardID: "b1", UserID: "u1", ConversationID: conv}
		j, err := agentJob(conf, &bridgeMsg{Messages: msgs, Context: cx}, seen.seen)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	hi := bridgeChatMsg{Role: "user", Content: "add a sticky"}

	a := job("conv-a", hi)
	b := job("conv-b", hi)
	if a["session"] != "vboard:c1:u1:b1:conv-a" || b["session"] != "vboard:c1:u1:b1:conv-b" {
		t.Fatalf("sessions %v %v", a["session"], b["session"])
	}
	if a["fresh"] != true || b["fresh"] != true {
		t.Fatalf("new conversations are fresh: %v %v", a["fresh"], b["fresh"])
	}
	// the same first question again in conversation a (its first answer failed, nothing answered): its session stays
	if again := job("conv-a", hi); again["fresh"] != false || again["session"] != a["session"] {
		t.Fatalf("a retry %v", again)
	}
	// a follow-up
	if f := job("conv-a", hi, bridgeChatMsg{Role: "assistant", Content: "done"}, bridgeChatMsg{Role: "user", Content: "thanks"}); f["fresh"] != false || f["session"] != a["session"] {
		t.Fatalf("follow-up %v", f)
	}

	// no id: the hash of the first message, fresh while nothing is answered
	old1, old2 := job("", hi), job("", hi)
	if old1["session"] != old2["session"] || !strings.HasPrefix(old1["session"].(string), "vboard:c1:u1:b1:") ||
		len(strings.TrimPrefix(old1["session"].(string), "vboard:c1:u1:b1:")) != 12 || old1["fresh"] != true || old2["fresh"] != true {
		t.Fatalf("without an id %v %v", old1, old2)
	}

	// an id that is long or has spaces is hashed
	long := job(strings.Repeat("x", 200), hi)["session"].(string)
	spaced := job("a b", hi)["session"].(string)
	if !strings.HasPrefix(long, "vboard:c1:u1:b1:c") || len(long) != len("vboard:c1:u1:b1:")+25 || strings.Contains(spaced, " ") {
		t.Fatalf("hashed ids %q %q", long, spaced)
	}
}
