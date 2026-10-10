package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A job feed keeps every event of its job however slowly they are taken (the fleet's broadcast drops for a slow
// subscriber), and before the job is named it keeps everything, then only the job's.
func TestJobFeedKeepsItsJob(t *testing.T) {
	f := &fleet{subs: map[int]chan []byte{}}
	feed := f.followJob()
	defer feed.close()
	slow, stop := f.subscribe()
	defer stop()
	f.broadcast([]byte(`{"jobId":"m1.a","job":{"state":"queued"}}`))
	f.broadcast([]byte(`{"jobId":"m1.b","job":{"state":"queued"}}`))
	feed.setJob("m1.a")
	for i := range 5000 {
		f.broadcast(fmt.Appendf(nil, `{"jobId":"m1.a","event":{"type":"text","text":"x"},"at":%d}`, i))
		f.broadcast([]byte(`{"jobId":"m1.b","event":{"type":"text","text":"y"}}`))
	}
	got := feed.take()
	if len(got) != 5001 || !strings.Contains(string(got[0]), `"queued"`) || !strings.Contains(string(got[5000]), `"at":4999`) {
		t.Fatalf("feed: %d events", len(got))
	}
	for _, d := range got {
		if strings.Contains(string(d), "m1.b") {
			t.Fatalf("another job's event %s", d)
		}
	}
	if len(slow) != cap(slow) { // the broadcast subscriber dropped what did not fit
		t.Fatalf("subscriber has %d", len(slow))
	}
	if more := feed.take(); len(more) != 0 {
		t.Fatalf("taken twice: %d", len(more))
	}
}

// The text is passed on where it belongs: in order, a piece seen again is not repeated, a gap is filled from the
// job; a gap that cannot be filled stops the stream (the end sends the rest), never joining pieces wrongly.
func TestTextTrack(t *testing.T) {
	at := func(n int) *int { return &n }
	job := "Hello, wide world!"
	var out strings.Builder
	tr := &textTrack{out: func(s string) { out.WriteString(s) }, fetch: func() (string, error) { return job[:12], nil }}
	tr.piece(at(0), "Hel")
	tr.piece(at(3), "lo,")
	tr.piece(at(9), "de ") // 6..9 (" wi") was dropped: filled from the job's text so far ("Hello, wide ")
	tr.piece(at(12), "wor")
	tr.piece(at(9), "de ") // again: nothing
	tr.piece(at(15), "ld!")
	if out.String() != job {
		t.Fatalf("passed on %q", out.String())
	}

	out.Reset()
	tr = &textTrack{out: func(s string) { out.WriteString(s) }, fetch: func() (string, error) { return "", errors.New("gone") }}
	tr.piece(at(0), "Hel")
	tr.piece(at(6), " wi")
	tr.piece(at(9), "de ")
	if out.String() != "Hel" || !tr.stopped {
		t.Fatalf("unfilled gap %q", out.String())
	}

	out.Reset() // an older worker: no "at", as they come
	tr = &textTrack{out: func(s string) { out.WriteString(s) }}
	tr.piece(nil, "a")
	tr.piece(nil, "b")
	if out.String() != "ab" {
		t.Fatalf("without at %q", out.String())
	}
}

// A client that reads slowly gets the whole answer, in order, while the worker writes thousands of pieces faster
// than it reads (they would overflow a broadcast subscriber).
func TestChatSlowConsumer(t *testing.T) {
	defer func(d time.Duration) { chatPoll = d }(chatPoll)
	chatPoll = 2 * time.Second // the event stream, the poll only finds the end
	_, srv := newHub(t, t.TempDir())
	base := srv.URL
	device := signIn(t, base, "laptop")
	_, token := makeClient(t, base, device, "app")
	_, wtok := enroll(t, base, device, "Mac")
	startAPIMac(t, base, wtok)
	var long strings.Builder
	for i := 0; long.Len() < 120000; i++ {
		fmt.Fprintf(&long, "w%d ", i) // ~30000 pieces of 3 bytes
	}
	want := long.String()

	r := do(t, "POST", base+"/v1/chat/completions", token, strings.NewReader(`{"model":"qwen3.6","stream":true,"messages":[{"role":"user","content":"say:`+want+`"}]}`))
	defer r.Body.Close()
	br := bufio.NewReader(r.Body)
	var content strings.Builder
	n := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		d, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok || d == "[DONE]" {
			continue
		}
		if n++; n == 1 {
			time.Sleep(time.Second) // slow at first: the worker is far ahead meanwhile
		}
		var ch struct {
			Choices []struct{ Delta struct{ Content string } }
		}
		json.Unmarshal([]byte(d), &ch)
		if len(ch.Choices) > 0 {
			content.WriteString(ch.Choices[0].Delta.Content)
		}
	}
	if content.String() != want {
		t.Fatalf("streamed %d bytes, want %d (first difference at %d)", content.Len(), len(want), firstDiff(content.String(), want))
	}
}

