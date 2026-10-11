package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/philipparndt/ai-connector/internal/audio"
	"github.com/philipparndt/ai-connector/internal/audio/audiotest"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/web"
)

// Music, sound and speech on the board (docs/api.md, "Music, sound and speech"): the generators as advertised, the
// requests as jobs, the speech generator the language picks, the WAV made AAC (on the worker, or here for an older
// one; WAV without an encoder), the waveform, and what generateDone says of each kind.

// fakeAudio is the music, sound or speech backend: a WAV of the asked length (8 kHz mono 16-bit, a rising ramp so
// the waveform is known), stats as the real ones report, and for speech the result (text, voice, language, words).
type fakeAudio struct {
	name string

	mu  sync.Mutex
	got []engine.Request
	big bool // a stereo float WAV at 48 kHz (5.8 MB for 15 s)
}

func (b *fakeAudio) Name() string   { return b.name }
func (b *fakeAudio) Heavy() bool    { return true }
func (b *fakeAudio) Loaded() string { return "" }
func (b *fakeAudio) Unload()        {}

func (b *fakeAudio) Run(ctx context.Context, m *engine.Model, r engine.Request, outDir string, emit engine.Emit) error {
	b.mu.Lock()
	b.got = append(b.got, r)
	big := b.big
	b.mu.Unlock()
	for i := 1; i <= 2; i++ {
		emit.Progress(i, 2)
		time.Sleep(20 * time.Millisecond)
	}
	sec := r.Duration
	if sec == 0 {
		sec = 1.5
	}
	rate, ch, bits, float := 8000, 1, 16, false
	if big {
		rate, ch, bits, float = 48000, 2, 32, true
	}
	n := int(sec * float64(rate))
	frames := make([][]float64, n)
	for i := range frames {
		v := float64(i) / float64(n-1) * 0.8
		frames[i] = []float64{v, v}[:ch]
	}
	out := filepath.Join(outDir, fmt.Sprintf("audio-%d.wav", time.Now().UnixNano()))
	if err := os.WriteFile(out, audiotest.WAV(rate, ch, bits, float, false, frames), 0o644); err != nil {
		return err
	}
	seed := r.Seed
	if seed == 0 {
		seed = 99
	}
	stats := map[string]any{"seed": seed, "duration": sec}
	if r.BPM > 0 {
		stats["bpm"] = r.BPM + 1 // the measured tempo
	}
	if m.Kind == engine.Music {
		stats["faded"] = r.Title != "" // (a song with a title: cut at its length and faded out)
	}
	emit(engine.Event{Type: "stats", Data: stats})
	emit(engine.Event{Type: "file", File: out})
	if m.Kind == engine.Speech {
		res := map[string]any{"text": r.Text, "language": r.Language,
			"voice": engine.PresetVoice{ID: cmpStr(r.Voice, "af_heart"), Name: "Heart (worker)", Language: "en"}}
		if m.ID == "kokoro-82m" && r.Language == "en" {
			res["words"] = []engine.Word{{Text: "Hello", Start: 0.1, End: 0.4}, {Text: "board", Start: 0.5, End: 0.9}}
		}
		emit(engine.Event{Type: "result", Data: res})
	}
	return nil
}

func (b *fakeAudio) last() engine.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.got[len(b.got)-1]
}

type audioMac struct {
	music, sound, speech *fakeAudio
}

var (
	kokoroCaps = &engine.AudioCaps{Voices: []engine.PresetVoice{{ID: "af_heart", Name: "Heart", Language: "en"}, {ID: "bf_emma", Name: "Emma (British)", Language: "en"},
		{ID: "ff_siwis", Name: "Siwis", Language: "fr"}}, Languages: []string{"en", "es", "fr", "it", "pt", "hi", "zh"}, MaxChars: 5000, Words: true}
	qwenCaps = &engine.AudioCaps{Voices: []engine.PresetVoice{{ID: "Ryan", Name: "Ryan", Language: "en"}, {ID: "Vivian", Name: "Vivian", Language: "zh"}},
		Languages: []string{"zh", "en", "de", "fr"}, Instruct: true, MaxChars: 5000}
)

