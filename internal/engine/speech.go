package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/pyenv"
)

// Speech: text to speech with a voice library (docs/voice-avatar.md), and Qwen3-ASR for transcribing (voice
// samples, "transcribe" jobs). Every model runs in one Python worker (workers/speech.py) on mlx-audio.

// mlxAudioCommit is mlx-audio's main after v0.5.7 (2026-10-03): v0.5.7 plus a Voxtral TTS first-frame fix.
const mlxAudioCommit = "543792553ba62fe7cdb9e0b22c505bfa899c0f09"

// SpeechEnv is the speech runtime, pinned (verified on an M1 Max, 64 GB).
var SpeechEnv = pyenv.Env{Name: "speech", Python: "3.12", Packages: []string{
	"mlx-audio[tts,stt] @ git+https://github.com/Blaizzy/mlx-audio@" + mlxAudioCommit,
	"mlx==0.32.3", "numpy==2.3.5", "transformers==5.18.0", // numpy < 2.4: mistral-common
	"mistral-common[audio]==1.12.0", // Voxtral's tekken.json (voice_num_audio_tokens) needs ≥ 1.12
	"torch==2.14.1",                 // Higgs v3: its codec's bf16 weights are read through PyTorch (CPU, at load)
}}

// speechFamily is what a model family can do with a request; see speechFamilies.
type speechFamily struct {
	presets  []string                 // the model's own voices
	preset   func(lang string) string // the default preset for a language ("" none: the model's own default voice)
	clone    bool                     // speaks with a voice of the library made from a sample
	design   bool                     // speaks with a voice of the library made from a description
	speed    bool                     // honours speed itself (else the wav is time-stretched afterwards, pitch kept)
	langs    []string                 // languages it needs to be told (nil: it detects them)
	honoured string                   // what of the request it honours, for the model's "about"
}

var qwenPresets = []string{"Vivian", "Serena", "Uncle_Fu", "Dylan", "Eric", "Ryan", "Aiden", "Ono_Anna", "Sohee"}

var voxtralPresets = []string{"casual_male", "casual_female", "cheerful_female", "neutral_male", "neutral_female",
	"fr_male", "fr_female", "es_male", "es_female", "de_male", "de_female", "it_male", "it_female", "pt_male",
	"pt_female", "nl_male", "nl_female", "ar_male", "hi_male", "hi_female"}

// speechFamilies: the worker's model families (Model.workerModel).
var speechFamilies = map[string]*speechFamily{
	// Qwen3-TTS: CustomVoice speaks the presets and follows instruct; Base clones (sample + transcript); VoiceDesign
	// makes a voice from its description. Language is passed (10 languages, German among them); speed by stretching.
	"qwen3-tts": {presets: qwenPresets, preset: func(string) string { return "Ryan" }, clone: true, design: true,
		honoured: "presets with instruct (emotion, style), cloning, voice design (1.7B), language"},
	// Chatterbox Multilingual v3: one default voice (Resemble's), clones from a sample; instruct sets the emotion
	// exaggeration (a number 0–2, or words: calm … dramatic). Needs the language (23).
	"chatterbox": {presets: []string{"default"}, preset: func(string) string { return "default" }, clone: true,
		langs: []string{"ar", "da", "de", "el", "en", "es", "fi", "fr", "he", "hi", "it", "ja", "ko", "ms", "nl", "no",
			"pl", "pt", "ru", "sv", "sw", "tr", "zh"},
		honoured: "cloning, instruct as emotion exaggeration, language"},
	// VoxCPM2: no presets (a random voice per seed), clones (with the transcript: continuation, closer), designs;
	// instruct is a style description. Detects the language (30).
	"voxcpm2": {clone: true, design: true, honoured: "cloning, voice design, instruct as a style description; 48 kHz"},
	// Higgs Audio v3: no presets (a random voice per seed), clones; instruct becomes its control tags (emotion,
	// whisper, shout, speed, pitch) or is passed as tags (<|emotion:…|>); speed as tags.
	"higgs": {clone: true, speed: true, honoured: "cloning, instruct as control tags (emotions, whisper/shout), speed as tags"},
	// Voxtral TTS: 20 presets (two German); no cloning in the open weights, no instruct.
	"voxtral": {presets: voxtralPresets, preset: func(lang string) string {
		switch lang {
		case "de", "fr", "es", "it", "pt", "nl", "hi":
			return lang + "_female"
		case "ar":
			return "ar_male"
		}
		return "neutral_female"
	}, honoured: "presets (the language picks the default one)"},
	"asr": {},
}

