package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/philipparndt/ai-connector/internal/audio"
)

// Music, sound and speech on vboard's board (docs/api.md, "Music, sound and speech"): generators of the kinds music,
// sound and speech, described by the worker's catalogue (engine.AudioCaps). The worker writes a WAV; it makes it
// AAC itself (POST /api/audio: only the small file goes through the tunnel) and computes the waveform's peaks — an
// older worker's WAV is fetched and made AAC here when this computer has an encoder.

const (
	genLyricsChars   = 8000
	genTitleChars    = 200
	genInstructChars = 500
	genMinBPM        = 30
	genMaxBPM        = 300
	genPeaks         = 256       // the waveform's numbers
	genAudioRead     = 512 << 20 // an older worker's WAV, fetched to make it AAC here
	speechCharsASec  = 14        // speech: the length to be made is estimated from the text
)

// genMaxAudioFile is vboard's limit for an audio file (a WAV over it, with no encoder, is refused).
var genMaxAudioFile int64 = 100 << 20

// the bit rates: songs and sounds in stereo, speech in mono
const (
	aacMusic  = 192000
	aacSpeech = 96000
)

func audioKind(kind string) bool { return kind == "music" || kind == "sound" || kind == "speech" }

// genDefaults are the generators "*" puts first of their kind (the first of a kind is its default): songs with
// lyrics, then short instrumentals; Kokoro (fast, small), then Qwen3-TTS (German and instructions).
var genDefaults = map[string]int{"ace-step-1.5-turbo": 1, "stable-audio-3-small": 2, "kokoro-82m": 1, "qwen3-tts-1.7b": 2}

func genRank(id string) int {
	if r, ok := genDefaults[id]; ok {
		return r
	}
	return 10
}

// audioGenerator is a music, sound or speech model as a generator: what the worker's catalogue says it takes (a
// worker that does not say it — an older one — offers none).
func audioGenerator(m catalogModel, g advertisedGenerator) (advertisedGenerator, bool) {
	a := m.Audio
	if a == nil || m.Backend != m.Kind {
		return g, false
	}
	def := 0.0
	if m.Defaults != nil {
		def = m.Defaults.Duration
	}
	switch m.Kind {
	case "music":
		g.Lyrics, g.Instrumental, g.BPM = a.Lyrics, a.Instrumental, a.BPM
		g.MaxSeconds = cmpOr(int(a.MaxSeconds), 240)
		g.Seconds = min(cmpOr(int(def), 60), g.MaxSeconds)
	case "sound":
		g.MaxSeconds = cmpOr(int(a.MaxSeconds), 30)
		g.Seconds = min(cmpOr(int(def), 5), g.MaxSeconds)
	case "speech":
		if a.MaxChars <= 0 { // the transcriber
			return g, false
		}
		g.Voices, g.Languages, g.Instruct, g.MaxChars = a.Voices, a.Languages, a.Instruct, a.MaxChars
	}
	return g, true
}

func wireAudioGenerator(g advertisedGenerator) map[string]any {
	w := map[string]any{"id": g.ID, "name": g.Name, "kind": g.Kind, "worker": g.Worker, "nonCommercial": g.NonCommercial}
	switch g.Kind {
	case "music":
		w["lyrics"], w["instrumental"], w["bpm"] = g.Lyrics, g.Instrumental, g.BPM
		w["default"], w["maxSeconds"] = map[string]any{"seconds": g.Seconds}, g.MaxSeconds
	case "sound":
		w["default"], w["maxSeconds"] = map[string]any{"seconds": g.Seconds}, g.MaxSeconds
	case "speech":
		voices, langs := g.Voices, g.Languages
		if voices == nil {
			voices = []genVoice{}
		}
		if langs == nil {
			langs = []string{}
		}
		w["voices"], w["languages"], w["instruct"], w["maxChars"] = voices, langs, g.Instruct, g.MaxChars
	}
	return w
}

// ---------- speech: which generator reads it ----------

