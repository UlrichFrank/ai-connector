// Package engine runs the models: every model of the catalogue belongs to one backend (the chat models on MLX in
// Go, the classifier and the embedder in Go, FLUX.2 klein through mflux, LTX-2.5 through ltx-2-mlx). A backend takes
// a Request and reports what happens as Events: status lines, streamed text, steps, the files it wrote.
package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/philipparndt/ai-connector/internal/llm"
)

// Kind is what a model does.
type Kind string

const (
	Chat     Kind = "chat"
	Embed    Kind = "embed"
	Classify Kind = "classify"
	Image    Kind = "image"
	Video    Kind = "video"
	Music    Kind = "music"
	Speech   Kind = "speech" // text to speech, voices (internal/engine/speech.go)
	Avatar   Kind = "avatar" // a talking video from a portrait and speech (internal/engine/avatar.go)
	Sound    Kind = "sound"  // sound effects from a description (internal/engine/sound.go)
)

// Kinds in the order the UIs list them.
var Kinds = []Kind{Chat, Image, Video, Music, Speech, Avatar, Sound, Embed, Classify}

// Request is one job for a model. Only the fields of the model's kind are read.
type Request struct {
	Model string `json:"model"`

	// chat
	Messages []llm.Message `json:"messages,omitempty"`
	// The sampling settings override the model's defaults for thinking on or off (llm.ModelDefaults: Qwen's
	// recommendations and the model's generation_config.json); unset (0, nil) takes the default. A 0 in JSON is
	// "not set", so greedy decoding is a flag of its own: Greedy (temperature 0, always the most likely token).
	Temperature     float32  `json:"temperature,omitempty"`
	TopP            float32  `json:"topP,omitempty"` // 1: off
	TopK            int      `json:"topK,omitempty"`
	PresencePenalty *float32 `json:"presencePenalty,omitempty"` // 0: off
	Greedy          bool     `json:"greedy,omitempty"`
	MaxTokens       int      `json:"maxTokens,omitempty"` // 0: 32768 thinking (llm.ThinkMaxTokens), else 4096
	Think           bool     `json:"think,omitempty"`
	// Zone: what the chat's tools may touch ("" no tools, "home", "web", "files"; see internal/agent). A follow-up
	// (Parent) keeps its parent's zone: the job manager derives it and refuses another.
	Zone string `json:"zone,omitempty"`
	// Tools: functions of the caller (an application, through the server's OpenAI API) the model may call. One
	// round: the job ends with the model's calls in result.toolCalls ([{"id","name","arguments"}]); they are never
	// run here. Not with a Zone (the zones' tools run on the worker).
	Tools []llm.Tool `json:"tools,omitempty"`
	// Client is the application that submitted the job (the server sets it from the client token's name).
	Client string `json:"client,omitempty"`

	// embed: the texts are compared with each other
	Texts []string `json:"texts,omitempty"`

	// classify: one choice question about every state
	States       []string `json:"states,omitempty"`
	Instructions string   `json:"instructions,omitempty"`
	Options      []Option `json:"options,omitempty"`

	// image, video
	Prompt   string   `json:"prompt,omitempty"`
	Width    int      `json:"width,omitempty"`
	Height   int      `json:"height,omitempty"`
	Steps    int      `json:"steps,omitempty"`
	Seed     int64    `json:"seed,omitempty"` // 0: random
	Guidance float64  `json:"guidance,omitempty"`
	Quantize int      `json:"quantize,omitempty"` // image: 0 = the model's default, -1 = none (bf16)
	Images   []string `json:"images,omitempty"`   // image: references to edit; video: the first frame
	Frames   int      `json:"frames,omitempty"`   // video: 8k+1 (0: LTX-2.5 predicts the length)
	FPS      int      `json:"fps,omitempty"`
	LowRAM   bool     `json:"lowRam,omitempty"`
	Script   []Clip   `json:"script,omitempty"` // video: a movie, clip by clip (Prompt: what every clip shares)
	NoAudio  bool     `json:"noAudio,omitempty"`

	// music: Prompt is the style; Steps, Seed as above
	Lyrics       string  `json:"lyrics,omitempty"`
	Duration     float64 `json:"duration,omitempty"` // seconds
	Instrumental bool    `json:"instrumental,omitempty"`
	BPM          int     `json:"bpm,omitempty"`       // the tempo (0: from "NNN bpm" in the style, else the model's choice)
	Planner      string  `json:"planner,omitempty"`   // ACE-Step's planner LM: "" 1.7B, "4b" (fetched when first used), "off"
	Shift        float64 `json:"shift,omitempty"`     // ACE-Step's timestep shift (0: 3.0 turbo, 1.0 base; a style's recommendation first)
	LockTempo    *bool   `json:"lockTempo,omitempty"` // stretch a new song to its tempo (nil: on when there is one; docs/music.md)
	Takes        int     `json:"takes,omitempty"`     // best of N (1–3): the take closest to the tempo is kept
	Title        string  `json:"title,omitempty"`
	Folder       string  `json:"folder,omitempty"`   // where the song is kept in the library ("" the top; "a/b" nested)
	CoverFor     string  `json:"coverFor,omitempty"` // image: becomes the cover of this song (job id) when done

	// the music studio: what to do with an existing song (Task "" or "generate": a new song)
	Task     string  `json:"task,omitempty"`     // generate | cover | repaint | extend | lego | extract
	Source   string  `json:"source,omitempty"`   // the song worked on (a file)
	Parent   string  `json:"parent,omitempty"`   // the job that made it (versions of a song form a tree)
	Start    float64 `json:"start,omitempty"`    // repaint, lego: seconds
	End      float64 `json:"end,omitempty"`      // repaint, lego: seconds (-1: to the end)
	Strength float64 `json:"strength,omitempty"` // cover: how much of the source stays (0..1)
	Track    string  `json:"track,omitempty"`    // lego, extract: vocals, drums, bass …
	Extend   float64 `json:"extend,omitempty"`   // extend: seconds to add
	ABC      string  `json:"abc,omitempty"`      // restyle (YuE2): the melody score (empty: the source song's)
	// a trained style (internal/styles): its LoRA on the model it was trained for, its trigger word added to the
	// style text; StyleStrength scales it (0: the style's own strength). Task "train-style" trains Style.
	Style         string  `json:"style,omitempty"`
	StyleStrength float64 `json:"styleStrength,omitempty"`

	// speech (and avatar when it speaks a text): Prompt is not used — Text is what is said.
	Text     string  `json:"text,omitempty"`
	Voice    string  `json:"voice,omitempty"`    // a voice of the library (voices.json id) or a model's preset voice name
	Instruct string  `json:"instruct,omitempty"` // how to say it: "warm, slow, a little amused" / an emotion
	Language string  `json:"language,omitempty"` // "de", "en", … ("" = detect)
	Speed    float64 `json:"speed,omitempty"`    // 1 = normal
	// avatar: the portrait is Images[0]; the speech is Audio (a /files/ url) or else Text spoken with Voice
	// (by a speech model: SpeechModel, default the first downloaded one). Prompt describes the scene/motion.
	Audio       string `json:"audio,omitempty"`
	SpeechModel string `json:"speechModel,omitempty"`
	// sound: Prompt describes the sound, Duration its length.
}

