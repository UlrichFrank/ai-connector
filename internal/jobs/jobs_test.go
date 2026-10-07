package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/engine"
)

// fake is a backend that streams its prompt back word by word (or blocks until canceled).
type fake struct {
	name   string
	heavy  bool
	loaded string
	block  bool
}

func (f *fake) Name() string   { return f.name }
func (f *fake) Heavy() bool    { return f.heavy }
func (f *fake) Loaded() string { return f.loaded }
func (f *fake) Unload()        { f.loaded = "" }
func (f *fake) Run(ctx context.Context, m *engine.Model, r engine.Request, _ string, emit engine.Emit) error {
	f.loaded = m.ID
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if r.Prompt == "fail" {
		return errors.New("boom")
	}
	for _, w := range []string{"a", "b"} {
		emit(engine.Event{Type: "text", Text: w})
	}
	if r.Prompt == "cover" {
		emit(engine.Event{Type: "file", File: "/out/cover.png"})
	}
	emit.Progress(2, 2)
	return nil
}

func setup(t *testing.T, backends ...engine.Backend) *Manager {
	cat := &engine.Catalog{Models: []*engine.Model{
		{ID: "chat-a", Kind: engine.Chat, Backend: "a"},
		{ID: "img-b", Kind: engine.Image, Backend: "b"},
	}}
	m := New(cat, backends, t.TempDir())
	settle(t, m)
	return m
}

// settle lets a test end only when the manager is idle and its history written: a save after the end would write
// into the removed temporary directory. (Registered after t.TempDir, it runs before the removal.)
func settle(t *testing.T, m *Manager) {
	t.Cleanup(func() {
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if m.Settled() {
				return
			}
		}
	})
}

func wait(t *testing.T, m *Manager, id string) *Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := m.Job(id); j != nil && !j.Active() {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return nil
}

func TestRunRecordsEvents(t *testing.T) {
	m := setup(t, &fake{name: "a", heavy: true}, &fake{name: "b", heavy: true})
	j, err := m.Submit(engine.Request{Model: "chat", Prompt: "hi"}) // a unique prefix
	if err != nil {
		t.Fatal(err)
	}
	done := wait(t, m, j.ID)
	if done.State != Done || done.Text != "ab" || done.Step != 2 || done.Total != 2 {
		t.Fatalf("%+v", done)
	}
	f, _ := m.Job(j.ID)
	if f.Model != "chat-a" {
		t.Fatalf("model %q", f.Model)
	}
	if _, err := m.Submit(engine.Request{Model: "nope"}); err == nil {
		t.Fatal("unknown model accepted")
	}
}

func TestFailureAndHistory(t *testing.T) {
	dir := t.TempDir()
	cat := &engine.Catalog{Models: []*engine.Model{{ID: "chat-a", Kind: engine.Chat, Backend: "a"}}}
	m := New(cat, []engine.Backend{&fake{name: "a"}}, dir)
	j, _ := m.Submit(engine.Request{Model: "chat-a", Prompt: "fail"})
	if done := wait(t, m, j.ID); done.State != Failed || done.Error != "boom" {
		t.Fatalf("%+v", done)
	}
	// the state is visible a moment before the history is written: wait for the file
	for i := 0; i < 200; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "history.json")); err == nil && strings.Contains(string(b), `"failed"`) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// a new manager reads the history
	m2 := New(cat, []engine.Backend{&fake{name: "a"}}, dir)
	if js := m2.Jobs(); len(js) != 1 || js[0].ID != j.ID {
		t.Fatalf("history: %+v", js)
	}
}

