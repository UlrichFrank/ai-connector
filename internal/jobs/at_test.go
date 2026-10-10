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

// A subscriber that does not keep up misses events, never a job's state: an older event makes room for it, and the
// order stays.
func TestSlowSubscriberGetsStates(t *testing.T) {
	m := setup(t, &fake{name: "a"})
	ch, stop := m.Subscribe()
	defer stop()
	m.publish(Update{JobID: "j", Job: &Job{ID: "j", State: Running}})
	for i := range subBuffer + 10 {
		m.publish(Update{JobID: "j", Event: &engine.Event{Type: "text", Text: "x"}, At: &i})
	}
	m.publish(Update{JobID: "j", Job: &Job{ID: "j", State: Done}})
	if len(ch) != subBuffer {
		t.Fatalf("%d kept", len(ch))
	}
	var got []Update
	for len(ch) > 0 {
		got = append(got, <-ch)
	}
	if got[0].Job == nil || got[0].Job.State != Running {
		t.Fatalf("first %+v", got[0])
	}
	if last := got[len(got)-1]; last.Job == nil || last.Job.State != Done {
		t.Fatalf("last %+v", last)
	}
	// the oldest event made room: the rest are in order
	for i, u := range got[1 : len(got)-1] {
		if *u.At != i+1 {
			t.Fatalf("event %d at %d", i, *u.At)
		}
	}
}