// speechGenerators are the speech generators on the connection, in order.
func (c *bridgeConn) speechGenerators() []advertisedGenerator {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	var out []advertisedGenerator
	for _, g := range c.caps.generators {
		if g.Kind == "speech" {
			out = append(out, g)
		}
	}
	return out
}

func (g advertisedGenerator) voice(id string) (genVoice, bool) {
	for _, v := range g.Voices {
		if strings.EqualFold(v.ID, id) {
			return v, true
		}
	}
	return genVoice{}, false
}

// speaks: the generator speaks lang (one without languages detects them: any).
func (g advertisedGenerator) speaks(lang string) bool {
	return lang == "" || len(g.Languages) == 0 || slices.Contains(g.Languages, lang)
}

// normLanguage is a language as the generators list it: "en-US" → "en", "pt_BR" → "pt".
func normLanguage(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "-_"); i > 0 {
		s = s[:i]
	}
	return s
}

// pickSpeech decides which speech generator reads the text (docs/api.md): the one asked for (or the first), unless
// it does not speak the language — then the next one that does (from the voice's language, else the request's,
// else as the text looks: a German text goes past Kokoro); a voice belongs to its generator (none named: the first
// that has it); an instruction prefers one that takes it when nothing else decides. It sets the request's language
// (that of the generator's own voice when none was given; "" when it cannot tell) and voice (its id as listed).
func (c *bridgeConn) pickSpeech(g advertisedGenerator, q *genRequest) (advertisedGenerator, error) {
	named := q.Generator != ""
	all := c.speechGenerators()
	voice := strings.TrimSpace(q.Voice)
	lang := normLanguage(q.Language)
	if voice != "" {
		if _, ok := g.voice(voice); !ok {
			if named {
				return g, fmt.Errorf("%s has no voice %q (its voices: %s)", g.Name, voice, voiceIDs(g))
			}
			i := slices.IndexFunc(all, func(x advertisedGenerator) bool { _, ok := x.voice(voice); return ok })
			if i < 0 {
				return g, fmt.Errorf("no speech generator has the voice %q", voice)
			}
			g = all[i]
		}
		v, _ := g.voice(voice)
		voice = v.ID
		if lang == "" {
			lang = v.Language
		}
		if !g.speaks(lang) {
			return g, fmt.Errorf("%s does not speak %q (it speaks: %s): leave the voice out for another generator",
				g.Name, lang, strings.Join(g.Languages, ", "))
		}
	} else {
		guessed := false
		if lang == "" {
			lang, guessed = audio.GuessLanguage(q.Text), true
		}
		ok := func(x advertisedGenerator) bool { return x.speaks(lang) }
		if q.Instruct != "" && !named && !g.Instruct && slices.ContainsFunc(all, func(x advertisedGenerator) bool { return x.Instruct && ok(x) }) {
			ok = func(x advertisedGenerator) bool { return x.Instruct && x.speaks(lang) } // how to say it: one that listens
		}
		if !ok(g) {
			// the next one that does, after it in the list, then from the start
			at := slices.IndexFunc(all, func(x advertisedGenerator) bool { return x.ID == g.ID })
			next := -1
			for k := 1; k <= len(all) && next < 0; k++ {
				if x := all[(at+k)%len(all)]; ok(x) {
					next = (at + k) % len(all)
				}
			}
			switch {
			case next >= 0:
				g = all[next]
			case guessed:
				lang = "" // a guess nobody speaks: the generator decides
			default:
				return g, fmt.Errorf("no speech generator speaks %q (%s speaks: %s)", lang, g.Name, strings.Join(g.Languages, ", "))
			}
		}
	}
	if !g.speaks(lang) {
		lang = ""
	}
	q.Voice, q.Language = voice, lang
	return g, nil
}

func voiceIDs(g advertisedGenerator) string {
	if len(g.Voices) == 0 {
		return "none: its own voice"
	}
	ids := make([]string, len(g.Voices))
	for i, v := range g.Voices {
		ids[i] = v.ID
	}
	return strings.Join(ids, ", ")
}