// SpeechPresets are the preset voices of each speech model (GET /api/voices).
func SpeechPresets() map[string][]string {
	out := map[string][]string{}
	for _, m := range speechModels() {
		if f := speechFamilies[m.workerModel]; f != nil && m.workerModel != "asr" {
			out[m.ID] = append([]string{}, f.presets...)
		}
	}
	return out
}

func qwenRepos(size string, revs ...string) []hubRepo {
	keys := []string{"custom", "base", "design"}
	var out []hubRepo
	for i, rev := range revs {
		variant := map[string]string{"custom": "CustomVoice", "base": "Base", "design": "VoiceDesign"}[keys[i]]
		out = append(out, hubRepo{key: keys[i], repo: "mlx-community/Qwen3-TTS-12Hz-" + size + "-" + variant + "-bf16", rev: rev})
	}
	return out
}

// asrRepo is Qwen3-ASR 1.7B (8-bit): transcribes the voice samples and the "transcribe" jobs.
var asrRepo = hubRepo{key: "asr", repo: "mlx-community/Qwen3-ASR-1.7B-8bit", rev: "a8379a2e2f9e313c9292cdf1af4055ab56d50d55"}

// speechModels are the catalogue's speech models.
func speechModels() []*Model {
	d := Request{Speed: 1}
	ms := []*Model{
		{ID: "qwen3-tts-1.7b", Name: "Qwen3-TTS 1.7B", Kind: Speech, Backend: "speech", workerModel: "qwen3-tts",
			repos:   qwenRepos("1.7B", "52f4770fd9726457eae3d3b6aa92047a25a10776", "a6eb4f68e4b056f1215157bb696209bc82a6db48", "7d3824abff87e49756bb0f83fb5411de75d160c4"),
			Licence: "Apache-2.0", Size: 12 * gb, Defaults: d,
			About: "Qwen3-TTS 12Hz 1.7B (bf16): 9 preset voices that follow an instruction (emotion, style), cloning from a sample, voices designed from a description; 10 languages incl. German. Each part (CustomVoice, Base, VoiceDesign, 4 GB each) is fetched when first used"},
		{ID: "qwen3-tts-0.6b", Name: "Qwen3-TTS 0.6B", Kind: Speech, Backend: "speech", workerModel: "qwen3-tts",
			repos:   qwenRepos("0.6B", "6415d95f88be018ff9e46813119dc3bc12261328", "1eccf1cb2519b5a4e8a95b5f0544f3303568164f"),
			Licence: "Apache-2.0", Size: 4670 << 20, Defaults: d,
			About: "Qwen3-TTS 12Hz 0.6B (bf16): the small one, faster; presets with instruct and cloning, no voice design"},
		{ID: "chatterbox-v3", Name: "Chatterbox Multilingual v3", Kind: Speech, Backend: "speech", workerModel: "chatterbox",
			repos: []hubRepo{
				{key: "model", repo: "mlx-community/chatterbox-multilingual-v3", rev: "03565773edd72e949572557597af8063bb49a18a"},
				{key: "conds", repo: "mlx-community/chatterbox-fp16", rev: "4923fcca09086356aeab5191a2348c5a17a23694", include: []string{"conds.safetensors"}},
				{key: "s3tok", repo: "mlx-community/S3TokenizerV2", rev: "e0c9886f0e1c35ae85b1f27277416fb19fc72bec", include: []string{"model.safetensors", "config.json"}},
			},
			Licence: "MIT", Size: 3000 << 20, Defaults: d,
			About: "Resemble AI's Chatterbox Multilingual v3 (23 languages): clones a voice from a few seconds; instruct sets the emotion exaggeration (0–2 or calm … dramatic)"},
		{ID: "voxcpm2", Name: "VoxCPM2", Kind: Speech, Backend: "speech", workerModel: "voxcpm2",
			repos:   []hubRepo{{key: "model", repo: "mlx-community/VoxCPM2-bf16", rev: "a7fb80d9056983135a54ca3f71fcc5a637ccddf5"}},
			Licence: "Apache-2.0", Size: 4730 << 20, Defaults: d,
			About: "OpenBMB's VoxCPM2 2B (bf16), 48 kHz studio quality, 30 languages: cloning, voices designed from a description, instruct as a style"},
		{ID: "higgs-audio-v3", Name: "Higgs Audio v3 TTS 4B", Kind: Speech, Backend: "speech", workerModel: "higgs",
			repos: []hubRepo{{key: "model", repo: "bosonai/higgs-tts-3-4b", rev: "239f63fb7b02b1aa085f98d9efae5e35cc5523e8",
				include: []string{"*.json", "*.safetensors", "*.jinja", "LICENSE"}}},
			Licence: "Boson Higgs Audio v3 Research and Non-Commercial License", NonCommercial: true, Size: 8730 << 20, Defaults: d,
			About: "Boson AI's Higgs Audio v3 4B: expressive, clones from a sample; instruct becomes control tags (emotions, whisper, shout, speed) or put tags like <|emotion:amusement|> into the text. Non-commercial"},
		{ID: "voxtral-tts", Name: "Voxtral 4B TTS", Kind: Speech, Backend: "speech", workerModel: "voxtral",
			repos:   []hubRepo{{key: "model", repo: "mlx-community/Voxtral-4B-TTS-2603-mlx-bf16", rev: "dd85c02adbae551f5bb29ded35ee60ccdfb90927"}},
			Licence: "CC BY-NC 4.0", NonCommercial: true, Size: 7670 << 20, Defaults: d,
			About: "Mistral's Voxtral 4B TTS (bf16): 20 natural preset voices in 9 languages (de_male, de_female …); no cloning. Non-commercial"},
		{ID: "qwen3-asr", Name: "Qwen3-ASR 1.7B (transcribe)", Kind: Speech, Backend: "speech", workerModel: "asr",
			repos: []hubRepo{asrRepo}, Licence: "Apache-2.0", Size: 2360 << 20,
			About: "Qwen3-ASR 1.7B (8-bit): speech to text in 30 languages; a job with an audio file gives its transcript (the voice library uses it for samples)"},
	}
	for _, m := range ms {
		m.Tasks = speechTasks(m)
	}
	return ms
}