// startAudioMac connects a worker with music, sound and speech models (and FLUX, an image model); noAudioAPI: an
// older worker without POST /api/audio.
func startAudioMac(t *testing.T, base, token string, noAudioAPI bool) *audioMac {
	t.Helper()
	music := engine.Request{Duration: 60}
	cat := &engine.Catalog{Models: []*engine.Model{
		{ID: "flux2-klein-4b", Name: "FLUX.2 [klein] 4B", Kind: engine.Image, Backend: "mflux", Size: gib, Here: true,
			Defaults: engine.Request{Width: 1024, Height: 1024}},
		{ID: "heartmula-3b", Name: "HeartMuLa 3B", Kind: engine.Music, Backend: "music", Size: gib, Here: true, Defaults: engine.Request{Duration: 30},
			Audio: &engine.AudioCaps{MaxSeconds: 240, Lyrics: true, Instrumental: true}},
		{ID: "stable-audio-3-small", Name: "Stable Audio 3 Small (music)", Kind: engine.Music, Backend: "music", Size: gib, Here: true,
			Defaults: music, Audio: &engine.AudioCaps{MaxSeconds: 120, Instrumental: true, BPM: true}, NonCommercial: true},
		{ID: "ace-step-1.5-turbo", Name: "ACE-Step 1.5 turbo", Kind: engine.Music, Backend: "music", Size: gib, Here: true, Defaults: music,
			Audio: &engine.AudioCaps{MaxSeconds: 600, Lyrics: true, Instrumental: true, BPM: true}},
		{ID: "ace-step-1.5-base", Name: "ACE-Step 1.5 base", Kind: engine.Music, Backend: "music", Size: gib, Defaults: music,
			Audio: &engine.AudioCaps{MaxSeconds: 600, Lyrics: true, Instrumental: true, BPM: true}}, // not on disk
		{ID: "moss-soundeffect", Name: "MOSS-SoundEffect", Kind: engine.Sound, Backend: "sound", Size: gib, Here: true,
			Defaults: engine.Request{Duration: 8}, Audio: &engine.AudioCaps{MaxSeconds: 30}},
		{ID: "qwen3-tts-1.7b", Name: "Qwen3-TTS 1.7B", Kind: engine.Speech, Backend: "speech", Size: gib, Here: true, Audio: qwenCaps},
		{ID: "kokoro-82m", Name: "Kokoro 82M", Kind: engine.Speech, Backend: "speech", Size: gib, Here: true, Audio: kokoroCaps},
		{ID: "qwen3-asr", Name: "Qwen3-ASR", Kind: engine.Speech, Backend: "speech", Size: gib, Here: true}, // no caps: no generator
		{ID: "old-music", Name: "Old", Kind: engine.Music, Backend: "music", Size: gib, Here: true},         // an older worker's: no caps
	}}
	m := &audioMac{music: &fakeAudio{name: "music"}, sound: &fakeAudio{name: "sound"}, speech: &fakeAudio{name: "speech"}}
	dir := t.TempDir()
	mgr := jobs.New(cat, []engine.Backend{m.music, m.sound, m.speech, &fakeGen{name: "mflux", ext: ".png", size: 100}}, dir)
	api := (&web.Server{Jobs: mgr, Prompts: prompts.Open(dir), Version: "test"}).Handler()
	if noAudioAPI {
		inner := api
		api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/audio" {
				http.NotFound(w, r)
				return
			}
			inner.ServeHTTP(w, r)
		})
	}
	stop, _ := connectWorkerWith(t, base, token, withRAM(api, 64))
	t.Cleanup(func() {
		stop()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !mgr.Settled(); time.Sleep(5 * time.Millisecond) {
		}
	})
	return m
}

// encoder makes the encoder a fake ffmpeg (the AAC is "AAC:" and the WAV), or none.
func encoder(t *testing.T, fake bool) {
	t.Helper()
	old := audio.Encoders
	audio.Encoders = []string{"ffmpeg"}
	t.Cleanup(func() { audio.Encoders = old })
	if fake {
		audiotest.FakeFFmpeg(t)
	} else {
		t.Setenv("PATH", t.TempDir())
	}
}

// audioWorld: a hub, the worker, a vboard with a connector offering generators.
func audioWorld(t *testing.T, generators string, noAudioAPI bool) (*fakeBridge, *audioMac) {
	t.Helper()
	fastGenerate(t)
	_, base, device := vbHub(t)
	_, wtok := enroll(t, base, device, "Mac")
	mac := startAudioMac(t, base, wtok, noAudioAPI)
	v := newFakeVboard(t, stageGenerators)
	addConnector(t, base, device, `{"name":"Studio AI","url":"`+v.srv.URL+`","token":"vbp_secret","generators":`+generators+`}`)
	return v.next(t), mac
}

