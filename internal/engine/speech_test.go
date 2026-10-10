package engine

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSentences(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"Guten Abend! Das ist ein Test. Wirklich?", []string{"Guten Abend!", "Das ist ein Test.", "Wirklich?"}},
		{"Das kostet z. B. 3,50 € bzw. etwas mehr. Danach gehen wir.", []string{"Das kostet z. B. 3,50 € bzw. etwas mehr.", "Danach gehen wir."}},
		{"Wir treffen uns am 3. Oktober bei Dr. Müller. Er kommt um 10 Uhr.", []string{"Wir treffen uns am 3. Oktober bei Dr. Müller.", "Er kommt um 10 Uhr."}},
		{"Das war im Jahr 2024. Danach kam 2025.", []string{"Das war im Jahr 2024.", "Danach kam 2025."}},
		{"Version 3.5 is out, see www.example.org. Great!", []string{"Version 3.5 is out, see www.example.org.", "Great!"}},
		{"„Wer bist du?“, fragte der Fuchs. „Ein Stern!“ Er lachte.", []string{"„Wer bist du?“, fragte der Fuchs.", "„Ein Stern!“", "Er lachte."}},
		{"J. R. R. Tolkien wrote it... Really?! Yes.", []string{"J. R. R. Tolkien wrote it...", "Really?!", "Yes."}},
		{"Mr. Smith arrived, e.g. at noon. It rained", []string{"Mr. Smith arrived, e.g. at noon.", "It rained"}},
	} {
		if got := sentences(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("sentences(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestSplitSentences(t *testing.T) {
	got := splitSentences("Guten Abend! Das ist ein Test der lokalen Sprachausgabe.")
	want := []speechChunk{{Text: "Guten Abend! Das ist ein Test der lokalen Sprachausgabe.", Pause: 0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("short sentence joins the next: %+v", got)
	}

	got = splitSentences("Erster Satz ist lang genug. Zweiter Satz ist auch lang genug. Ja.\nNeue Zeile mit Inhalt hier.\n\n\nNeuer Absatz beginnt hier.")
	want = []speechChunk{
		{Text: "Erster Satz ist lang genug.", Pause: pauseSentence},
		{Text: "Zweiter Satz ist auch lang genug. Ja.", Pause: pauseLine}, // a short tail joins the sentence before
		{Text: "Neue Zeile mit Inhalt hier.", Pause: pauseParagraph},
		{Text: "Neuer Absatz beginnt hier.", Pause: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}

	if got := splitSentences("  \n\n "); len(got) != 0 {
		t.Fatalf("empty text: %+v", got)
	}

	// a long sentence is cut at a comma, never above the limit, and nothing is lost
	long := strings.Repeat("ein Wort nach dem anderen, ", 30) + "und am Ende ein Punkt."
	got = splitSentences(long)
	if len(got) < 2 {
		t.Fatalf("long sentence not split: %d chunks", len(got))
	}
	var words []string
	for _, c := range got {
		if n := len([]rune(c.Text)); n > chunkMax {
			t.Fatalf("chunk of %d runes: %q", n, c.Text)
		}
		if !strings.HasSuffix(c.Text, ",") && !strings.HasSuffix(c.Text, ".") {
			t.Fatalf("not cut at a clause mark: %q", c.Text)
		}
		words = append(words, strings.Fields(c.Text)...)
	}
	if strings.Join(words, " ") != strings.Join(strings.Fields(long), " ") {
		t.Fatal("words lost or changed")
	}
	// no clause marks at all: cut between words
	for _, c := range splitLong(strings.Repeat("Wort ", 200), 50) {
		if len(c) > 50 {
			t.Fatalf("chunk %q", c)
		}
	}
}

type fakeVoices map[string]Voice

func (f fakeVoices) Voice(id string) (Voice, bool) { v, ok := f[id]; return v, ok }

func TestResolveVoice(t *testing.T) {
	sample := filepath.Join(t.TempDir(), "s.wav")
	if err := os.WriteFile(sample, []byte("RIFF"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := fakeVoices{
		"anna-1": {ID: "anna-1", Name: "Anna", Kind: "clone", Sample: sample, Transcript: "Hallo.", Language: "de"},
		"gone-1": {ID: "gone-1", Name: "Gone", Kind: "clone", Sample: sample + ".missing"},
		"oma-1":  {ID: "oma-1", Name: "Oma", Kind: "design", Description: "a calm older woman"},
	}
	byID := map[string]*Model{}
	for _, m := range speechModels() {
		byID[m.ID] = m
	}
	for _, c := range []struct {
		model, voice string
		want         speechVoice
		err          string
	}{
		{"qwen3-tts-1.7b", "", speechVoice{Mode: "default"}, ""},
		{"qwen3-tts-1.7b", "ryan", speechVoice{Mode: "preset", Preset: "Ryan"}, ""},
		{"qwen3-tts-1.7b", "anna-1", speechVoice{Mode: "clone", RefAudio: sample, RefText: "Hallo.", name: "Anna", language: "de"}, ""},
		{"qwen3-tts-1.7b", "oma-1", speechVoice{Mode: "design", Description: "a calm older woman", name: "Oma"}, ""},
		{"qwen3-tts-0.6b", "oma-1", speechVoice{}, "cannot make a voice from a description"},
		{"qwen3-tts-1.7b", "gone-1", speechVoice{}, "sample of voice \"Gone\" is missing"},
		{"qwen3-tts-1.7b", "nobody", speechVoice{}, "presets: Vivian"},
		{"voxtral-tts", "anna-1", speechVoice{}, "cannot clone"},
		{"voxtral-tts", "DE_MALE", speechVoice{Mode: "preset", Preset: "de_male"}, ""},
		{"chatterbox-v3", "default", speechVoice{Mode: "default"}, ""},
		{"chatterbox-v3", "anna-1", speechVoice{Mode: "clone", RefAudio: sample, RefText: "Hallo.", name: "Anna", language: "de"}, ""},
		{"chatterbox-v3", "oma-1", speechVoice{}, "cannot make a voice"},
		{"voxcpm2", "oma-1", speechVoice{Mode: "design", Description: "a calm older woman", name: "Oma"}, ""},
		{"higgs-audio-v3", "ryan", speechVoice{}, "has no preset voices"},
	} {
		got, err := resolveVoice(byID[c.model], lib, c.voice)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s/%s: error %v, want %q", c.model, c.voice, err, c.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s/%s: %+v %v, want %+v", c.model, c.voice, got, err, c.want)
		}
	}
	// no library: presets only
	if _, err := resolveVoice(byID["qwen3-tts-1.7b"], nil, "anna-1"); err == nil {
		t.Error("a library voice without a library")
	}
}

func TestSpeechCatalog(t *testing.T) {
	presets := SpeechPresets()
	if _, ok := presets["qwen3-asr"]; ok {
		t.Error("the transcriber has no voices")
	}
	if len(presets["voxtral-tts"]) != 20 || len(presets["qwen3-tts-1.7b"]) != 9 || len(presets["kokoro-82m"]) != 49 {
		t.Errorf("presets %v", presets)
	}
	for _, m := range speechModels() {
		if m.Licence == "" {
			t.Errorf("%s: no licence", m.ID)
		}
		if speechFamilies[m.workerModel] == nil {
			t.Errorf("%s: no family %q", m.ID, m.workerModel)
		}
		for _, hr := range m.repos {
			if len(hr.rev) != 40 {
				t.Errorf("%s: %s not pinned", m.ID, hr.repo)
			}
		}
		nc := m.ID == "higgs-audio-v3" || m.ID == "voxtral-tts"
		if m.NonCommercial != nc {
			t.Errorf("%s: nonCommercial %v", m.ID, m.NonCommercial)
		}
	}
	tasks := map[string]string{}
	for _, m := range speechModels() {
		tasks[m.ID] = strings.Join(m.Tasks, ",")
	}
	for id, want := range map[string]string{
		"qwen3-tts-1.7b": "clone,design,presets,instruct,speed,language",
		"qwen3-tts-0.6b": "clone,presets,instruct,speed,language",
		"chatterbox-v3":  "clone,presets,instruct,speed,language",
		"voxcpm2":        "clone,design,instruct,speed",
		"higgs-audio-v3": "clone,instruct,speed",
		"voxtral-tts":    "presets,speed,language",
		"kokoro-82m":     "presets,speed,language",
		"qwen3-asr":      "transcribe",
	} {
		if tasks[id] != want {
			t.Errorf("%s tasks %q, want %q", id, tasks[id], want)
		}
	}
	if qwenPart("clone") != "base" || qwenPart("design") != "design" || qwenPart("preset") != "custom" {
		t.Error("qwen parts")
	}
}

func TestExaggerationAndLanguage(t *testing.T) {
	for in, want := range map[string]float64{"": 0.5, "0.8": 0.8, "5": 2, "very dramatic!": 1.3, "ruhig, calm": 0.35, "excited": 0.9, "something": 0.5} {
		if got := exaggeration(in); got != want {
			t.Errorf("exaggeration(%q) = %v, want %v", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"Guten Abend! Das ist ein Test der lokalen Sprachausgabe.": "de",
		"Good evening, this is a test of the speech output.":       "en",
		"Grüße": "de",
		"12345": "",
	} {
		if got := guessLanguage(in); got != want {
			t.Errorf("guessLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWavInfo(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x.wav")
	// 0.5 s of 48 kHz mono 16-bit with a LIST chunk before the data
	data := make([]byte, 48000)
	var b []byte
	le := binary.LittleEndian
	b = append(b, "RIFF"...)
	b = le.AppendUint32(b, uint32(4+8+16+8+4+8+len(data)))
	b = append(b, "WAVE"...)
	b = append(b, "fmt "...)
	b = le.AppendUint32(b, 16)
	b = le.AppendUint16(b, 1)
	b = le.AppendUint16(b, 1)
	b = le.AppendUint32(b, 48000)
	b = le.AppendUint32(b, 96000)
	b = le.AppendUint16(b, 2)
	b = le.AppendUint16(b, 16)
	b = append(b, "LIST"...)
	b = le.AppendUint32(b, 4)
	b = append(b, "INFO"...)
	b = append(b, "data"...)
	b = le.AppendUint32(b, uint32(len(data)))
	b = append(b, data...)
	if err := os.WriteFile(f, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if r, s := wavInfo(f); r != 48000 || s != 0.5 {
		t.Fatalf("wavInfo = %d %v", r, s)
	}
	if r, s := wavInfo(f + ".nope"); r != 0 || s != 0 {
		t.Fatal("missing file")
	}
}