// The follower itself stalls (its first piece takes a second to pass on) while the worker writes thousands of
// pieces: none is lost, the text comes whole and in order, from the event stream alone (no poll in between).
func TestChatRunStalledFollower(t *testing.T) {
	defer func(d time.Duration) { chatPoll = d }(chatPoll)
	chatPoll = time.Minute
	s, srv := newHub(t, t.TempDir())
	device := signIn(t, srv.URL, "laptop")
	_, wtok := enroll(t, srv.URL, device, "Mac")
	startAPIMac(t, srv.URL, wtok)
	var long strings.Builder
	for i := 0; long.Len() < 30000; i++ {
		fmt.Fprintf(&long, "w%d ", i)
	}
	want := long.String()
	// the end must come by the event stream: the poll (a minute) is far later than this deadline, so a lost end
	// fails here, soon, instead of being found by the poll
	const within = 30 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	run, e := s.placeChat(ctx, "qwen3.6", chatFor{user: "Philipp", via: viaDevice})
	if e != nil {
		t.Fatal(e.message)
	}
	feed := s.fleet.followJob()
	defer feed.close()
	slow, stop := s.fleet.subscribe() // a broadcast subscriber that is not read: it drops
	defer stop()
	var got strings.Builder
	first := true
	j, e := run.run(ctx, feed, map[string]any{"model": run.model, "messages": []map[string]string{{"role": "user", "content": "say:" + want}}},
		chatHooks{text: func(s string) {
			if first {
				first = false
				time.Sleep(time.Second)
			}
			got.WriteString(s)
		}})
	if e != nil && ctx.Err() != nil {
		t.Fatalf("no end by the event stream within %s (passed on %d of %d bytes): the job's end was lost", within, got.Len(), len(want))
	}
	if e != nil {
		t.Fatal(e.message)
	}
	if j.Text != want || got.String() != want {
		t.Fatalf("job %d bytes, passed on %d of %d (first difference at %d)", len(j.Text), got.Len(), len(want), firstDiff(got.String(), want))
	}
	if len(slow) != cap(slow) {
		t.Fatalf("the broadcast subscriber kept all (%d): the test did not stall long enough", len(slow))
	}
}

func firstDiff(a, b string) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// A connector's chat says where it waits or runs at once, not first after the poll interval.
func TestVboardFirstStatusAtOnce(t *testing.T) {
	fastBridges(t)
	chatPoll = time.Hour
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	mac := startVboardMac(t, base, wtok, "chat")
	v := newFakeVboard(t, stageHead)
	addConnector(t, base, device, `{"url":"`+v.srv.URL+`","token":"vbp_secret"}`)
	b := v.next(t)
	hold := make(chan struct{})
	mac.chat.setHold(hold)
	defer mac.chat.setHold(nil)
	defer close(hold)
	_, ch := b.chat(`[{"role":"user","content":"hi"}]`, "")
	// placed (queued, or running already), then running as soon as it starts
	var statuses []string
	deadline := time.After(3 * time.Second)
	for len(statuses) == 0 || !strings.HasPrefix(statuses[len(statuses)-1], "Running on Mac (qwen3.6-35b-a3b)") {
		select {
		case ev := <-ch:
			if ev.Type == "status" {
				statuses = append(statuses, ev.Text)
			}
		case <-deadline:
			t.Fatalf("statuses %q", statuses)
		}
	}
	if len(statuses) > 2 || len(statuses) == 2 && !strings.HasPrefix(statuses[0], "Waiting for Mac (queued)") {
		t.Fatalf("statuses %q", statuses)
	}
}