// ---------- the request ----------

// planAudio checks a music, sound or speech request and works out the job: the length to be made (music and
// sound: seconds, 0 the default, at most the generator's; speech: an estimate from the text) and its fields.
func planAudio(g advertisedGenerator, q *genRequest) (genPlan, error) {
	p := genPlan{job: map[string]any{}}
	if q.Seed < 0 || q.Seconds < 0 || q.BPM < 0 {
		return p, errors.New("seed, seconds and bpm are 0 (the default) or more")
	}
	if len(q.Images) > 0 {
		return p, fmt.Errorf("%s takes no images", g.Name)
	}
	if g.Kind == "speech" {
		text := strings.TrimSpace(q.Text)
		switch {
		case text == "":
			return p, errors.New("a text to read is needed")
		case len([]rune(text)) > g.MaxChars:
			return p, fmt.Errorf("the text is longer than %d characters", g.MaxChars)
		case len([]rune(q.Instruct)) > genInstructChars:
			return p, fmt.Errorf("instruct is longer than %d characters", genInstructChars)
		}
		p.job["text"] = text
		if q.Voice != "" {
			p.job["voice"] = q.Voice
		}
		if q.Language != "" {
			p.job["language"] = q.Language
		}
		if in := strings.TrimSpace(q.Instruct); in != "" && g.Instruct {
			p.job["instruct"] = in
		}
		p.seconds = max(1, math.Round(float64(len([]rune(text)))/speechCharsASec*10)/10)
		return p, nil
	}
	q.Prompt = strings.TrimSpace(q.Prompt)
	switch {
	case q.Prompt == "" && g.Kind == "music":
		return p, errors.New("a prompt is needed: the style (genre, mood, instruments)")
	case q.Prompt == "":
		return p, errors.New("a prompt is needed: the sound to make")
	case len([]rune(q.Prompt)) > genPromptChars:
		return p, fmt.Errorf("the prompt is longer than %d characters", genPromptChars)
	}
	sec := q.Seconds
	if sec == 0 {
		sec = float64(g.Seconds)
	}
	p.seconds = min(max(sec, 1), float64(g.MaxSeconds))
	p.job["prompt"], p.job["duration"] = q.Prompt, p.seconds
	if g.Kind == "sound" {
		return p, nil
	}
	lyrics, title := strings.TrimSpace(q.Lyrics), strings.TrimSpace(q.Title)
	switch {
	case len([]rune(lyrics)) > genLyricsChars:
		return p, fmt.Errorf("the lyrics are longer than %d characters", genLyricsChars)
	case lyrics != "" && !g.Lyrics:
		return p, fmt.Errorf("%s makes instrumentals: it sings no lyrics", g.Name)
	case q.Instrumental && !g.Instrumental:
		return p, fmt.Errorf("%s cannot leave the voice out", g.Name)
	case q.BPM > 0 && !g.BPM:
		return p, fmt.Errorf("%s takes no tempo (bpm)", g.Name)
	case q.BPM > 0 && (q.BPM < genMinBPM || q.BPM > genMaxBPM):
		return p, fmt.Errorf("bpm is %d to %d", genMinBPM, genMaxBPM)
	case len([]rune(title)) > genTitleChars:
		return p, fmt.Errorf("the title is longer than %d characters", genTitleChars)
	}
	if lyrics != "" {
		p.job["lyrics"] = lyrics
	}
	if q.Instrumental {
		p.job["instrumental"] = true
	}
	if q.BPM > 0 {
		p.job["bpm"] = q.BPM
	}
	if title != "" {
		p.job["title"] = title
	}
	q.Lyrics, q.Title = lyrics, title
	return p, nil
}