// helloRaw is hello.generators as vboard reads it.
func helloRaw(t *testing.T, b *fakeBridge) []map[string]any {
	t.Helper()
	var h struct{ Generators []map[string]any }
	if err := json.Unmarshal(b.raw, &h); err != nil {
		t.Fatal(err)
	}
	return h.Generators
}

// genFile is a generation's file, put together from its pieces; its mime.
func genFile(t *testing.T, evs []vbGenEv) ([]byte, string) {
	t.Helper()
	var file bytes.Buffer
	mime := ""
	for _, ev := range evs {
		if ev.Type != "generateFile" {
			continue
		}
		data, _ := base64.StdEncoding.DecodeString(ev.M["data"].(string))
		file.Write(data)
		mime = ev.M["mime"].(string)
		if ev.M["size"] != float64(0) && ev.M["last"] == true && float64(file.Len()) != ev.M["size"] {
			t.Fatalf("%d bytes of %v", file.Len(), ev.M["size"])
		}
	}
	return file.Bytes(), mime
}

func genDone(t *testing.T, fields string, b *fakeBridge) (first vbGenEv, evs []vbGenEv) {
	t.Helper()
	_, ch := b.generate(fields)
	evs = genUntil(t, ch)
	if evs[len(evs)-1].Type != "generateDone" {
		t.Fatalf("%s: %+v", fields, evs[len(evs)-1])
	}
	return evs[0], evs
}

// The generators: "*" offers music, sound and speech models the worker describes, the defaults first of their kind
// (ACE-Step turbo, Stable Audio 3 Small; Kokoro, Qwen3-TTS), with what each takes; not the transcriber, not a model
// an older worker does not describe, not one off disk.
func TestVboardAudioGenerators(t *testing.T) {
	encoder(t, true)
	b, _ := audioWorld(t, `["*"]`, false)
	gs := helloRaw(t, b)
	var ids []string
	byID := map[string]map[string]any{}
	for _, g := range gs {
		ids = append(ids, g["id"].(string))
		byID[g["id"].(string)] = g
	}
	order := func(kind string) []string {
		var out []string
		for _, g := range gs {
			if g["kind"] == kind {
				out = append(out, g["id"].(string))
			}
		}
		return out
	}
	if m := order("music"); !reflect.DeepEqual(m, []string{"ace-step-1.5-turbo", "stable-audio-3-small", "heartmula-3b"}) {
		t.Fatalf("music %v (all: %v)", m, ids)
	}
	if s := order("speech"); !reflect.DeepEqual(s, []string{"kokoro-82m", "qwen3-tts-1.7b"}) {
		t.Fatalf("speech %v", s)
	}
	if s := order("sound"); !reflect.DeepEqual(s, []string{"moss-soundeffect"}) || len(order("image")) != 1 {
		t.Fatalf("sound %v, all %v", s, ids)
	}
	js := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if got := js(byID["ace-step-1.5-turbo"]); got != `{"bpm":true,"default":{"seconds":60},"id":"ace-step-1.5-turbo","instrumental":true,`+
		`"kind":"music","lyrics":true,"maxSeconds":600,"name":"ACE-Step 1.5 turbo","nonCommercial":false,"worker":"Mac"}` {
		t.Fatalf("ace-step %s", got)
	}
	if got := js(byID["stable-audio-3-small"]); !strings.Contains(got, `"lyrics":false`) || !strings.Contains(got, `"maxSeconds":120`) ||
		!strings.Contains(got, `"nonCommercial":true`) {
		t.Fatalf("stable audio %s", got)
	}
	if got := js(byID["heartmula-3b"]); !strings.Contains(got, `"bpm":false`) || !strings.Contains(got, `"default":{"seconds":30}`) {
		t.Fatalf("heartmula %s", got)
	}
	if got := js(byID["moss-soundeffect"]); got != `{"default":{"seconds":8},"id":"moss-soundeffect","kind":"sound","maxSeconds":30,`+
		`"name":"MOSS-SoundEffect","nonCommercial":false,"worker":"Mac"}` {
		t.Fatalf("sound %s", got)
	}
	if got := js(byID["kokoro-82m"]); got != `{"id":"kokoro-82m","instruct":false,"kind":"speech","languages":["en","es","fr","it","pt","hi","zh"],`+
		`"maxChars":5000,"name":"Kokoro 82M","nonCommercial":false,"voices":[{"id":"af_heart","language":"en","name":"Heart"},`+
		`{"id":"bf_emma","language":"en","name":"Emma (British)"},{"id":"ff_siwis","language":"fr","name":"Siwis"}],"worker":"Mac"}` {
		t.Fatalf("kokoro %s", got)
	}
	if byID["qwen3-tts-1.7b"]["instruct"] != true {
		t.Fatalf("qwen %v", byID["qwen3-tts-1.7b"])
	}
}

