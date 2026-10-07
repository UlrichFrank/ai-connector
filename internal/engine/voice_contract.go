package engine

import "context"

// This file is the contract between the voice parts (written before them): the speech backend (speech.go, an
// mlx-audio worker), the avatar backend (avatar.go) and the sound backend (sound.go). See docs/voice-avatar.md.

// Voice is one voice of the library (<config dir>/voices/voices.json; the sample files next to it). A cloned voice
// has a sample of the speaker (5–15 s, mono) and its transcript; a designed voice has a description; a preset is a
// model's own named voice (not stored in the library).
type Voice struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`                  // "clone" | "design"
	Sample      string `json:"sample,omitempty"`      // clone: the audio file (absolute path; the API shows a /files/ url)
	Transcript  string `json:"transcript,omitempty"`  // clone: what the sample says (transcribed when added)
	Description string `json:"description,omitempty"` // design: "a calm older woman, low voice, slight Bavarian accent"
	Language    string `json:"language,omitempty"`
	Created     string `json:"created"`
}

// Speaker says a text with a voice into a wav file, in this process (not as a job: the avatar backend runs inside a
// job and the queue runs one job at a time). The speech backend implements it.
type Speaker interface {
	Speak(ctx context.Context, model string, r Request, outFile string, emit Emit) error
}