// speechTasks are what a speech model honours, for the UI: clone, design, presets, instruct, speed, language (the
// transcriber: transcribe).
func speechTasks(m *Model) []string {
	if m.workerModel == "asr" {
		return []string{"transcribe"}
	}
	f := speechFamilies[m.workerModel]
	var t []string
	if f.clone {
		t = append(t, "clone")
	}
	if f.design && (m.workerModel != "qwen3-tts" || hasRepo(m, "design")) {
		t = append(t, "design")
	}
	if len(f.presets) > 0 {
		t = append(t, "presets")
	}
	if m.workerModel != "voxtral" { // Voxtral has no instruction; the others take it (see speechFamilies)
		t = append(t, "instruct")
	}
	t = append(t, "speed") // the model's own (Higgs) or by time-stretching
	if m.workerModel == "qwen3-tts" || m.workerModel == "chatterbox" || m.workerModel == "voxtral" {
		t = append(t, "language") // told the language (Voxtral: it picks the default preset)
	}
	return t
}

// coreRepos are the repositories a model needs to count as here (Qwen3-TTS: its preset part; Base and VoiceDesign
// come when first used).
func coreRepos(m *Model) []hubRepo {
	if m.workerModel == "qwen3-tts" {
		return m.repos[:1]
	}
	return m.repos
}

// reposHere: the store has every file of these repositories.
func reposHere(store *models.Store, repos []hubRepo) bool {
	for _, hr := range repos {
		if _, err := os.Stat(filepath.Join(store.Dir, filepath.FromSlash(hr.repo), hr.rev, ".files.json")); err != nil {
			return false
		}
		hm, err := store.FromHub(context.Background(), hr.repo, hr.repo, hr.rev, hr.keep()) // from the cached list
		if err != nil || store.Missing(hm) > 0 {
			return false
		}
	}
	return true
}

func (hr hubRepo) keep() func(string) bool {
	if len(hr.include) == 0 {
		return nil
	}
	inc := hr.include
	return func(p string) bool { return models.Matches(p, inc...) }
}

// VoiceStore looks voices of the library up (internal/voices implements it).
type VoiceStore interface {
	Voice(id string) (Voice, bool)
}

// SpeechBackend runs the speech models in one mlx-audio worker; it keeps the last TTS model (and Qwen3-ASR, once
// used) loaded. It is the Speaker of the avatar backend.
type SpeechBackend struct {
	Root    string
	Store   *models.Store
	Catalog *Catalog   // Speak looks its model up here
	Voices  VoiceStore // the voice library (nil: presets only)

	mu    sync.Mutex
	w     *pyWorker
	state status
}

var _ Speaker = (*SpeechBackend)(nil)

