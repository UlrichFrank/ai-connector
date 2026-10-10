package jobs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
)

// A text event says where its piece starts in the job's text ("at"), so a follower sees a gap; other events do not.
func TestTextEventsAt(t *testing.T) {
	m := setup(t, &fake{name: "a"}, &fake{name: "b"})
	ch, stop := m.Subscribe()
	defer stop()
	j, err := m.Submit(engine.Request{Model: "chat-a", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var ats []int
	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-ch:
			if u.JobID != j.ID {
				continue
			}
			if u.Event != nil && u.Event.Type == "text" {
				if u.At == nil {
					t.Fatalf("text without at")
				}
				ats = append(ats, *u.At)
				if b, _ := json.Marshal(u); !strings.Contains(string(b), `"at":`) {
					t.Fatalf("on the wire %s", b)
				}
			} else if u.At != nil {
				t.Fatalf("at on %+v", u)
			}
			if u.Job != nil && !u.Job.Active() {
				if len(ats) != 2 || ats[0] != 0 || ats[1] != 1 {
					t.Fatalf("at %v", ats)
				}
				return
			}
		case <-deadline:
			t.Fatalf("no end (at %v)", ats)
		}
	}
}
