package engine

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// userSong is the song HeartMuLa sang the stage direction of.
const userSong = "[intro]\n(Heavy bass drop, mechanical breathing)\nYeah.\nBlack. Pink.\nWatch your step.\n\n" +
	"[verse]\nChrome on the dance floor, we rise (we rise)\n*whispered* Nobody sees us\n\n" +
	"[drop]\n(Distorted synth stabs)\n\n[chorus]\nBlack and pink forever\n(ooh, ooh)\n\n[outro]\n(Fade to silence)"

func TestSplitTag(t *testing.T) {
	for _, c := range []struct {
		in, tag, label, number, desc string
		ok                           bool
	}{
		{"intro", "intro", "intro", "", "", true},
		{"Verse 2", "verse", "Verse 2", "2", "", true},
		{"verse-1", "verse", "verse-1", "1", "", true},
		{"Pre-Chorus", "pre-chorus", "Pre-Chorus", "", "", true},
		{"Pre Chorus", "pre-chorus", "Pre Chorus", "", "", true},
		{"prechorus", "pre-chorus", "prechorus", "", "", true},
		{"Drop - heavy bass, mechanical breathing", "drop", "Drop", "", "heavy bass, mechanical breathing", true},
		{"Chorus: anthemic", "chorus", "Chorus", "", "anthemic", true},
		{"Chorus (whispered)", "chorus", "Chorus", "", "whispered", true},
		{"BUILD-UP", "build-up", "BUILD-UP", "", "", true},
		{"Build", "build-up", "Build", "", "", true},
		{"Hook", "chorus", "Hook", "", "", true},
		{"Final Chorus", "chorus", "Final Chorus", "", "", true},
		{"intro-short", "intro", "intro-short", "", "short", true},
		{"Guitar Solo", "instrumental", "Guitar Solo", "", "guitar solo", true},
		{"Interlude", "instrumental", "Interlude", "", "", true},
		{"Fade Out", "outro", "Fade Out", "", "", true},
		{"sound of rain", "", "", "", "", false},
		{"Heavy bass drop, mechanical breathing", "", "", "", "", false},
		{"laughs", "", "", "", "", false},
	} {
		tag, label, number, desc, ok := splitTag(c.in)
		if tag != c.tag || label != c.label || number != c.number || desc != c.desc || ok != c.ok {
			t.Errorf("splitTag(%q) = %q %q %q %q %v, want %q %q %q %q %v", c.in, tag, label, number, desc, ok,
				c.tag, c.label, c.number, c.desc, c.ok)
		}
	}
}

func TestDirectionLine(t *testing.T) {
	for _, c := range []struct {
		in, dir string
		ok      bool
	}{
		{"(Heavy bass drop, mechanical breathing)", "Heavy bass drop, mechanical breathing", true},
		{"*whispered*", "whispered", true},
		{"[sound of rain]", "sound of rain", true},
		{"_spoken_", "spoken", true},
		{"(ooh, ooh)", "", false}, // a backing vocal
		{"(Yeah!)", "", false},
		{"(1, 2, 3, go!)", "", false}, // a shout
		{"(Silence)", "Silence", true},
		{"(la la la)", "", false},
		{"we rise (we rise)", "", false},
		{"(we rise) (we rise)", "", false},
		{"Yeah.", "", false},
		{"()", "", false}, // nothing: no direction, and an empty line to sing gets dropped below
	} {
		dir, ok := directionLine(c.in)
		if dir != c.dir || ok != c.ok {
			t.Errorf("directionLine(%q) = %q %v, want %q %v", c.in, dir, ok, c.dir, c.ok)
		}
	}
}