func (b *SpeechBackend) Name() string   { return "speech" }
func (b *SpeechBackend) Heavy() bool    { return true }
func (b *SpeechBackend) Loaded() string { return b.state.get() }

func (b *SpeechBackend) Unload() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.w != nil {
		b.w.stop()
	}
	b.state.set("")
}

// Run says r.Text into a wav in outDir, or (qwen3-asr) transcribes r.Audio.
func (b *SpeechBackend) Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m.workerModel == "asr" {
		if r.Audio == "" {
			return errors.New("an audio file to transcribe")
		}
		if _, err := os.Stat(r.Audio); err != nil {
			return fmt.Errorf("the audio to transcribe: %w", err)
		}
		t0 := time.Now()
		text, err := b.transcribe(ctx, r.Audio, r.Language, emit)
		if err != nil {
			return err
		}
		emit(Event{Type: "text", Text: text})
		emit(Event{Type: "result", Data: map[string]any{"text": text}})
		emit(Event{Type: "stats", Data: map[string]any{"seconds": time.Since(t0).Seconds(), "duration": wavSeconds(r.Audio)}})
		return nil
	}
	if r.Seed == 0 {
		r.Seed = rand.Int64N(1_000_000_000)
	}
	out := filepath.Join(outDir, fmt.Sprintf("%s-%d.wav", time.Now().Format("20060102-150405"), r.Seed))
	if err := b.speak(ctx, m, r, out, emit); err != nil {
		return err
	}
	emit(Event{Type: "file", File: out})
	return nil
}

// Speak says r.Text with r.Voice into outFile, in this process (the avatar backend runs it inside its own job).
// model "" is the first speech model on this machine (else Qwen3-TTS 1.7B).
func (b *SpeechBackend) Speak(ctx context.Context, model string, r Request, outFile string, emit Emit) error {
	m, err := b.speechModel(model)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Seed == 0 {
		r.Seed = rand.Int64N(1_000_000_000)
	}
	return b.speak(ctx, m, r, outFile, emit)
}

func (b *SpeechBackend) speechModel(id string) (*Model, error) {
	var all []*Model
	if b.Catalog != nil {
		all = b.Catalog.OfKind(Speech)
	} else {
		all = speechModels()
	}
	var tts []*Model
	for _, m := range all {
		if m.Backend == "speech" && m.workerModel != "asr" {
			tts = append(tts, m)
		}
	}
	if id == "" {
		for _, m := range tts {
			if m.Here {
				return m, nil
			}
		}
		for _, m := range tts {
			if m.ID == "qwen3-tts-1.7b" {
				return m, nil
			}
		}
	}
	for _, m := range tts {
		if m.ID == id {
			return m, nil
		}
	}
	return nil, fmt.Errorf("no speech model %q", id)
}

// PreviewVoice says a short text with a designed voice of the library (Qwen3-TTS 1.7B VoiceDesign) into out: the
// library keeps it as the voice's sample.
func (b *SpeechBackend) PreviewVoice(ctx context.Context, v Voice, out string) error {
	text := "Hello! This is how my voice sounds. I hope you like it."
	if v.Language == "de" {
		text = "Hallo! So klingt meine Stimme. Ich hoffe, sie gefällt dir."
	}
	return b.Speak(ctx, "qwen3-tts-1.7b", Request{Text: text, Voice: v.ID, Language: v.Language}, out, func(Event) {})
}

// Transcribe writes down what an audio file says (Qwen3-ASR; language "" detects it).
func (b *SpeechBackend) Transcribe(ctx context.Context, file, language string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.transcribe(ctx, file, language, func(Event) {})
}

func (b *SpeechBackend) transcribe(ctx context.Context, file, language string, emit Emit) (string, error) {
	dir, err := b.fetchRepo(ctx, "Qwen3-ASR", asrRepo, emit)
	if err != nil {
		return "", err
	}
	if err := b.worker(ctx, emit); err != nil {
		return "", err
	}
	if b.state.get() == "" {
		b.state.set("qwen3-asr")
	}
	ev, err := b.w.call(ctx, map[string]any{"op": "asr", "dirs": map[string]string{"asr": dir}, "audio": file,
		"language": language}, emit)
	if err != nil {
		b.afterError()
		return "", err
	}
	return strings.TrimSpace(ev.Text), nil
}

// fetchRepo makes sure one pinned repository is in the store and returns its folder.
func (b *SpeechBackend) fetchRepo(ctx context.Context, name string, hr hubRepo, emit Emit) (string, error) {
	hm, err := b.Store.FromHub(ctx, name, hr.repo, hr.rev, hr.keep())
	if errors.Is(err, models.ErrGated) {
		return "", GatedError(hr.repo)
	}
	if err != nil {
		return "", err
	}
	return fetch(ctx, b.Store, hm, emit)
}

