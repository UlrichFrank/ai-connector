package agents

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A tool call whose input takes long to write (Claude Code streams it as input_json_delta) is reported as it
// grows, at most every writingEvery: "writing create_items … 8 KB".
func TestWritingStatus(t *testing.T) {
	defer func(d time.Duration) { writingEvery = d }(writingEvery)
	writingEvery = 40 * time.Millisecond
	m, _ := fakeManager(t)
	m.Env = append(m.Env, "AGENTS_FAKE_DELTA_MS=10") // 20 pieces of 1 KB, 10 ms apart: ~200 ms
	r := &recorder{}
	start := time.Now()
	res, err := m.Turn(context.Background(), Options{Key: "w", Owner: "alice"}, "bigwrite", r.emit)
	took := time.Since(start)
	if err != nil || res.Text != "Added 20 items." {
		t.Fatalf("%+v %v", res, err)
	}
	var writing []string
	using := false
	for _, e := range r.events {
		if e.Type != "status" {
			continue
		}
		if e.Text == "using mcp__vboard__create_items …" {
			using = true
		}
		if strings.HasPrefix(e.Text, "writing ") {
			writing = append(writing, e.Text)
		}
	}
	if !using || len(writing) < 2 || len(writing) > int(took/writingEvery)+1 {
		t.Fatalf("statuses in %v: using %v, writing %q", took, using, writing)
	}
	last := 0
	for _, w := range writing {
		size, ok := strings.CutPrefix(w, "writing create_items … ")
		n, err := strconv.Atoi(strings.TrimSuffix(size, " KB"))
		if !ok || err != nil || !strings.HasSuffix(size, " KB") || n < last || n > 20 {
			t.Fatalf("status %q after %d KB", w, last)
		}
		last = n
	}
	if tools := r.tools(); len(tools) != 2 || tools[0].Name != "mcp__vboard__create_items" || tools[1].State != "done" {
		t.Fatalf("tools %+v", tools)
	}
}

func TestSizeText(t *testing.T) {
	for n, want := range map[int]string{0: "0 bytes", 1000: "1000 bytes", 1024: "1 KB", 8*1024 + 600: "9 KB", 3 << 20: "3.0 MB"} {
		if got := sizeText(n); got != want {
			t.Errorf("sizeText(%d) = %q, want %q", n, got, want)
		}
	}
	if toolShort("mcp__vboard__create_items") != "create_items" || toolShort("Read") != "Read" {
		t.Fatal("toolShort")
	}
}

// --effort: from the turn's options, else agents.json's; another effort restarts the session's process (resumed);
// an unknown one is refused.
func TestEffort(t *testing.T) {
	m, argsLog := guardedManager(t)
	if _, err := m.Turn(context.Background(), Options{Key: "e", Owner: "alice", Effort: "high"}, "hi", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Turn(context.Background(), Options{Key: "e", Owner: "alice", Effort: "high"}, "again", nil); err != nil {
		t.Fatal(err)
	}
	all := startsOf(t, argsLog)
	if len(all) != 1 {
		t.Fatalf("%d starts for one effort", len(all))
	}
	if e, _ := all[0].flag("--effort"); e != "high" {
		t.Fatalf("--effort %q", e)
	}
	m.Config.Effort = "low" // agents.json: the default of a turn that names none
	if _, err := m.Turn(context.Background(), Options{Key: "e", Owner: "alice"}, "hi", nil); err != nil {
		t.Fatal(err)
	}
	all = startsOf(t, argsLog)
	if len(all) != 2 {
		t.Fatalf("%d starts", len(all))
	}
	if e, _ := all[1].flag("--effort"); e != "low" {
		t.Fatalf("--effort %q", e)
	}
	if r, _ := all[1].flag("--resume"); r == "" {
		t.Fatalf("the new process does not resume: %v", all[1].Args)
	}
	m.Config.Effort = ""
	if _, err := m.Turn(context.Background(), Options{Key: "n", Owner: "alice"}, "hi", nil); err != nil {
		t.Fatal(err)
	}
	all = startsOf(t, argsLog)
	if _, ok := all[2].flag("--effort"); ok {
		t.Fatalf("--effort without one: %v", all[2].Args)
	}
	if _, err := m.Turn(context.Background(), Options{Key: "e", Owner: "alice", Effort: "extreme"}, "hi", nil); err == nil ||
		!strings.Contains(err.Error(), "unknown effort") {
		t.Fatalf("unknown effort: %v", err)
	}
}