func TestCancelAndOneHeavyModel(t *testing.T) {
	a := &fake{name: "a", heavy: true, block: true}
	b := &fake{name: "b", heavy: true}
	m := setup(t, a, b)
	ja, _ := m.Submit(engine.Request{Model: "chat-a"})
	jb, _ := m.Submit(engine.Request{Model: "img-b", Prompt: "x"})
	for { // wait until a runs
		if j, _ := m.Job(ja.ID); j.State == Running {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := m.Cancel(ja.ID); err != nil {
		t.Fatal(err)
	}
	if done := wait(t, m, ja.ID); done.State != Canceled {
		t.Fatalf("a: %+v", done)
	}
	if done := wait(t, m, jb.ID); done.State != Done {
		t.Fatalf("b: %+v", done)
	}
	if a.loaded != "" || b.loaded != "img-b" {
		t.Fatalf("loaded: a=%q b=%q (b's job should unload a)", a.loaded, b.loaded)
	}
}

// Queued jobs survive a restart and run then.
func TestQueuedSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	cat := &engine.Catalog{Models: []*engine.Model{{ID: "chat-a", Kind: engine.Chat, Backend: "a"}}}
	blocker := &fake{name: "a", block: true}
	m := New(cat, []engine.Backend{blocker}, dir)
	m.Submit(engine.Request{Model: "chat-a"})
	q, _ := m.Submit(engine.Request{Model: "chat-a", Prompt: "later"})
	m2 := New(cat, []engine.Backend{&fake{name: "a"}}, dir)
	settle(t, m2)
	if done := wait(t, m2, q.ID); done.State != Done || done.Text != "ab" {
		t.Fatalf("%+v", done)
	}
	if j, _ := m2.Submit(engine.Request{Model: "chat-a"}); j.ID == q.ID {
		t.Fatal("id reused")
	}
}

func TestExplainAndRetry(t *testing.T) {
	p := Explain("exit status 1 green on MLX 0.30). Known mitigations: * relaunch with AGX_RELAX_CDM_CTXSTORE_TIMEOUT=1 …")
	if p.Title != "macOS stopped the GPU job" || p.Hint == "" {
		t.Fatalf("%+v", p)
	}
	if Explain("something odd\nmore").Title != "something odd" {
		t.Fatal("unknown errors keep their first line")
	}
	m := setup(t, &fake{name: "a"}, &fake{name: "b"})
	j, _ := m.Submit(engine.Request{Model: "chat-a", Prompt: "fail"})
	failed := wait(t, m, j.ID)
	if failed.Problem == nil {
		t.Fatal("no problem for a failed job")
	}
	r, err := m.Retry(j.ID)
	if err != nil || r.ID == j.ID || r.Request.Prompt != "fail" {
		t.Fatalf("retry: %+v %v", r, err)
	}
	if _, ok := m.Job(j.ID); ok {
		t.Fatal("the failed job should give way to its retry")
	}
	wait(t, m, r.ID)
}

func TestRename(t *testing.T) {
	m := setup(t, &fake{name: "a"}, &fake{name: "b"})
	j, _ := m.Submit(engine.Request{Model: "chat-a", Prompt: "x"})
	wait(t, m, j.ID)
	r, err := m.Rename(j.ID, "  Sunrise drive (remix) ")
	if err != nil || r.Request.Title != "Sunrise drive (remix)" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := m.Rename(j.ID, " "); err == nil {
		t.Fatal("empty title accepted")
	}
	if _, err := m.Rename("nope", "x"); err == nil {
		t.Fatal("unknown job accepted")
	}
}

func TestFolders(t *testing.T) {
	m := setup(t, &fake{name: "a"}, &fake{name: "b"})
	song, _ := m.Submit(engine.Request{Model: "chat-a", Prompt: "x"})
	wait(t, m, song.ID)
	remix, _ := m.Submit(engine.Request{Model: "chat-a", Prompt: "y", Parent: song.ID})
	wait(t, m, remix.ID)
	other, _ := m.Submit(engine.Request{Model: "chat-a", Prompt: "z"})
	wait(t, m, other.ID)

	moved, err := m.Move(song.ID, " Albums // Summer ")
	if err != nil || len(moved) != 2 {
		t.Fatalf("a song and its version should move together: %d %v", len(moved), err)
	}
	folder := func(id string) string { j, _ := m.Job(id); return j.Request.Folder }
	if folder(song.ID) != "Albums/Summer" || folder(remix.ID) != "Albums/Summer" || folder(other.ID) != "" {
		t.Fatalf("folders: %q %q %q", folder(song.ID), folder(remix.ID), folder(other.ID))
	}
	// a new version lands with its original
	v, _ := m.Submit(engine.Request{Model: "chat-a", Prompt: "v", Parent: song.ID})
	if v.Request.Folder != "Albums/Summer" {
		t.Fatalf("new version in %q", v.Request.Folder)
	}
	wait(t, m, v.ID)
	if n, _ := m.RenameFolder("Albums", "Records"); n != 3 || folder(song.ID) != "Records/Summer" {
		t.Fatalf("rename: %d %q", n, folder(song.ID))
	}
	if _, err := m.RenameFolder("Records/Summer", ""); err != nil || folder(song.ID) != "Records" {
		t.Fatalf("remove: %q", folder(song.ID))
	}
}

// A picture made for a song becomes its cover when it is done.
func TestCoverFor(t *testing.T) {
	files := &fake{name: "b"}
	m := setup(t, &fake{name: "a"}, files)
	song, _ := m.Submit(engine.Request{Model: "chat-a", Prompt: "x"})
	wait(t, m, song.ID)
	img, _ := m.Submit(engine.Request{Model: "img-b", Prompt: "cover", CoverFor: song.ID})
	wait(t, m, img.ID)
	if j, _ := m.Job(song.ID); j.Cover != "/out/cover.png" {
		t.Fatalf("the picture did not become the cover: %q", j.Cover)
	}
	if j, _ := m.SetCover(song.ID, ""); j.Cover != "" {
		t.Fatal("cover not removed")
	}
}

// Deleting removes the files a job made, but not files another job still uses.
func TestDeleteWithFiles(t *testing.T) {
	m := setup(t, &fake{name: "a"}, &fake{name: "b"})
	out := m.OutDir()
	os.MkdirAll(filepath.Join(out, "movies", "x"), 0o755)
	song, own, shared, score := filepath.Join(out, "song.wav"), filepath.Join(out, "own.png"), filepath.Join(out, "shared.png"), filepath.Join(out, "song.abc")
	for _, f := range []string{song, own, shared, score, filepath.Join(out, "movies", "x", "clip-01.mp4")} {
		os.WriteFile(f, []byte("x"), 0o644)
	}
	a := m.Import(engine.Music, "Uploaded", song, engine.Request{Model: "upload"})
	m.mu.Lock()
	m.find(a.ID).Files = []string{song}
	m.find(a.ID).Stats = map[string]any{"clipsDir": filepath.Join(out, "movies", "x")}
	m.mu.Unlock()
	b := m.Import(engine.Image, "Uploaded", own, engine.Request{Model: "upload"})
	c := m.Import(engine.Image, "Uploaded", shared, engine.Request{Model: "upload"})
	// a version made from the song keeps it alive; c's picture is the cover of b
	v := m.Import(engine.Music, "v", filepath.Join(out, "v.wav"), engine.Request{Model: "upload", Source: song})
	m.mu.Lock()
	m.find(b.ID).Cover = shared
	m.mu.Unlock()

	if err := m.DeleteWithFiles(a.ID); err != nil {
		t.Fatal(err)
	}
	if !fileExists(song) || fileExists(score) || fileExists(filepath.Join(out, "movies", "x")) {
		t.Fatalf("song kept (used by a version) %v, score gone %v, clips gone %v", fileExists(song), !fileExists(score), !fileExists(filepath.Join(out, "movies", "x")))
	}
	_ = m.DeleteWithFiles(v.ID)
	_ = m.DeleteWithFiles(c.ID) // its picture is still b's cover
	if !fileExists(shared) {
		t.Fatal("a cover in use was deleted")
	}
	_ = m.DeleteWithFiles(b.ID)
	if fileExists(own) {
		t.Fatal("own file kept")
	}
	if _, ok := m.Job(a.ID); ok {
		t.Fatal("job still listed")
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestStar(t *testing.T) {
	m := setup(t, &fake{name: "a"}, &fake{name: "b"})
	j, _ := m.Submit(engine.Request{Model: "chat-a", Prompt: "x"})
	wait(t, m, j.ID)
	if s, _ := m.Star(j.ID, true); !s.Starred {
		t.Fatal("not starred")
	}
	if s, _ := m.Star(j.ID, false); s.Starred {
		t.Fatal("still starred")
	}
}