func (b *SpeechBackend) worker(ctx context.Context, emit Emit) error {
	if b.w == nil {
		b.w = &pyWorker{env: SpeechEnv, script: "speech.py"}
	}
	if b.w.running() {
		return nil
	}
	b.state.set("")
	return b.w.start(ctx, b.Root, emit)
}

func (b *SpeechBackend) afterError() {
	if b.w == nil || !b.w.running() {
		b.state.set("")
	}
}

// speak does the work of Run and Speak (b.mu held).
func (b *SpeechBackend) speak(ctx context.Context, m *Model, r Request, out string, emit Emit) error {
	fam := speechFamilies[m.workerModel]
	if fam == nil || m.workerModel == "asr" {
		return fmt.Errorf("%s does not speak", m.Name)
	}
	text := strings.TrimSpace(r.Text)
	if text == "" {
		text = strings.TrimSpace(r.Prompt)
	}
	if text == "" {
		return errors.New("a text to say")
	}
	v, err := resolveVoice(m, b.Voices, r.Voice)
	if err != nil {
		return err
	}
	lang := strings.ToLower(strings.TrimSpace(r.Language))
	if lang == "" {
		lang = v.language
	}
	if lang == "" {
		lang = guessLanguage(text)
	}
	if v.Mode == "default" && fam.preset != nil {
		v.Mode, v.Preset = "preset", fam.preset(lang)
	}
	if fam.langs != nil && !slices.Contains(fam.langs, lang) {
		if r.Language != "" {
			return fmt.Errorf("%s does not speak %q (it speaks: %s)", m.Name, lang, strings.Join(fam.langs, ", "))
		}
		lang = "en"
	}
	chunks := splitSentences(text)

	// the repositories: Qwen3-TTS needs the part of its voice (presets: CustomVoice, clone: Base, design: VoiceDesign)
	dirs := map[string]string{}
	for _, hr := range m.repos {
		key := hr.key
		if m.workerModel == "qwen3-tts" {
			if hr.key != qwenPart(v.Mode) {
				continue
			}
			key = "model"
		}
		dir, err := b.fetchRepo(ctx, m.Name, hr, emit)
		if err != nil {
			return err
		}
		dirs[key] = dir
	}
	if dirs["model"] == "" {
		return fmt.Errorf("%s cannot speak with a %s voice", m.Name, v.Mode)
	}
	if err := b.worker(ctx, emit); err != nil {
		return err
	}
	req := map[string]any{"op": "tts", "family": m.workerModel, "dirs": dirs, "voice": v, "instruct": r.Instruct,
		"language": lang, "seed": r.Seed, "chunks": chunks, "output": out}
	if m.workerModel == "chatterbox" {
		req["exaggeration"] = exaggeration(r.Instruct)
	}
	if fam.speed {
		req["speed"] = r.Speed
	}
	emit(Event{Type: "stats", Data: map[string]any{"seed": r.Seed, "voice": v.label(), "sentences": len(chunks), "language": lang}})
	emit.Log(m.Name + " honours: " + fam.honoured)
	b.state.set(m.ID) // loading counts as loaded: it takes the memory
	ev, err := b.w.call(ctx, req, emit)
	if err != nil {
		b.afterError()
		return err
	}
	if !fam.speed && r.Speed > 0 && (r.Speed < 0.99 || r.Speed > 1.01) {
		emit.Status(fmt.Sprintf("speed ×%.2f …", r.Speed))
		if err := stretch(ctx, out, r.Speed); err != nil {
			return err
		}
	}
	rate, dur := wavInfo(out)
	stats := map[string]any{"seconds": ev.Seconds, "duration": dur, "sampleRate": rate}
	if dur > 0 {
		stats["rtf"] = ev.Seconds / dur
	}
	if ev.PeakGB > 0 {
		stats["peakGB"] = ev.PeakGB
	}
	emit(Event{Type: "stats", Data: stats})
	emit.Log(fmt.Sprintf("%.1f s of speech in %.1f s", dur, ev.Seconds))
	return nil
}

// qwenPart is the Qwen3-TTS checkpoint a voice needs.
func qwenPart(mode string) string {
	switch mode {
	case "clone":
		return "base"
	case "design":
		return "design"
	}
	return "custom"
}

