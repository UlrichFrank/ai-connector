package hub

import "slices"

// Generators are the models that make images, videos, music, sound and speech, and those that upscale an image or
// cut it out. What each takes — its kind, the sizes (rounded down to a multiple, the longer side at most max), the
// lengths, the factors, the voices — is described here from the workers' catalogue for the applications
// (GET /api/capabilities, docs/api.md). The engine's limits are repeated here: the hub does not import the
// worker's packages.

const (
	genImageMultiple  = 16 // FLUX and Qwen-Image (mflux): sides are rounded down to it (engine/flux.go)
	genImageMax       = 2048
	genVideoMultiple  = 64 // LTX's two-stage pipeline (engine/ltx.go)
	genVideoMax       = 1280
	genVideoMaxSecond = 10
	genUpscaleMax     = 4096 // SeedVR2 (engine.UpscaleMax): the longest side of the result
	genCutoutMax      = 4096 // BiRefNet: the longest side of an image it cuts out (the result has its size)
)

// genUpscaleScales are the factors an upscaler takes (a request's scale goes down to the nearest).
var genUpscaleScales = []int{2, 4}

// generatorInfo is a generator model and what it takes (ID, Name, Worker, Model: where an application would get it).
type generatorInfo struct {
	ID, Name, Kind, Worker string
	Model                  string // the catalogue id
	// image: Edit takes reference images, TextToImage makes one without (false: an editing model that needs one);
	// video: FirstFrame may start from an image, Audio has a soundtrack
	Edit, TextToImage, FirstFrame, Audio bool
	Multiple, Max                        int // sides are rounded down to Multiple; the longer side is at most Max
	Width, Height, FPS, Seconds          int // the defaults
	MaxSeconds                           int
	Scales                               []int // upscale: the factors it takes
	NonCommercial                        bool
	// music: it sings lyrics, it leaves the voice out, it takes a tempo (Seconds, MaxSeconds: its lengths);
	// speech: its voices, languages (none: it detects them), instruct, the longest text
	Lyrics, Instrumental, BPM, Instruct bool
	Voices                              []genVoice
	Languages                           []string
	MaxChars                            int
}

// generatorOf is what a catalogue model can make (false: it is no generator the hub knows how to describe): FLUX
// and Qwen-Image (mflux) for images, SeedVR2 (mflux, task "upscale") for upscales, BiRefNet (task "cutout") for
// cutouts, LTX for videos, and the music, sound and speech models that describe themselves (audioGenerator).
func generatorOf(m catalogModel) (generatorInfo, bool) {
	d := modelDefaults{}
	if m.Defaults != nil {
		d = *m.Defaults
	}
	g := generatorInfo{Kind: m.Kind, NonCommercial: m.NonCommercial}
	switch {
	case m.Kind == "image" && m.Backend == "mflux" && slices.Contains(m.Tasks, "upscale"):
		g.Kind, g.Scales, g.Max = "upscale", genUpscaleScales, genUpscaleMax
	case m.Kind == "image" && slices.Contains(m.Tasks, "cutout"):
		g.Kind, g.Max = "cutout", genCutoutMax
	case m.Kind == "image" && m.Backend == "mflux":
		// a model with tasks does only those (Qwen-Image: text to image, Qwen-Image-Edit: editing); FLUX does both
		g.Edit = len(m.Tasks) == 0 || slices.Contains(m.Tasks, "edit")
		g.TextToImage = len(m.Tasks) == 0 || slices.Contains(m.Tasks, "generate")
		g.Multiple, g.Max = genImageMultiple, genImageMax
		g.Width, g.Height = cmpOr(d.Width, 1024), cmpOr(d.Height, 1024)
	case m.Kind == "video" && m.Backend == "ltx":
		g.FirstFrame, g.Audio = true, true
		g.Multiple, g.Max, g.MaxSeconds = genVideoMultiple, genVideoMax, genVideoMaxSecond
		g.Width, g.Height, g.FPS = cmpOr(d.Width, 704), cmpOr(d.Height, 448), cmpOr(d.FPS, 24)
		g.Seconds = 4
		if d.Frames > 1 {
			g.Seconds = max(1, (d.Frames-1)/g.FPS) // 97 frames at 24 fps: 4 s
		}
	case audioKind(m.Kind):
		return audioGenerator(m, g)
	default:
		return g, false
	}
	return g, true
}

func cmpOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// generatorsJSON is the list as the applications read it (GET /api/capabilities; vboard's hello.generators too).
func generatorsJSON(gs []generatorInfo) []map[string]any {
	out := make([]map[string]any, len(gs))
	for i, g := range gs {
		if audioKind(g.Kind) {
			out[i] = audioGeneratorJSON(g)
			continue
		}
		if g.Kind == "upscale" {
			out[i] = map[string]any{"id": g.ID, "name": g.Name, "kind": g.Kind, "worker": g.Worker, "scales": g.Scales,
				"max": g.Max, "nonCommercial": g.NonCommercial}
			continue
		}
		if g.Kind == "cutout" { // one image in, the same size out: no sizes, no factors
			out[i] = map[string]any{"id": g.ID, "name": g.Name, "kind": g.Kind, "worker": g.Worker, "max": g.Max,
				"nonCommercial": g.NonCommercial}
			continue
		}
		w := map[string]any{"id": g.ID, "name": g.Name, "kind": g.Kind, "worker": g.Worker, "multiple": g.Multiple,
			"max": g.Max, "nonCommercial": g.NonCommercial}
		def := map[string]any{"width": g.Width, "height": g.Height}
		if g.Kind == "video" {
			w["firstFrame"], w["audio"], w["maxSeconds"] = g.FirstFrame, g.Audio, g.MaxSeconds
			def["fps"], def["seconds"] = g.FPS, g.Seconds
		} else {
			w["edit"] = g.Edit
			if !g.TextToImage {
				w["textToImage"] = false // it only edits: at least one reference image
			}
		}
		w["default"] = def
		out[i] = w
	}
	return out
}

func audioKind(kind string) bool { return kind == "music" || kind == "sound" || kind == "speech" }

// audioGenerator is a music, sound or speech model as a generator: what the worker's catalogue says it takes (a
// worker that does not say it — an older one — offers none).
func audioGenerator(m catalogModel, g generatorInfo) (generatorInfo, bool) {
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

func audioGeneratorJSON(g generatorInfo) map[string]any {
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
