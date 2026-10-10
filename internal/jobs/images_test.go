package jobs

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/llm"
)

// seer keeps the requests it ran and returns their messages as the result (as the agent loop does).
type seer struct {
	mu  sync.Mutex
	got []engine.Request
}

func (s *seer) Name() string   { return "see" }
func (s *seer) Heavy() bool    { return false }
func (s *seer) Loaded() string { return "" }
func (s *seer) Unload()        {}
func (s *seer) Run(_ context.Context, _ *engine.Model, r engine.Request, _ string, emit engine.Emit) error {
	s.mu.Lock()
	s.got = append(s.got, r)
	s.mu.Unlock()
	emit(engine.Event{Type: "text", Text: "red"})
	emit(engine.Event{Type: "result", Data: map[string]any{"messages": r.Messages}})
	return nil
}

func (s *seer) last() engine.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got[len(s.got)-1]
}

// A chat's images go into files of the outputs; the history keeps references (no base64), the backend gets the
// bytes; a model without the capability refuses them; a retry reuses the files and deleting the job removes them.
func TestChatImagesKeptAsFiles(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nred square image")
	b := &seer{}
	cat := &engine.Catalog{Models: []*engine.Model{
		{ID: "vision", Name: "Vision", Kind: engine.Chat, Backend: "see", Images: true},
		{ID: "text", Name: "Text", Kind: engine.Chat, Backend: "see"},
	}}
	m := New(cat, []engine.Backend{b}, t.TempDir())
	settle(t, m)
	msgs := []llm.Message{{Role: "user", Content: "what colour is [image-1]?", Images: []llm.Image{{ID: "image-1", MIME: "image/png", Data: png}}}}

	if _, err := m.Submit(engine.Request{Model: "text", Messages: msgs}); err == nil || !strings.Contains(err.Error(), "does not take images") {
		t.Fatalf("text model: %v", err)
	}
	j, err := m.Submit(engine.Request{Model: "vision", Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs[0].Images[0].Data) == 0 {
		t.Fatal("the caller's request was changed")
	}
	im := j.Request.Messages[0].Images[0]
	if len(im.Data) != 0 || !m.keptImage(im.File) || !strings.HasSuffix(im.File, ".png") {
		t.Fatalf("kept %+v", im)
	}
	done := wait(t, m, j.ID)
	if done.State != Done {
		t.Fatalf("%s %s", done.State, done.Error)
	}
	if got := b.last().Messages[0].Images[0]; !bytes.Equal(got.Data, png) {
		t.Fatalf("the backend got %+v", got)
	}
	if rm := ResultMessages(done); len(rm) != 1 || len(rm[0].Images) != 1 || rm[0].Images[0].Data != nil || rm[0].Images[0].File != im.File {
		t.Fatalf("result messages %+v", rm)
	}
	waitSettled(t, m)
	hist, _ := os.ReadFile(m.historyPath())
	if bytes.Contains(hist, []byte(base64.StdEncoding.EncodeToString(png))) || !bytes.Contains(hist, []byte(im.File)) {
		t.Fatalf("history %s", hist)
	}

	// a forged reference outside the images folder is refused
	bad := []llm.Message{{Role: "user", Content: "[image-1]", Images: []llm.Image{{ID: "image-1", MIME: "image/png", File: m.historyPath()}}}}
	if _, err := m.Submit(engine.Request{Model: "vision", Messages: bad}); err == nil {
		t.Fatal("a file outside the images folder")
	}
	// too many
	var many []llm.Image
	for i := range 9 {
		many = append(many, llm.Image{ID: "image-" + string(rune('a'+i)), MIME: "image/png", Data: png})
	}
	if _, err := m.Submit(engine.Request{Model: "vision", Messages: []llm.Message{{Role: "user", Content: "x", Images: many}}}); err == nil {
		t.Fatal("9 images")
	}

	// a retry runs with the same files
	r, err := m.Retry(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, m, r.ID)
	if got := b.last().Messages[0].Images[0]; !bytes.Equal(got.Data, png) || r.Request.Messages[0].Images[0].File != im.File {
		t.Fatalf("retry %+v", got)
	}
	waitSettled(t, m)
	if err := m.DeleteWithFiles(r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(im.File); !os.IsNotExist(err) {
		t.Fatalf("the image stays: %v", err)
	}
}

func waitSettled(t *testing.T, m *Manager) {
	t.Helper()
	for i := 0; i < 600 && !m.Settled(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
}