// speechVoice is a request's voice as the worker gets it.
type speechVoice struct {
	Mode        string `json:"mode"` // preset | clone | design | default (the model's own voice)
	Preset      string `json:"preset,omitempty"`
	RefAudio    string `json:"ref_audio,omitempty"`
	RefText     string `json:"ref_text,omitempty"`
	Description string `json:"description,omitempty"`
	name        string // the library voice's name
	language    string // the library voice's language
}

func (v speechVoice) label() string {
	switch {
	case v.name != "":
		return v.name
	case v.Preset != "":
		return v.Preset
	}
	return v.Mode
}

// resolveVoice: a voice of the library (by id, else by name) → its sample and transcript (clone) or description
// (design); else one of the model's presets (case does not matter); "" → the model's default.
func resolveVoice(m *Model, lib VoiceStore, name string) (speechVoice, error) {
	fam := speechFamilies[m.workerModel]
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "default") {
		return speechVoice{Mode: "default"}, nil
	}
	if lib != nil {
		if lv, ok := lib.Voice(name); ok {
			sv := speechVoice{Mode: lv.Kind, name: lv.Name, language: lv.Language}
			switch lv.Kind {
			case "clone":
				if !fam.clone {
					return sv, fmt.Errorf("%s cannot clone a voice: pick one of its presets (%s)", m.Name, strings.Join(fam.presets, ", "))
				}
				if _, err := os.Stat(lv.Sample); err != nil {
					return sv, fmt.Errorf("the sample of voice %q is missing: %w", lv.Name, err)
				}
				sv.RefAudio, sv.RefText = lv.Sample, lv.Transcript
			case "design":
				if !fam.design || (m.workerModel == "qwen3-tts" && !hasRepo(m, "design")) {
					return sv, fmt.Errorf("%s cannot make a voice from a description (Qwen3-TTS 1.7B and VoxCPM2 can)", m.Name)
				}
				sv.Description = lv.Description
			default:
				return sv, fmt.Errorf("voice %q: unknown kind %q", lv.Name, lv.Kind)
			}
			return sv, nil
		}
	}
	for _, p := range fam.presets {
		if strings.EqualFold(p, name) {
			if p == "default" {
				return speechVoice{Mode: "default"}, nil
			}
			return speechVoice{Mode: "preset", Preset: p}, nil
		}
	}
	if len(fam.presets) == 0 {
		return speechVoice{}, fmt.Errorf("no voice %q: %s has no preset voices, use one of the library", name, m.Name)
	}
	return speechVoice{}, fmt.Errorf("no voice %q (not in the library; %s's presets: %s)", name, m.Name, strings.Join(fam.presets, ", "))
}

func hasRepo(m *Model, key string) bool {
	return slices.ContainsFunc(m.repos, func(hr hubRepo) bool { return hr.key == key })
}

// exaggeration maps an instruction onto Chatterbox's emotion exaggeration (0.5 neutral … 2 extreme): a number as it
// is, else words.
func exaggeration(instruct string) float64 {
	s := strings.ToLower(strings.TrimSpace(instruct))
	if s == "" {
		return 0.5
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return min(max(f, 0), 2)
	}
	for _, w := range []struct {
		words []string
		v     float64
	}{
		{[]string{"extreme", "furious", "hysteri", "screaming", "shouting", "over the top", "very dramatic"}, 1.3},
		{[]string{"dramatic", "excited", "angry", "emotional", "passionate", "energetic", "enthusias", "intense"}, 0.9},
		{[]string{"lively", "happy", "cheerful", "expressive", "amused", "warm"}, 0.7},
		{[]string{"monoton", "flat", "robotic"}, 0.25},
		{[]string{"calm", "neutral", "soft", "quiet", "gentle", "relaxed", "slow", "serious"}, 0.35},
	} {
		for _, k := range w.words {
			if strings.Contains(s, k) {
				return w.v
			}
		}
	}
	return 0.5
}

// guessLanguage tells German from English (for the models that must be told; "" when it cannot tell).
func guessLanguage(text string) string {
	de, en := 0, 0
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) }) {
		switch w {
		case "der", "die", "das", "und", "ist", "nicht", "ein", "eine", "ich", "du", "wir", "sie", "es", "mit", "auf",
			"für", "zu", "den", "dem", "von", "auch", "sich", "noch", "wie", "aber", "heute", "guten", "danke", "bitte":
			de++
		case "the", "and", "is", "not", "a", "an", "i", "you", "we", "they", "it", "with", "on", "for", "to", "of",
			"also", "this", "that", "are", "be", "what", "how", "but", "today", "good", "thanks", "please":
			en++
		}
		if strings.ContainsAny(w, "äöüß") {
			de += 2
		}
	}
	switch {
	case de > en:
		return "de"
	case en > de:
		return "en"
	}
	return ""
}