func TestParseLyrics(t *testing.T) {
	got := parseLyrics(userSong)
	want := []lyricSection{
		{tag: "intro", label: "intro", directions: []string{"Heavy bass drop", "mechanical breathing"},
			lines: []string{"Yeah.", "Black. Pink.", "Watch your step."}},
		{tag: "verse", label: "verse", directions: []string{"whispered"},
			lines: []string{"Chrome on the dance floor, we rise (we rise)", "Nobody sees us"}},
		{tag: "drop", label: "drop", directions: []string{"Distorted synth stabs"}},
		{tag: "chorus", label: "chorus", lines: []string{"Black and pink forever", "(ooh, ooh)"}},
		{tag: "outro", label: "outro", directions: []string{"Fade to silence"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseLyrics:\n got %#v\nwant %#v", got, want)
	}

	// lines before any tag, words after a tag, inline [..] and CRLF
	got = parseLyrics("Hello there\r\n[Chorus: big] Sing it [laughs] loud\r\n")
	want = []lyricSection{
		{lines: []string{"Hello there"}},
		{tag: "chorus", label: "Chorus", desc: []string{"big"}, directions: []string{"laughs"}, lines: []string{"Sing it loud"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseLyrics:\n got %#v\nwant %#v", got, want)
	}
}

func TestTranslateLyrics(t *testing.T) {
	const style = "dark techno, female vocal"
	for _, c := range []struct {
		runtime, lyrics, style string
		want, wantStyle        string
	}{
		{"acestep", userSong, style,
			"[Intro - heavy bass drop, mechanical breathing]\nYeah.\nBlack. Pink.\nWatch your step.\n\n" +
				"[Verse - whispered]\nChrome on the dance floor, we rise (we rise)\nNobody sees us\n\n" +
				"[Drop - distorted synth stabs]\n\n[Chorus]\nBlack and pink forever\n(ooh, ooh)\n\n[Outro - fade to silence]",
			style},
		{"yue2", userSong, style,
			"[Intro]\nYeah.\nBlack. Pink.\nWatch your step.\n\n[Verse]\nChrome on the dance floor, we rise, we rise\n" +
				"Nobody sees us\n\n[Interlude]\n\n[Chorus]\nBlack and pink forever\nooh, ooh\n\n[Outro]",
			style + ", heavy bass drop, mechanical breathing, whispered, distorted synth stabs, fade to silence"},
		{"heartmula", userSong, style,
			"[Intro]\nYeah.\nBlack. Pink.\nWatch your step.\n\n[Verse]\nChrome on the dance floor, we rise, we rise\n" +
				"Nobody sees us\n\n[Interlude]\n\n[Chorus]\nBlack and pink forever\nooh, ooh\n\n[Outro]",
			style + ", heavy bass drop, mechanical breathing, whispered, distorted synth stabs, fade to silence"},
		{"levo2", userSong, style,
			"[intro-short]\n\n[verse]\nYeah.\nBlack. Pink.\nWatch your step.\n\n[verse]\nChrome on the dance floor, we rise, we rise\n" +
				"Nobody sees us\n\n[inst-medium]\n\n[chorus]\nBlack and pink forever\nooh, ooh\n\n[outro-short]",
			style + ", heavy bass drop, mechanical breathing, whispered, distorted synth stabs, fade to silence"},

		// YuE2 gets an ending; words in an outro stay sung
		{"yue2", "[verse]\nOne line", "pop", "[Verse]\nOne line\n\n[Outro]", "pop"},
		{"yue2", "[verse]\nOne line\n[outro]\nBye now", "pop", "[Verse]\nOne line\n\n[Outro]\nBye now", "pop"},
		// sections a model does not have: by whether they carry words
		{"heartmula", "[breakdown]\nSlow\n[pre-chorus]\nUp\n[build-up]\nHigher\n[drop]\nNow\n[breakdown]\n[instrumental]", "",
			"[Bridge]\nSlow\n\n[Prechorus]\nUp\n\n[Prechorus]\nHigher\n\n[Chorus]\nNow\n\n[Interlude]\n\n[Interlude]", ""},
		{"yue2", "[build-up]\nHigher\n[drop]\n\n[outro]", "",
			"[Pre-Chorus]\nHigher\n\n[Interlude]\n\n[Outro]", ""},
		{"acestep", "[Verse 2]\nA\n[build-up]\n[Guitar Solo]\n[instrumental]", "rock",
			"[Verse 2]\nA\n\n[Build]\n\n[Guitar Solo]\n\n[Instrumental]", "rock"},
		{"acestep", "[break]\n(Heavy synth stabs)\nWait.\n[inst]", "",
			"[Break - heavy synth stabs]\nWait.\n\n[Instrumental]", ""},
		// ACE-Step: at most two short phrases in a tag, the rest to the caption; lines before a tag: caption
		{"acestep", "(rain)\n[drop - big]\n(a, b, c, d)", "edm",
			"[Drop - big, a]", "edm, rain, b, c, d"},
		{"acestep", "[drop]\n(Heavy industrial bass drop with high-pitched vocal chops)\n(Beat kicks in)", "",
			"[Drop - beat kicks in]", "heavy industrial bass drop"},
		// the style gets short phrases only, six at most
		{"yue2", "[verse]\n(a, b, c, d, e, f, g)\n(This one is far too long to be a useful style tag at all)\nX", "",
			"[Verse]\nX\n\n[Outro]", "a, b, c, d, e, f"},
		// LeVo: words in wordless sections move to a sung one; lengths are kept
		{"levo2", "[intro-long]\n[verse]\nHi\n[drop]\nGo go\n[breakdown]\n[outro: medium]\nBye", "female, pop",
			"[intro-long]\n\n[verse]\nHi\n\n[chorus]\nGo go\n\n[inst-short]\n\n[verse]\nBye\n\n[outro-medium]", "female, pop"},
		{"levo2", "No tag here (echo)", "", "[verse]\nNo tag here, echo", ""},
		// directions already in the style are not repeated, nor twice
		{"heartmula", "[verse]\n(Piano)\nA\n[chorus]\n(piano, Whispered)\nB", "piano,happy",
			"[Verse]\nA\n\n[Chorus]\nB", "piano,happy, whispered"},
		// nothing to do
		{"heartmula", "", "pop", "", "pop"},
		{"stableaudio3", userSong, style, userSong, style},
	} {
		got, gotStyle := TranslateLyrics(c.runtime, c.lyrics, c.style)
		if got != c.want || gotStyle != c.wantStyle {
			t.Errorf("TranslateLyrics(%s, %q, %q)\n got %q\n     %q\nwant %q\n     %q", c.runtime, c.lyrics, c.style,
				got, gotStyle, c.want, c.wantStyle)
		}
	}
}

// No translation leaves a direction word in the lyrics of the models that sing everything.
func TestTranslateLyricsDropsDirections(t *testing.T) {
	for _, rt := range []string{"yue2", "heartmula", "levo2"} {
		got, style := TranslateLyrics(rt, userSong, "techno")
		for _, w := range []string{"bass drop", "mechanical", "breathing", "whispered", "synth stabs", "(", "*"} {
			if strings.Contains(strings.ToLower(got), w) {
				t.Errorf("%s: %q still in the lyrics:\n%s", rt, w, got)
			}
		}
		if !strings.Contains(style, "mechanical breathing") {
			t.Errorf("%s: the direction is lost: style %q", rt, style)
		}
	}
}

// The whole song the "write-song" prompt wrote (testdata/neon-venom.txt): every direction line leaves the sung text.
func TestTranslateNeonVenom(t *testing.T) {
	b, err := os.ReadFile("testdata/neon-venom.txt")
	if err != nil {
		t.Fatal(err)
	}
	directions := []string{"mechanical breathing", "synth stabs", "aggressive rap", "industrial", "vocal chops",
		"kicks in", "brass blast", "fading echo", "silence"}
	for _, rt := range []string{"acestep", "yue2", "heartmula", "levo2"} {
		got, style := TranslateLyrics(rt, string(b), "K-Pop, Dark Pop")
		t.Logf("%s\nstyle: %s\n%s", rt, style, got)
		for _, l := range strings.Split(got, "\n") {
			if strings.HasPrefix(l, "[") {
				continue // ACE-Step keeps them in its tags
			}
			for _, d := range directions {
				if strings.Contains(strings.ToLower(l), d) {
					t.Errorf("%s sings %q: %q", rt, d, l)
				}
			}
		}
		for _, w := range []string{"Black. Pink.", "Wait.", "NOW!", "Dangerous.", "1, 2, 3, go!", "Take a bow."} {
			if !strings.Contains(got, w) {
				t.Errorf("%s lost the sung %q", rt, w)
			}
		}
	}
}

func TestUnbracket(t *testing.T) {
	for in, want := range map[string]string{
		"we rise (we rise)":        "we rise, we rise",
		"(ooh) we go (ooh)":        "ooh, we go, ooh",
		"Stay! (stay)":             "Stay! stay",
		"open (paren":              "open paren",
		"nothing to do here, okay": "nothing to do here, okay",
	} {
		if got := unbracket(in); got != want {
			t.Errorf("unbracket(%q) = %q, want %q", in, got, want)
		}
	}
}
