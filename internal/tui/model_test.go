package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/philipparndt/ai-connector/internal/client"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
)

// The model takes the stream's updates without a terminal or a server: a chat answer streams in, a video shows
// its progress, and both end done.
func TestApplyUpdates(t *testing.T) {
	mo := newModel(nil, true)
	mo.setCatalog(&client.Catalog{Models: []*engine.Model{
		{ID: "vid", Name: "Video", Kind: engine.Video, Here: true},
		{ID: "qwen", Name: "Qwen", Kind: engine.Chat, Here: true},
	}, Loaded: map[string]string{"mlx": "qwen"}})
	if md := mo.cur(); md == nil || md.ID != "qwen" {
		t.Fatalf("starts on %+v", md)
	}
	mo.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	mo.chats["qwen"] = []turn{{role: "user", content: "Hi"}, {role: "assistant", job: "c1"}}

	send := func(u jobs.Update) { mo.Update(updateMsg(u)) }
	send(jobs.Update{JobID: "c1", Job: &jobs.Job{ID: "c1", Model: "qwen", Kind: engine.Chat, State: jobs.Running, Started: time.Now()}})
	send(jobs.Update{JobID: "c1", Event: &engine.Event{Type: "text", Text: "Hello "}})
	send(jobs.Update{JobID: "c1", Event: &engine.Event{Type: "text", Text: "there"}})
	if j := mo.jobs["c1"]; j.Text != "Hello there" {
		t.Fatalf("text %q", j.Text)
	}
	if !strings.Contains(mo.content(), "Hello there") {
		t.Fatalf("chat does not show the answer:\n%s", mo.content())
	}
	if h := mo.history("qwen"); len(h) != 2 || h[1].Content != "Hello there" {
		t.Fatalf("history %+v", h)
	}

	send(jobs.Update{JobID: "v1", Job: &jobs.Job{ID: "v1", Model: "vid", Name: "Video", Kind: engine.Video, State: jobs.Running,
		Request: engine.Request{Prompt: "a fox"}, Created: time.Now(), Started: time.Now()}})
	send(jobs.Update{JobID: "v1", Event: &engine.Event{Type: "progress", Step: 3, Total: 8}})
	send(jobs.Update{JobID: "v1", Event: &engine.Event{Type: "status", Text: "denoising"}})
	if j := mo.jobs["v1"]; j.Step != 3 || j.Total != 8 || j.Status != "denoising" {
		t.Fatalf("progress %+v", j)
	}
	if f := mo.footer(); !strings.Contains(f, "3/8") || !strings.Contains(f, "denoising") {
		t.Fatalf("footer %q", f)
	}

	done := *mo.jobs["v1"]
	done.State, done.Files = jobs.Done, []string{"/files/fox.mp4"}
	send(jobs.Update{JobID: "v1", Job: &done})
	if mo.jobs["v1"].State != jobs.Done {
		t.Fatal("not done")
	}
	mo.cursor = 1 // the video model (chat models come first)
	if c := mo.content(); !strings.Contains(c, "done") || !strings.Contains(c, "/files/fox.mp4") {
		t.Fatalf("video pane:\n%s", c)
	}

	send(jobs.Update{JobID: "v1"}) // deleted
	if _, ok := mo.jobs["v1"]; ok {
		t.Fatal("deleted job still there")
	}

	// the header knows the Mac; the whole view renders
	mo.Update(workerMsg{Connected: true, Name: "studio"})
	if v := mo.View(); !strings.Contains(v, "Mac online") || !strings.Contains(v, "in memory: qwen") {
		t.Fatalf("view:\n%s", v)
	}
	mo.Update(workerMsg{Connected: false})
	if v := mo.View(); !strings.Contains(v, "Mac offline") {
		t.Fatalf("view:\n%s", v)
	}
}

// An answer that comes back from submitting fills the waiting chat turn and clears the input.
func TestSubmitted(t *testing.T) {
	mo := newModel(nil, false)
	mo.chats["qwen"] = []turn{{role: "user", content: "Hi"}, {role: "assistant"}}
	mo.input.SetValue("Hi")
	mo.submitted(submittedMsg{model: "qwen", text: "Hi", turn: 1, job: &jobs.Job{ID: "c1", Kind: engine.Chat, State: jobs.Queued}})
	if mo.chats["qwen"][1].job != "c1" || mo.jobs["c1"] == nil || mo.input.Value() != "" {
		t.Fatalf("%+v %v %q", mo.chats["qwen"], mo.jobs, mo.input.Value())
	}
	mo.chats["qwen"] = append(mo.chats["qwen"], turn{role: "user", content: "Again"}, turn{role: "assistant"})
	mo.submitted(submittedMsg{model: "qwen", turn: 3, err: client.ErrOffline})
	if mo.flash != "the Mac is offline" || !strings.HasPrefix(mo.chats["qwen"][3].content, "not sent") {
		t.Fatalf("%q %+v", mo.flash, mo.chats["qwen"])
	}
}

// Every example reads back with the parsers.
func TestExamplesParse(t *testing.T) {
	for kind, exs := range engine.Examples {
		md := &engine.Model{ID: "m", Kind: kind, Defaults: engine.Request{Frames: 97}}
		for _, e := range exs {
			text := exampleText(kind, e)
			var err error
			switch {
			case kind == engine.Video && strings.Contains(text, "\n"):
				_, err = parseMovie(md, text)
			case kind == engine.Image || kind == engine.Video:
				_, err = parsePrompt(md, text)
			case kind == engine.Music:
				_, err = parseMusic(md, text)
			}
			if err != nil {
				t.Errorf("%s %q: %v", kind, e.Title, err)
			}
		}
	}
}