// songTitle names a song: the title asked for, else the style's first part ("dark techno, 130 bpm" → "dark techno").
func songTitle(q *genRequest) string {
	if q.Title != "" {
		return q.Title
	}
	t, _, _ := strings.Cut(q.Prompt, ",")
	t = strings.TrimSpace(strings.Split(t, "\n")[0])
	if r := []rune(t); len(r) > 60 {
		t = strings.TrimSpace(string(r[:60])) + "…"
	}
	return t
}

// ---------- the file and the end ----------

// audioFile is the worker's answer to POST /api/audio.
type audioFile struct {
	File    string    `json:"file"`
	MIME    string    `json:"mime"`
	Size    int64     `json:"size"`
	Seconds float64   `json:"seconds"`
	Peaks   []float64 `json:"peaks"`
}

// sendAudio sends a music, sound or speech job's file: AAC made on the worker (or here for an older worker), the
// WAV when nobody can make AAC and it fits vboard's limit. It returns the length and the waveform.
func (r *vboardRun) sendAudio(ctx context.Context, c *bridgeConn, id string, n *node, g advertisedGenerator, j *genJob, fail func(string)) (float64, []float64, bool) {
	wav := ""
	for _, f := range j.Files { // the first is the song; a tempo-locked song has the model's own after it
		if strings.ToLower(path.Ext(f)) == ".wav" {
			wav = f
			break
		}
	}
	if wav == "" {
		fail(fmt.Sprintf("the job made no %s file", g.Kind))
		return 0, nil, false
	}
	bitrate, channels := aacMusic, 2
	if g.Kind == "speech" {
		bitrate, channels = aacSpeech, 1
	}
	a, err := r.s.encodeOn(ctx, n, wav, bitrate, channels)
	if err == nil {
		res, err := r.s.fleet.request(ctx, n, http.MethodGet, a.File, nil, "")
		if err != nil {
			fail(fmt.Sprintf("the file from %s: %v", n.name, err))
			return 0, nil, false
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			fail(fmt.Sprintf("the file from %s: %s", n.name, res.Status))
			return 0, nil, false
		}
		size := a.Size
		if res.ContentLength >= 0 {
			size = res.ContentLength
		}
		if size > genMaxAudioFile {
			fail(fmt.Sprintf("the %s is %.1f MB, more than vboard takes (%d MB)", g.Kind, float64(size)/(1<<20), genMaxAudioFile>>20))
			return 0, nil, false
		}
		return a.Seconds, a.Peaks, r.sendPieces(ctx, c, id, n, a.MIME, size, res.Body, fail)
	}
	if ctx.Err() != nil {
		return 0, nil, false
	}
	// an older worker (no /api/audio) or its encoder failed: the WAV comes here
	r.logf("generate %s: %s did not make AAC (%v): the WAV comes here", id, n.name, err)
	res, err := r.s.fleet.request(ctx, n, http.MethodGet, wav, nil, "")
	if err != nil {
		fail(fmt.Sprintf("the file from %s: %v", n.name, err))
		return 0, nil, false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		fail(fmt.Sprintf("the file from %s: %s", n.name, res.Status))
		return 0, nil, false
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, genAudioRead+1))
	if err != nil {
		fail(fmt.Sprintf("the file from %s: %v", n.name, err))
		return 0, nil, false
	}
	if len(b) > genAudioRead {
		fail(fmt.Sprintf("the %s is more than %d MB", g.Kind, genAudioRead>>20))
		return 0, nil, false
	}
	peaks, info, err := audio.Peaks(b, genPeaks)
	if err != nil {
		fail(fmt.Sprintf("the %s: %v", g.Kind, err))
		return 0, nil, false
	}
	mime, body := "audio/wav", b
	if m4a, err := encodeHere(ctx, b, bitrate, channels); err == nil {
		mime, body = "audio/mp4", m4a
	} else if !errors.Is(err, audio.ErrNoEncoder) {
		r.logf("generate %s: making AAC here: %v", id, err)
	}
	if int64(len(body)) > genMaxAudioFile {
		fail(fmt.Sprintf("the %s is %.1f MB as WAV, more than vboard takes (%d MB), and no AAC encoder (afconvert, ffmpeg) is there",
			g.Kind, float64(len(body))/(1<<20), genMaxAudioFile>>20))
		return 0, nil, false
	}
	return info.Seconds, peaks, r.sendPieces(ctx, c, id, n, mime, int64(len(body)), bytes.NewReader(body), fail)
}