// speechChunk is one piece the model says at once, and the silence after it.
type speechChunk struct {
	Text  string  `json:"text"`
	Pause float64 `json:"pause"`
}

const (
	chunkMin       = 24  // a shorter sentence goes with the next one ("Hallo!" alone sounds clipped)
	chunkMax       = 280 // a longer one is split at commas (the models drift on long inputs)
	pauseSentence  = 0.25
	pauseLine      = 0.4
	pauseParagraph = 0.7
)

// splitSentences splits a text into sentences for saying one at a time (a line break and a paragraph end one too); the
// worker joins them with short pauses: longer after a line, longer still after a paragraph.
func splitSentences(text string) []speechChunk {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var out []speechChunk
	for _, para := range splitParagraphs(text) {
		for _, line := range strings.Split(para, "\n") {
			line = strings.Join(strings.Fields(line), " ")
			if line == "" {
				continue
			}
			var lineChunks []string
			pending := ""
			for _, s := range sentences(line) {
				if pending != "" {
					s, pending = pending+" "+s, ""
				}
				if utf8.RuneCountInString(s) < chunkMin {
					pending = s
					continue
				}
				lineChunks = append(lineChunks, splitLong(s, chunkMax)...)
			}
			if pending != "" { // a short tail joins the sentence before it, else stands alone
				if n := len(lineChunks); n > 0 && utf8.RuneCountInString(lineChunks[n-1]+pending) < chunkMax {
					lineChunks[n-1] += " " + pending
				} else {
					lineChunks = append(lineChunks, pending)
				}
			}
			for _, c := range lineChunks {
				out = append(out, speechChunk{Text: c, Pause: pauseSentence})
			}
			out[len(out)-1].Pause = pauseLine
		}
		if n := len(out); n > 0 {
			out[n-1].Pause = pauseParagraph
		}
	}
	if n := len(out); n > 0 {
		out[n-1].Pause = 0
	}
	return out
}

func splitParagraphs(text string) []string {
	var out, cur []string
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == "" {
			if len(cur) > 0 {
				out = append(out, strings.Join(cur, "\n"))
				cur = nil
			}
			continue
		}
		cur = append(cur, l)
	}
	if len(cur) > 0 {
		out = append(out, strings.Join(cur, "\n"))
	}
	return out
}

// abbreviations end in a dot without ending the sentence (lower case, inner dots kept).
var abbreviations = map[string]bool{}

func init() {
	for _, a := range strings.Fields(`dr prof hr hrn fr frl nr st str ca bzw usw vgl ggf evtl bspw bzgl inkl exkl zzgl
		z.b d.h u.a o.ä u.u s.o s.u v.a mr mrs ms jr sr vs etc e.g i.e inc ltd co no approx dept fig jan feb mar apr jun
		jul aug sep sept oct okt nov dec dez mio mrd tel abs art bd geb gest dipl ing mag dt engl franz lt gem min max
		ff hl mt sog`) {
		abbreviations[a] = true
	}
}

var months = map[string]bool{"januar": true, "februar": true, "märz": true, "april": true, "mai": true, "juni": true,
	"juli": true, "august": true, "september": true, "oktober": true, "november": true, "dezember": true}

const (
	sentenceMarks = ".!?…"
	closingMarks  = `"'»«“”„‘’)]`
)

// sentences splits one line at sentence ends: . ! ? … (and closing quotes), not inside numbers or addresses, not
// after abbreviations, initials or ordinals ("am 3. Oktober").
func sentences(line string) []string {
	rs := []rune(line)
	var out []string
	start := 0
	for i := 0; i < len(rs); i++ {
		if !strings.ContainsRune(sentenceMarks, rs[i]) {
			continue
		}
		j := i
		for j+1 < len(rs) && strings.ContainsRune(sentenceMarks, rs[j+1]) {
			j++
		}
		k := j
		for k+1 < len(rs) && strings.ContainsRune(closingMarks, rs[k+1]) {
			k++
		}
		if k+1 < len(rs) && !unicode.IsSpace(rs[k+1]) {
			i = j
			continue // 3.5, www.example.org
		}
		if j == i && rs[i] == '.' && !sentenceEnd(rs[start:i], nextWord(rs[k+1:])) {
			i = j
			continue
		}
		if s := strings.TrimSpace(string(rs[start : k+1])); s != "" {
			out = append(out, s)
		}
		start, i = k+1, k
	}
	if s := strings.TrimSpace(string(rs[start:])); s != "" {
		out = append(out, s)
	}
	return out
}

