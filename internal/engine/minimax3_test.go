package engine

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestTranslateLyricsMiniMax3(t *testing.T) {
	const style = "dark techno, female vocal"
	for _, c := range []struct {
		lyrics, style   string
		want, wantStyle string
	}{
		// lowercase official tags, no blank lines, directions to the description, backing vocals kept
		{userSong, style,
			"[intro]\nYeah.\nBlack. Pink.\nWatch your step.\n[verse]\nChrome on the dance floor, we rise (we rise)\n" +
				"Nobody sees us\n[instrumental]\n[chorus]\nBlack and pink forever\n(ooh, ooh)\n[outro]",
			style + ", heavy bass drop, mechanical breathing, whispered, distorted synth stabs, fade to silence"},
		// numbers go, post-chorus and solo keep their tags, sections it has no tag for by whether they carry words
		{"[Verse 2]\nA\n[Post-Chorus]\nB\n[Guitar Solo]\n[Drop]\nC\n[build-up]\nD\n[breakdown]\nE\n[drop - heavy]\n[Hook]\nF",
			"edm",
			"[verse]\nA\n[post-chorus]\nB\n[solo]\n[chorus]\nC\n[pre-chorus]\nD\n[bridge]\nE\n[instrumental]\n[chorus]\nF",
			"edm, guitar solo, heavy"}, // (the solo's instrument goes to the description)
		// a description in the tag is not sung: it goes to the style
		{"[chorus: anthemic, gang vocals]\nWe are one", "pop", "[chorus]\nWe are one", "pop, anthemic, gang vocals"},
	} {
		got, gotStyle := TranslateLyrics("minimax3", c.lyrics, c.style)
		if got != c.want || gotStyle != c.wantStyle {
			t.Errorf("TranslateLyrics(minimax3, %q, %q)\n got %q\n     %q\nwant %q\n     %q", c.lyrics, c.style,
				got, gotStyle, c.want, c.wantStyle)
		}
	}
	// the whole "write-song" song: no direction is left in a sung line, every sung line is kept
	b, err := os.ReadFile("testdata/neon-venom.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := TranslateLyrics("minimax3", string(b), "K-Pop, Dark Pop")
	for _, l := range strings.Split(got, "\n") {
		if l == "" {
			t.Errorf("a blank line in %q", got)
		}
		if strings.HasPrefix(l, "[") {
			if !slices.Contains([]string{"[intro]", "[verse]", "[pre-chorus]", "[chorus]", "[post-chorus]", "[bridge]",
				"[instrumental]", "[solo]", "[outro]"}, l) {
				t.Errorf("a tag MiniMax Music 3 does not list: %q", l)
			}
			continue
		}
		for _, d := range []string{"mechanical breathing", "synth stabs", "industrial", "vocal chops", "brass blast"} {
			if strings.Contains(strings.ToLower(l), d) {
				t.Errorf("sings %q: %q", d, l)
			}
		}
	}
	for _, w := range []string{"Black. Pink.", "Wait.", "NOW!", "1, 2, 3, go!", "Take a bow."} {
		if !strings.Contains(got, w) {
			t.Errorf("lost the sung %q", w)
		}
	}
}

func TestMusicRequestMiniMax3(t *testing.T) {
	m := findMusic(t, "minimax-music3-8bit")
	r := Request{Prompt: "Genre: synth-pop. Warm female lead", Lyrics: "[Verse 1]\n(soft pads)\nHello\n[chorus]\nWorld",
		BPM: 118, Duration: 30, Steps: 30, Seed: 4}
	req, err := musicRequest(m, r, "generate", map[string]string{"weights": "/w"}, "/o.wav")
	if err != nil {
		t.Fatal(err)
	}
	if req["model"] != "8bit" || req["lyrics"] != "[verse]\nHello\n[chorus]\nWorld" || req["bpm"] != 118 ||
		req["prompt"] != "Genre: synth-pop. Warm female lead, soft pads, 118 BPM" {
		t.Errorf("MiniMax request: %v", req)
	}
	// instrumental: wordless sections with an explicit line, and the description says so
	req, _ = musicRequest(m, Request{Prompt: "melodic techno, 128 bpm", Lyrics: "[verse]\nLa", Instrumental: true}, "generate", nil, "/o.wav")
	if req["lyrics"] != minimax3Instrumental || req["prompt"] != "melodic techno, 128 bpm, instrumental, no vocals" {
		t.Errorf("instrumental: %v", req)
	}
	req, _ = musicRequest(m, Request{Prompt: "instrumental ambient"}, "generate", nil, "/o.wav")
	if req["lyrics"] != minimax3Instrumental || req["prompt"] != "instrumental ambient" {
		t.Errorf("no lyrics: %v", req)
	}
}

func TestMiniMax3Catalog(t *testing.T) {
	rt := MusicRuntimes()["minimax3"]
	if rt == nil || rt.script != "minimax3.py" {
		t.Fatal("no minimax3 runtime")
	}
	if _, err := workerFiles.ReadFile("workers/minimax3.py"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rt.env.Packages[0], "@"+minimax3Commit) || len(minimax3Commit) != 40 {
		t.Errorf("the port is not pinned to a commit: %v", rt.env.Packages[0])
	}
	for _, p := range rt.env.Packages[1:] {
		if !strings.Contains(p, "==") {
			t.Errorf("unpinned dependency %q", p)
		}
	}
	for _, c := range []struct {
		id, repo string
		size     int64
	}{
		{"minimax-music3-8bit", "mikoy92/MiniMax-Music3-MLX-8bit", 13363585426},
		{"minimax-music3-4bit", "mikoy92/MiniMax-Music3-MLX-4bit", 7960010136},
	} {
		m := findMusic(t, c.id)
		if m.runtime != "minimax3" || len(m.repos) != 1 || m.repos[0].repo != c.repo || len(m.repos[0].rev) != 40 ||
			m.repos[0].key != "weights" || m.Size != c.size {
			t.Errorf("%s: %+v %+v", c.id, m, m.repos)
		}
		if m.maxSeconds != 300 || m.Defaults.Steps != 30 || m.Defaults.Duration <= 0 {
			t.Errorf("%s: limits and defaults %v %+v", c.id, m.maxSeconds, m.Defaults)
		}
		if m.NonCommercial || !strings.Contains(m.Licence, "MiniMax-Music3") || !strings.Contains(m.About, "\"MiniMax-Music3\"") {
			t.Errorf("%s: licence %q (commercial use allowed with the name shown)", c.id, m.Licence)
		}
		if m.Gated || len(m.Tasks) != 0 {
			t.Errorf("%s: not gated, no studio tasks", c.id)
		}
	}
}