// encodeOn asks the worker to make its WAV AAC (POST /api/audio).
func (s *Server) encodeOn(ctx context.Context, n *node, file string, bitrate, channels int) (audioFile, error) {
	var a audioFile
	b, _ := json.Marshal(map[string]any{"file": file, "bitrate": bitrate, "channels": channels, "peaks": genPeaks})
	res, err := s.fleet.request(ctx, n, http.MethodPost, "/api/audio", bytes.NewReader(b), "application/json")
	if err != nil {
		return a, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return a, fmt.Errorf("%s %s", res.Status, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(res.Body).Decode(&a); err != nil {
		return a, err
	}
	if a.File == "" || (a.MIME != "audio/mp4" && a.MIME != "audio/wav") {
		return a, fmt.Errorf("an answer without a file (%q, %q)", a.File, a.MIME)
	}
	return a, nil
}

// encodeHere makes a WAV AAC on this computer.
func encodeHere(ctx context.Context, wav []byte, bitrate, channels int) ([]byte, error) {
	if name, _ := audio.Encoder(); name == "" {
		return nil, audio.ErrNoEncoder
	}
	dir, err := os.MkdirTemp("", "vboard-audio-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "in.wav"), filepath.Join(dir, "out.m4a")
	if err := os.WriteFile(in, wav, 0o600); err != nil {
		return nil, err
	}
	if _, err := audio.ToAAC(ctx, in, out, bitrate, channels); err != nil {
		return nil, err
	}
	return os.ReadFile(out)
}

// audioDone is {generateDone} of a music, sound or speech job: the length, the seed, the waveform, and what each
// kind is (speech: the text, voice, language and the words' times; music: title, tempo, lyrics, faded).
func audioDone(g advertisedGenerator, q *genRequest, plan genPlan, j *genJob, seconds float64, peaks []float64) map[string]any {
	done := map[string]any{"type": "generateDone", "generator": g.ID, "seed": q.Seed, "peaks": peaks,
		"took": math.Round(j.Finished.Sub(j.Started).Seconds()*10) / 10}
	if v, ok := j.stat("seed"); ok {
		done["seed"] = int64(v)
	}
	if seconds <= 0 {
		seconds, _ = j.stat("duration")
	}
	if seconds <= 0 {
		seconds = plan.seconds
	}
	done["seconds"] = math.Round(seconds*100) / 100
	switch g.Kind {
	case "speech":
		res := j.Result
		done["text"], done["language"] = cmpStr(str(res["text"]), str(plan.job["text"])), cmpStr(str(res["language"]), q.Language)
		if v, ok := res["voice"].(map[string]any); ok && str(v["id"]) != "" {
			name := str(v["name"])
			if gv, ok := g.voice(str(v["id"])); ok {
				name = gv.Name
			}
			done["voice"] = map[string]any{"id": v["id"], "name": cmpStr(name, str(v["id"]))}
		}
		if w, ok := res["words"].([]any); ok && len(w) > 0 {
			done["words"] = w
		}
	case "music":
		done["title"] = songTitle(q)
		if v, ok := j.stat("bpm"); ok && v > 0 {
			done["bpm"] = int(math.Round(v))
		} else if q.BPM > 0 {
			done["bpm"] = q.BPM
		}
		if q.Lyrics != "" && !q.Instrumental && g.Lyrics {
			done["lyrics"] = q.Lyrics
		}
		if f, _ := j.Stats["faded"].(bool); f { // cut at the length and faded out (audio.FadeCut)
			done["faded"] = true
		}
	}
	return done
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