// A song: the length at once, the job with the style, lyrics, length, tempo, title and seed, AAC made on the
// worker, and generateDone with the length, the waveform, title, tempo (as measured), lyrics and faded.
func TestVboardMusic(t *testing.T) {
	encoder(t, true)
	b, mac := audioWorld(t, `["*"]`, false)
	first, evs := genDone(t, `"kind":"music","prompt":" dark techno, 130 bpm ","lyrics":"[verse]\nla la la\n[chorus]\noh","seconds":12,
		"bpm":128,"title":"Night Drive","seed":5,"context":{"userName":"Anna"}`, b)
	if first.Type != "generateStatus" || first.M["seconds"] != 12.0 || first.M["width"] != nil || first.M["generator"] != nil {
		t.Fatalf("first %+v", first)
	}
	r := mac.music.last()
	if r.Prompt != "dark techno, 130 bpm" || r.Lyrics != "[verse]\nla la la\n[chorus]\noh" || r.Duration != 12 || r.BPM != 128 ||
		r.Title != "Night Drive" || r.Seed != 5 || r.Instrumental || r.Client != "vboard: Studio AI" {
		t.Fatalf("request %+v", r)
	}
	file, mime := genFile(t, evs)
	if mime != "audio/mp4" || !bytes.HasPrefix(file, []byte("AAC:RIFF")) {
		t.Fatalf("file %s %q", mime, file[:min(len(file), 8)])
	}
	done := evs[len(evs)-1].M
	peaks, _ := done["peaks"].([]any)
	if done["generator"] != "ace-step-1.5-turbo" || done["seconds"] != 12.0 || done["seed"] != 5.0 || done["title"] != "Night Drive" ||
		done["bpm"] != 129.0 || done["lyrics"] != "[verse]\nla la la\n[chorus]\noh" || len(peaks) != 256 || done["width"] != nil ||
		done["faded"] != true {
		t.Fatalf("done %+v", done)
	}
	if p0, p1 := peaks[0].(float64), peaks[255].(float64); p0 > 0.01 || math.Abs(p1-0.8) > 0.01 {
		t.Fatalf("peaks %v … %v", p0, p1)
	}
	// the default length, a title from the style, an instrumental; too long: cut to the generator's longest
	_, evs = genDone(t, `"kind":"music","generator":"stable-audio-3-small","prompt":"lo-fi beat, mellow piano","instrumental":true,"seconds":500`, b)
	done = evs[len(evs)-1].M
	if r := mac.music.last(); r.Duration != 120 || !r.Instrumental || r.Lyrics != "" || done["title"] != "lo-fi beat" ||
		done["lyrics"] != nil || done["bpm"] != nil || done["seed"] != 99.0 || done["faded"] != nil {
		t.Fatalf("request %+v done %+v", r, done)
	}
	_, _ = genDone(t, `"kind":"music","prompt":"a march"`, b)
	if r := mac.music.last(); r.Duration != 60 {
		t.Fatalf("default length %+v", r)
	}
}

