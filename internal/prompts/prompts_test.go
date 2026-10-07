package prompts

import (
	"strings"
	"testing"
)

// The song prompt keeps directions out of the sung lines (HeartMuLa sang "(Heavy bass drop …)"); an unedited
// built-in follows a changed default, an edited one keeps its text and resets to the new default.
func TestWriteSongDefault(t *testing.T) {
	dir := t.TempDir()
	l := Open(dir)
	p, _ := l.Get("write-song")
	if p.Modified || p.Text != p.Default || !strings.Contains(p.Text, "only section tags and the words to be sung") ||
		!strings.Contains(p.Text, "[drop - heavy bass, mechanical breathing]") {
		t.Fatalf("write-song: %+v", p)
	}
	p.Text = "Old text."
	_, _ = l.Save(p)
	if p, _ = Open(dir).Get("write-song"); !p.Modified || p.Text != "Old text." {
		t.Fatalf("edited: %+v", p)
	}
	_ = l.Reset("write-song")
	if p, _ = Open(dir).Get("write-song"); p.Modified || p.Text != p.Default {
		t.Fatalf("reset: %+v", p)
	}
}

func TestEditResetOwn(t *testing.T) {
	dir := t.TempDir()
	l := Open(dir)
	p, ok := l.Get("enhance-video")
	if !ok || !p.Builtin || p.Modified || !strings.Contains(p.Text, "audio-visual caption") {
		t.Fatalf("built-in video prompt: %+v", p)
	}
	// edit a built-in one: it stays a built-in of its use, marked modified; it survives a reopen
	p.Text, p.Use = "Shorter.", Chat
	if _, err := l.Save(p); err != nil {
		t.Fatal(err)
	}
	p, _ = Open(dir).Get("enhance-video")
	if p.Text != "Shorter." || !p.Modified || p.Use != EnhanceVideo || p.Default == "" {
		t.Fatalf("edited: %+v", p)
	}
	if err := l.Reset("enhance-video"); err != nil {
		t.Fatal(err)
	}
	if p, _ = Open(dir).Get("enhance-video"); p.Modified || p.Text != p.Default {
		t.Fatalf("reset: %+v", p)
	}

	// your own: an id from the name, unique; deleted for good
	a, err := l.Save(Prompt{Name: "Claude style", Use: Chat, Text: "Be thoughtful."})
	if err != nil || a.ID != "my-claude-style" || a.Builtin {
		t.Fatalf("own: %+v %v", a, err)
	}
	b, _ := l.Save(Prompt{Name: "Claude style", Use: Chat, Text: "Other."})
	if b.ID != "my-claude-style-2" {
		t.Fatalf("second id %q", b.ID)
	}
	_ = l.Delete(a.ID)
	if _, ok := Open(dir).Get(a.ID); ok {
		t.Fatal("deleted prompt still there")
	}
	if _, err := l.Save(Prompt{Name: "x", Use: "nonsense", Text: "y"}); err == nil {
		t.Fatal("unknown use accepted")
	}
}