func nextWord(rs []rune) string {
	if f := strings.Fields(string(rs)); len(f) > 0 {
		return f[0]
	}
	return ""
}

// sentenceEnd: does the dot after before end the sentence (next: the word after it)?
func sentenceEnd(before []rune, next string) bool {
	f := strings.Fields(string(before))
	if next == "" || len(f) == 0 {
		return true
	}
	word := strings.TrimLeft(f[len(f)-1], closingMarks+"([")
	if abbreviations[strings.ToLower(word)] {
		return false
	}
	if rs := []rune(word); len(rs) == 1 && unicode.IsLetter(rs[0]) {
		return false // an initial: J. R. R. Tolkien, z. B.
	}
	nr, _ := utf8.DecodeRuneInString(strings.TrimLeft(next, closingMarks+"(["))
	if allDigits(word) && len(word) <= 2 && (unicode.IsLower(nr) || months[strings.ToLower(strings.Trim(next, ",.;:"))]) {
		return false // an ordinal: am 3. Oktober
	}
	return !unicode.IsLower(nr)
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// splitLong splits a sentence longer than max runes at its last clause mark (, ; : – —) that fits, else between
// words.
func splitLong(s string, max int) []string {
	var out, cur []string
	n := 0
	for _, w := range strings.Fields(s) {
		wl := utf8.RuneCountInString(w) + 1
		if n+wl > max && len(cur) > 0 {
			cut := len(cur) - 1 // between words
			for k := len(cur) - 1; k > 0; k-- {
				if strings.HasSuffix(cur[k], ",") || strings.HasSuffix(cur[k], ";") || strings.HasSuffix(cur[k], ":") || cur[k] == "–" || cur[k] == "—" {
					cut = k
					break
				}
			}
			out = append(out, strings.Join(cur[:cut+1], " "))
			cur = append([]string{}, cur[cut+1:]...)
			n = 0
			for _, r := range cur {
				n += utf8.RuneCountInString(r) + 1
			}
		}
		cur = append(cur, w)
		n += wl
	}
	if len(cur) > 0 {
		out = append(out, strings.Join(cur, " "))
	}
	return out
}

// stretch changes the speed of a wav, keeping the pitch (ffmpeg atempo, 0.5–2).
func stretch(ctx context.Context, file string, speed float64) error {
	speed = min(max(speed, 0.5), 2)
	tmp := strings.TrimSuffix(file, ".wav") + ".tempo.wav"
	out, err := exec.CommandContext(ctx, "ffmpeg", "-y", "-v", "error", "-i", file, "-filter:a",
		"atempo="+strconv.FormatFloat(speed, 'f', 3, 64), tmp).CombinedOutput()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("changing the speed (ffmpeg): %v %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, file)
}

// wavInfo reads a PCM wav's sample rate and length from its header.
func wavInfo(file string) (rate int, seconds float64) {
	f, err := os.Open(file)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var hdr [12]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil || string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return 0, 0
	}
	var byteRate uint32
	for {
		var ch [8]byte
		if _, err := io.ReadFull(f, ch[:]); err != nil {
			return rate, 0
		}
		size := binary.LittleEndian.Uint32(ch[4:8])
		switch string(ch[0:4]) {
		case "fmt ":
			b := make([]byte, size+size%2)
			if _, err := io.ReadFull(f, b); err != nil || len(b) < 12 {
				return 0, 0
			}
			rate, byteRate = int(binary.LittleEndian.Uint32(b[4:8])), binary.LittleEndian.Uint32(b[8:12])
		case "data":
			if byteRate == 0 {
				return rate, 0
			}
			if size == 0 || size == 0xffffffff { // streamed: the rest of the file
				if st, err := f.Stat(); err == nil {
					pos, _ := f.Seek(0, io.SeekCurrent)
					size = uint32(st.Size() - pos)
				}
			}
			return rate, float64(size) / float64(byteRate)
		default:
			if _, err := f.Seek(int64(size+size%2), io.SeekCurrent); err != nil {
				return rate, 0
			}
		}
	}
}

// wavSeconds is a sound file's length (the wav header; ffprobe for other formats).
func wavSeconds(file string) float64 {
	if _, s := wavInfo(file); s > 0 {
		return s
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", file).Output()
	if err != nil {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return f
}
