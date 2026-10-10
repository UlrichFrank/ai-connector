package agents

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

var testPNG = []byte("\x89PNG\r\n\x1a\nred square")

// The json driver sends a turn with images as content blocks: the text up to each tag, the image, the rest (the
// fake echoes an image as <image type bytes>); the opening carries its own images.
func TestJSONImageBlocks(t *testing.T) {
	m, _ := fakeManager(t)
	r := &recorder{}
	ims := []Image{{ID: "image-1", MIME: "image/png", Data: testPNG}, {ID: "image-2", MIME: "image/webp", Data: []byte("wbp")}}
	res, err := m.Turn(context.Background(), Options{Key: "k", Images: ims}, "compare [image-2] with [image-1]", r.emit)
	if err != nil {
		t.Fatal(err)
	}
	if want := "echo: compare [image-2]<image image/webp 3> with [image-1]<image image/png 18>"; res.Text != want {
		t.Fatalf("answer %q, want %q", res.Text, want)
	}
	// a new conversation with history: the opening goes with its images, not the turn's
	op := []Image{{ID: "m1-image-1", MIME: "image/png", Data: testPNG}}
	res, err = m.Turn(context.Background(), Options{Key: "k2", Opening: "earlier [m1-image-1]; now: hi", OpeningImages: op, Images: ims[:1]}, "hi", r.emit)
	if err != nil || res.Text != "echo: earlier [m1-image-1]<image image/png 18>; now: hi" {
		t.Fatalf("opening %q %v", res.Text, err)
	}
	// its next turn: the turn's images
	res, _ = m.Turn(context.Background(), Options{Key: "k2", Opening: "x", OpeningImages: op, Images: ims[:1]}, "and [image-1]", r.emit)
	if res.Text != "echo: and [image-1]<image image/png 18>" {
		t.Fatalf("next turn %q", res.Text)
	}
}

func TestContentBlocksUntagged(t *testing.T) {
	b := contentBlocks("no tag here", []Image{{ID: "image-1", MIME: "image/png", Data: []byte{1}}})
	if len(b) != 3 || b[0]["text"] != "no tag here" || b[1]["text"] != "[image-1]" || b[2]["type"] != "image" {
		t.Fatalf("%v", b)
	}
}

// The tty driver writes the images into the session's folder (attachments/), names the files where the tags are,
// and keeps an earlier turn's file; the session's rules let it read them.
func TestTTYAttachments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no tty on Windows")
	}
	m, _ := guardedManager(t)
	im := []Image{{ID: "image-1", MIME: "image/png", Data: testPNG}}
	res, err := m.Turn(context.Background(), Options{Key: "k", Driver: DriverTTY, Images: im}, "what colour is [image-1]?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "echo: what colour is [image-1: attachments/image-1.png]?" {
		t.Fatalf("typed %q", res.Text)
	}
	m.mu.Lock()
	s := m.sessions[res.Session]
	m.mu.Unlock()
	file := filepath.Join(s.cwd, "attachments", "image-1.png")
	if b, err := os.ReadFile(file); err != nil || !bytes.Equal(b, testPNG) {
		t.Fatalf("attachment %v", err)
	}
	// a second image-1 does not overwrite the first
	im2 := []Image{{ID: "image-1", MIME: "image/jpeg", Data: []byte("jpg")}}
	res, _ = m.Turn(context.Background(), Options{Key: "k", Driver: DriverTTY, Images: im2}, "and [image-1]", nil)
	if res.Text != "echo: and [image-1: attachments/image-1.jpg]" {
		t.Fatalf("second %q", res.Text)
	}
	res, _ = m.Turn(context.Background(), Options{Key: "k", Driver: DriverTTY, Images: im}, "again [image-1]", nil)
	if res.Text != "echo: again [image-1: attachments/image-1-2.png]" {
		t.Fatalf("third %q", res.Text)
	}
	// inside the folder the session may read (Read(./**)), and no rule denies it
	rel, err := filepath.Rel(s.cwd, file)
	if err != nil || strings.HasPrefix(rel, "..") || !slices.Contains(m.Config.AllowedTools, "Read(./**)") {
		t.Fatalf("not in the session's folder: %s (%v)", rel, m.Config.AllowedTools)
	}
	for _, r := range m.denyRules(s) {
		p := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r, "Read("), ")"), "/**")
		if strings.HasPrefix(r, "Read(//") && strings.HasPrefix("/"+strings.TrimPrefix(filepath.ToSlash(file), "/"), "/"+strings.TrimPrefix(p, "//")) {
			t.Fatalf("rule %s denies the attachment", r)
		}
	}
}

func TestAttachRefusesBadIDs(t *testing.T) {
	dir := t.TempDir()
	if _, err := attach(dir, "x", []Image{{ID: "../evil", MIME: "image/png", Data: []byte{1}}}); err == nil {
		t.Fatal("a path as id")
	}
	if _, err := attach(dir, "x", []Image{{ID: "a", MIME: "text/html", Data: []byte{1}}}); err == nil {
		t.Fatal("not an image type")
	}
}