// The requests vboard may get wrong.
func TestVboardAudioErrors(t *testing.T) {
	encoder(t, true)
	b, _ := audioWorld(t, `["*"]`, false)
	for fields, want := range map[string]string{
		`"kind":"music"`:                           "a prompt is needed: the style",
		`"kind":"sound","prompt":" "`:              "a prompt is needed: the sound",
		`"kind":"music","prompt":"x","seconds":-1`: "0 (the default) or more",
		`"kind":"music","generator":"stable-audio-3-small","prompt":"x","lyrics":"la"`: "sings no lyrics",
		`"kind":"music","generator":"heartmula-3b","prompt":"x","bpm":120`:             "takes no tempo",
		`"kind":"music","prompt":"x","bpm":500`:                                        "bpm is 30 to 300",
		`"kind":"music","prompt":"x","lyrics":"` + strings.Repeat("a", 8001) + `"`:     "longer than 8000",
		`"kind":"music","prompt":"x","title":"` + strings.Repeat("a", 201) + `"`:       "title is longer",
		`"kind":"speech","text":"  "`:                                                  "a text to read is needed",
		`"kind":"speech","text":"` + strings.Repeat("a", 5001) + `"`:                   "longer than 5000",
		`"kind":"speech","generator":"kokoro-82m","text":"hi","voice":"Ryan"`:          `has no voice "Ryan"`,
		`"kind":"speech","text":"hi","voice":"nobody"`:                                 `no speech generator has the voice "nobody"`,
		`"kind":"speech","text":"Hallo","voice":"af_heart","language":"de"`:            "leave the voice out",
		`"kind":"speech","text":"Habari","language":"sw"`:                              `no speech generator speaks "sw"`,
		`"kind":"sound","prompt":"rain","images":[{"mime":"image/png","data":"AA=="}]`: "takes no images",
		`"generator":"old-music","prompt":"x"`:                                         "is not available now",
		`"generator":"qwen3-asr","text":"x"`:                                           "is not available now",
	} {
		if msg := genError(t, b, fields); !strings.Contains(msg, want) {
			t.Errorf("%s: %q, want %q", fields[:min(len(fields), 80)], msg, want)
		}
	}
}

// Speech: the generator the language picks — Kokoro for English and French, Qwen3-TTS for German (said or seen in
// the text), also past a generator that was named; a voice picks its generator; an instruction prefers one that
// takes it. The first status names the generator; generateDone has the text, the voice, the language and words.
func TestVboardSpeech(t *testing.T) {
	encoder(t, true)
	b, mac := audioWorld(t, `["*"]`, false)
	first, evs := genDone(t, `"kind":"speech","text":" Hello board, how are you? "`, b)
	r := mac.speech.last()
	if first.M["generator"] != "kokoro-82m" || first.M["name"] != "Kokoro 82M" || first.M["seconds"] != 1.8 || r.Text != "Hello board, how are you?" ||
		r.Language != "en" || r.Voice != "" || r.Instruct != "" {
		t.Fatalf("first %+v request %+v", first.M, r)
	}
	file, mime := genFile(t, evs)
	if mime != "audio/mp4" || !bytes.HasPrefix(file, []byte("AAC:RIFF")) {
		t.Fatalf("file %s", mime)
	}
	done := evs[len(evs)-1].M
	if done["generator"] != "kokoro-82m" || done["text"] != "Hello board, how are you?" || done["language"] != "en" ||
		!reflect.DeepEqual(done["voice"], map[string]any{"id": "af_heart", "name": "Heart"}) || done["seconds"] != 1.5 ||
		!reflect.DeepEqual(done["words"], []any{map[string]any{"text": "Hello", "start": 0.1, "end": 0.4},
			map[string]any{"text": "board", "start": 0.5, "end": 0.9}}) || len(done["peaks"].([]any)) != 256 {
		t.Fatalf("done %+v", done)
	}
	for _, c := range []struct {
		fields, gen, voice, lang, instruct string
	}{
		{`"text":"Guten Morgen, wie geht es dir heute?"`, "qwen3-tts-1.7b", "", "de", ""},                    // seen in the text
		{`"text":"Hallo","language":"de-DE"`, "qwen3-tts-1.7b", "", "de", ""},                                // said
		{`"generator":"kokoro-82m","text":"Hallo zusammen","language":"de"`, "qwen3-tts-1.7b", "", "de", ""}, // past the named one
		{`"text":"Bonjour","language":"fr"`, "kokoro-82m", "", "fr", ""},
		{`"text":"Bonjour","voice":"ff_siwis"`, "kokoro-82m", "ff_siwis", "fr", ""},                            // the voice's language
		{`"text":"Hello there","voice":"ryan"`, "qwen3-tts-1.7b", "Ryan", "en", ""},                            // the voice's generator
		{`"text":"How are you today?","instruct":"warm, slow"`, "qwen3-tts-1.7b", "", "en", "warm, slow"},      // one that listens
		{`"generator":"kokoro-82m","text":"How are you today?","instruct":"warm"`, "kokoro-82m", "", "en", ""}, // named: dropped
		{`"text":"xyz qrs"`, "kokoro-82m", "", "", ""},                                                         // cannot tell: the default decides
	} {
		first, evs := genDone(t, `"kind":"speech",`+c.fields, b)
		r := mac.speech.last()
		done := evs[len(evs)-1].M
		if first.M["generator"] != c.gen || done["generator"] != c.gen || r.Voice != c.voice || r.Language != c.lang || r.Instruct != c.instruct {
			t.Errorf("%s: status %v, request voice %q language %q instruct %q", c.fields, first.M["generator"], r.Voice, r.Language, r.Instruct)
		}
		if done["words"] != nil && (c.gen != "kokoro-82m" || c.lang != "en") {
			t.Errorf("%s: words %v", c.fields, done["words"])
		}
	}
}