// Option is one answer of a choice question.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Answer is the classifier's answer about one text.
type Answer struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

// Embedder is a backend that embeds texts directly, beside the job queue (the server's embeddings API: quick calls
// that must not wait behind a video): one vector per text, in their order.
type Embedder interface {
	Embed(ctx context.Context, m *Model, texts []string) ([][]float32, error)
}

// Classifier is a backend that answers a choice question directly, beside the job queue.
type Classifier interface {
	Classify(ctx context.Context, m *Model, states []string, instructions string, options []Option) ([]Answer, error)
}

// Event is something a running job reports.
type Event struct {
	Type  string         `json:"type"` // status | log | text | progress | file | stats | result
	Text  string         `json:"text,omitempty"`
	Step  int            `json:"step,omitempty"`
	Total int            `json:"total,omitempty"`
	File  string         `json:"file,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
}

// Emit passes an event on.
type Emit func(Event)

func (e Emit) Status(s string)      { e(Event{Type: "status", Text: s}) }
func (e Emit) Log(s string)         { e(Event{Type: "log", Text: s}) }
func (e Emit) Progress(step, n int) { e(Event{Type: "progress", Step: step, Total: n}) }

// Backend runs the models of one runtime.
type Backend interface {
	Name() string
	// Run does one job. outDir is where it writes files.
	Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error
	// Loaded is the model in memory ("" if none).
	Loaded() string
	// Heavy: its models take a large share of the memory (only one heavy backend keeps a model loaded).
	Heavy() bool
	// Unload frees the memory.
	Unload()
}

// status is what a backend reports as loaded. It has a lock of its own: the backend's lock is held for a whole job
// (minutes: a download, a song), and asking what is loaded must never wait for that.
type status struct {
	mu sync.Mutex
	v  string
}

func (s *status) set(v string) {
	s.mu.Lock()
	s.v = v
	s.mu.Unlock()
}

func (s *status) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v
}

// ConfigDir is ~/.config/mlxlab ($MLXLAB_HOME, $XDG_CONFIG_HOME/mlxlab).
func ConfigDir() string {
	if d := os.Getenv("MLXLAB_HOME"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "mlxlab")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "mlxlab")
	}
	return filepath.Join(os.TempDir(), "mlxlab")
}
