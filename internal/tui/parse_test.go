package tui

import (
	"testing"

	"github.com/philipparndt/ai-connector/internal/engine"
)

func TestParsePrompt(t *testing.T) {
	md := &engine.Model{ID: "v", Kind: engine.Video, Defaults: engine.Request{Frames: 97}}
	r, err := parsePrompt(md, "a fox in snow --size 768x512 --seed 7 --lowram")
	if err != nil {
		t.Fatal(err)
	}
	if r.Prompt != "a fox in snow" || r.Width != 768 || r.Height != 512 || r.Seed != 7 || !r.LowRAM || r.Frames != 97 {
		t.Fatalf("%+v", r)
	}
	for _, bad := range []string{"x --steps", "x --steps many", "x --size 7", "x --what 1", "x --image /does/not/exist.png"} {
		if _, err := parsePrompt(md, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseQuestion(t *testing.T) {
	q, opts, texts := parseQuestion("Which?\n- A: first\n- B\nsome text\n\nmore text")
	if q != "Which?" || len(opts) != 2 || opts[0].Label != "A" || opts[0].Description != "first" || opts[1].Label != "B" || len(texts) != 2 {
		t.Fatalf("%q %+v %q", q, opts, texts)
	}
}

func TestParseMusic(t *testing.T) {
	md := &engine.Model{ID: "m", Kind: engine.Music}
	r, err := parseMusic(md, "indie pop, warm vocal --duration 45 --seed 3\n[verse]\nHello\n[chorus]\nWorld")
	if err != nil {
		t.Fatal(err)
	}
	if r.Prompt != "indie pop, warm vocal" || r.Duration != 45 || r.Seed != 3 || r.Lyrics != "[verse]\nHello\n[chorus]\nWorld" || r.Instrumental {
		t.Fatalf("%+v", r)
	}
	r, _ = parseMusic(md, "synthwave --instrumental")
	if !r.Instrumental || r.Lyrics != "" || r.Prompt != "synthwave" {
		t.Fatalf("%+v", r)
	}
	if _, err := parseMusic(md, "x --duration long"); err == nil {
		t.Fatal("bad duration accepted")
	}
}

func TestParseMovie(t *testing.T) {
	md := &engine.Model{ID: "v", Kind: engine.Video, Defaults: engine.Request{Frames: 97}}
	r, err := parseMovie(md, "cartoon fox and squirrel --size 704x448\nthe squirrel carries an acorn\n+ the fox sneaks after it --frames 49\nsunset, both on a log")
	if err != nil {
		t.Fatal(err)
	}
	if r.Prompt != "cartoon fox and squirrel" || r.Width != 704 || len(r.Script) != 3 {
		t.Fatalf("%+v", r)
	}
	if r.Script[0].Continue || !r.Script[1].Continue || r.Script[1].Frames != 49 || r.Script[1].Prompt != "the fox sneaks after it" || r.Script[2].Continue {
		t.Fatalf("%+v", r.Script)
	}
	if _, err := parseMovie(md, "only a description\n\n"); err == nil {
		t.Fatal("a movie without clips accepted")
	}
}