// A sound: its default length, and nothing more in generateDone than the length and the waveform.
func TestVboardSound(t *testing.T) {
	encoder(t, true)
	b, mac := audioWorld(t, `["moss-soundeffect"]`, false)
	first, evs := genDone(t, `"kind":"sound","prompt":"a door creaking","seed":3`, b)
	if r := mac.sound.last(); first.M["seconds"] != 8.0 || r.Prompt != "a door creaking" || r.Duration != 8 || r.Seed != 3 || r.Steps != 0 {
		t.Fatalf("first %v request %+v", first.M, r)
	}
	done := evs[len(evs)-1].M
	want := map[string]bool{"type": true, "id": true, "generator": true, "seed": true, "took": true, "seconds": true, "peaks": true}
	for k := range done {
		if !want[k] {
			t.Errorf("done has %q: %v", k, done)
		}
	}
	_, _ = genDone(t, `"kind":"sound","prompt":"thunder","seconds":0.2`, b)
	if r := mac.sound.last(); r.Duration != 1 {
		t.Fatalf("at least 1 s: %+v", r)
	}
}

// No encoder anywhere: the WAV as it is (audio/wav) while it fits vboard's limit, else an error.
func TestVboardAudioWAV(t *testing.T) {
	encoder(t, false)
	b, mac := audioWorld(t, `["*"]`, false)
	_, evs := genDone(t, `"kind":"sound","prompt":"rain"`, b)
	file, mime := genFile(t, evs)
	if mime != "audio/wav" || !bytes.HasPrefix(file, []byte("RIFF")) {
		t.Fatalf("file %s", mime)
	}
	mac.sound.mu.Lock()
	mac.sound.big = true
	mac.sound.mu.Unlock()
	old := genMaxAudioFile
	genMaxAudioFile = 4 << 20
	t.Cleanup(func() { genMaxAudioFile = old })
	_, ch := b.generate(`"kind":"sound","prompt":"rain","seconds":15`)
	evs = genUntil(t, ch)
	if last := evs[len(evs)-1]; last.Type != "error" || !strings.Contains(last.M["message"].(string), "more than vboard takes (4 MB)") {
		t.Fatalf("too large: %+v", last)
	}
}

// An older worker (no POST /api/audio): its WAV comes to the hub, which makes it AAC and the waveform itself; a
// stereo float WAV.
func TestVboardAudioOlderWorker(t *testing.T) {
	encoder(t, true)
	b, mac := audioWorld(t, `["*"]`, true)
	mac.music.mu.Lock()
	mac.music.big = true
	mac.music.mu.Unlock()
	_, evs := genDone(t, `"kind":"music","prompt":"strings","seconds":3`, b)
	file, mime := genFile(t, evs)
	done := evs[len(evs)-1].M
	peaks := done["peaks"].([]any)
	if mime != "audio/mp4" || !bytes.HasPrefix(file, []byte("AAC:RIFF")) || done["seconds"] != 3.0 || len(peaks) != 256 ||
		math.Abs(peaks[255].(float64)-0.8) > 0.01 {
		t.Fatalf("file %s, done %v", mime, done["seconds"])
	}
}

// The real encoder (afconvert on macOS, else ffmpeg) when there is one: an MP4.
func TestVboardAudioRealEncoder(t *testing.T) {
	if name, _ := audio.Encoder(); name == "" {
		t.Skip("no AAC encoder")
	}
	b, _ := audioWorld(t, `["*"]`, false)
	_, evs := genDone(t, `"kind":"speech","text":"Hello there, general reader."`, b)
	file, mime := genFile(t, evs)
	if mime != "audio/mp4" || len(file) < 100 || !bytes.Contains(file[:64], []byte("ftyp")) {
		t.Fatalf("file %s %d bytes", mime, len(file))
	}
}
