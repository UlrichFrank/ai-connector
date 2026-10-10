package engine

import (
	"slices"
	"strings"
)

// AudioCaps is what a music, sound or speech model takes — the part vboard's generators show (docs/api.md, "Music,
// sound and speech"); the hub reads it from the catalogue.
type AudioCaps struct {
	// music, sound: the longest piece it makes (seconds); the default length is Defaults.Duration
	MaxSeconds float64 `json:"maxSeconds,omitempty"`
	// music: it sings given lyrics, it can leave the voice out, it takes a tempo
	Lyrics       bool `json:"lyrics,omitempty"`
	Instrumental bool `json:"instrumental,omitempty"`
	BPM          bool `json:"bpm,omitempty"`
	// speech: its preset voices, the languages it speaks (none: it detects them), whether it follows an instruction
	// (how to say it), the longest text, and whether it gives word timings
	Voices    []PresetVoice `json:"voices,omitempty"`
	Languages []string      `json:"languages,omitempty"`
	Instruct  bool          `json:"instruct,omitempty"`
	MaxChars  int           `json:"maxChars,omitempty"`
	Words     bool          `json:"words,omitempty"`
}

// PresetVoice is one of a speech model's own voices.
type PresetVoice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Language string `json:"language,omitempty"` // the language it is made for ("" any)
}

// speechMaxChars is the longest text a speech request on the board takes (the worker says it sentence by sentence).
const speechMaxChars = 5000

// musicLongest is how long a song model makes a song at most: its own limit, else its runtime's (ACE-Step 1.5's
// DURATION_MAX, levo-cli's limit), else 240 s (HeartMuLa's and YuE2's practical length).
func musicLongest(m *Model) float64 {
	if m.maxSeconds > 0 {
		return m.maxSeconds
	}
	switch m.runtime {
	case "acestep":
		return 600
	case "levo2":
		return 270
	}
	return 240
}

// musicCaps: every song model can make an instrumental; Stable Audio 3 sings nothing; the tempo is honoured by
// ACE-Step (its own field) and by Stable Audio 3 and MiniMax Music 3 (in the description).
func musicCaps(m *Model) *AudioCaps {
	return &AudioCaps{MaxSeconds: musicLongest(m), Lyrics: m.runtime != "stableaudio3", Instrumental: true,
		BPM: m.runtime == "acestep" || m.runtime == "stableaudio3" || m.runtime == "minimax3"}
}

// speechCaps: the family's presets (with their languages), the languages it is told (or speaks), instruct as
// speechTasks says.
func speechCaps(m *Model) *AudioCaps {
	f := speechFamilies[m.workerModel]
	if f == nil || m.workerModel == "asr" {
		return nil
	}
	c := &AudioCaps{Languages: f.speaks, Instruct: slices.Contains(m.Tasks, "instruct"), MaxChars: speechMaxChars, Words: f.words}
	if f.langs != nil {
		c.Languages = f.langs
	}
	for _, p := range f.presets {
		c.Voices = append(c.Voices, presetVoice(m.workerModel, p))
	}
	return c
}

// qwenVoiceLangs: the language each Qwen3-TTS preset is made for (each speaks all ten).
var qwenVoiceLangs = map[string]string{"Vivian": "zh", "Serena": "zh", "Uncle_Fu": "zh", "Dylan": "zh", "Eric": "zh",
	"Ryan": "en", "Aiden": "en", "Ono_Anna": "ja", "Sohee": "ko"}

// presetVoice names a preset and says its language.
func presetVoice(family, id string) PresetVoice {
	v := PresetVoice{ID: id, Name: strings.ReplaceAll(id, "_", " ")}
	switch family {
	case "kokoro":
		lang, british := kokoroLanguage(id)
		name := id
		if _, after, ok := strings.Cut(id, "_"); ok {
			name = strings.ToUpper(after[:1]) + after[1:]
		}
		if british {
			name += " (British)"
		}
		v.Name, v.Language = name, lang
	case "qwen3-tts":
		v.Language = qwenVoiceLangs[id]
	case "chatterbox":
		v.Name, v.Language = "Default", "en"
	case "voxtral":
		// casual_male, de_female …: the language in front, else English
		lang, rest, _ := strings.Cut(id, "_")
		if len(lang) == 2 {
			v.Language, v.Name = lang, strings.ToUpper(lang)+" "+rest
		} else {
			v.Language = "en"
		}
	}
	return v
}
