// Package styles is the library of trained music styles ("train your own style", docs/architecture.md): a style is
// a LoRA of a song model trained on tracks the user owns. Each style is a folder <config dir>/styles/<id>/:
//
//	style.json              name, base model, trigger word, strength, tracks, settings, status, training progress
//	tracks/<hash>.wav       the uploaded tracks, converted to 48 kHz stereo (the only copy: uploads are not kept)
//	work/segments/*.wav     the tracks cut into segments for training (made again when missing)
//	work/…                  the trainer's captions, tensors and resumable checkpoint (engine/workers/train_acestep.py)
//	samples/step-N.wav      short songs rendered during training with the LoRA so far
//	lora/                   the trained LoRA (PEFT: adapter_config.json, adapter_model.safetensors)
//
// style.json is read on every call, so the web server and the music backend share one library.
package styles

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Status of a style.
type Status string

const (
	New      Status = "new"      // no tracks yet
	Ready    Status = "ready"    // tracks, not trained
	Queued   Status = "queued"   // a training job waits (set by the API from the job)
	Training Status = "training" // a training job runs
	Paused   Status = "paused"   // stopped with a checkpoint: training resumes from it
	Trained  Status = "trained"  // the LoRA is there
	Failed   Status = "failed"   // the last training failed (Error)
)

// Limits of a track.
const (
	MaxTrackSeconds = 15 * 60
	MinTrackSeconds = 5
	MaxTracks       = 50
	SampleRate      = 48000
)

// ErrNotFound: no style (or track) of that id.
var ErrNotFound = errors.New("no such style")

// Track is one uploaded track.
type Track struct {
	ID       string    `json:"id"` // the first 16 hex digits of the converted audio's SHA-256
	Name     string    `json:"name"`
	File     string    `json:"file"` // tracks/<id>.wav
	Seconds  float64   `json:"seconds"`
	Segments int       `json:"segments"`
	Vocals   bool      `json:"vocals,omitempty"` // it has vocals: the trainer transcribes them (else instrumental)
	BPM      float64   `json:"bpm,omitempty"`    // its tempo, measured for trainers that put it into the captions
	Added    time.Time `json:"added"`
}

// Settings of the training.
type Settings struct {
	Steps           int     `json:"steps"`
	Rank            int     `json:"rank"`
	Alpha           int     `json:"alpha"`
	LR              float64 `json:"lr"`
	Warmup          int     `json:"warmup"`
	SegmentSeconds  float64 `json:"segmentSeconds"`
	CheckpointEvery int     `json:"checkpointEvery"`
	SampleEvery     int     `json:"sampleEvery"` // 0: no samples
	SampleSeconds   float64 `json:"sampleSeconds"`
	SamplePrompt    string  `json:"samplePrompt,omitempty"` // "" the first segment's caption
	Seed            int64   `json:"seed"`
}

// Defaults are the settings a new style starts with: PEFT LoRA rank 64 / alpha 128 on q, k, v, o (ACE-Step's
// "recommended" preset), learning rate 1e-4, 30 s segments.
func Defaults() Settings {
	return Settings{Steps: 1000, Rank: 64, Alpha: 128, LR: 1e-4, Warmup: 50, SegmentSeconds: 30, CheckpointEvery: 50,
		SampleEvery: 250, SampleSeconds: 12, Seed: 42}
}

// Filled is the settings with the defaults in unset fields.
func (s Settings) Filled() Settings {
	d := Defaults()
	if s.Steps <= 0 {
		s.Steps = d.Steps
	}
	if s.Rank <= 0 {
		s.Rank = d.Rank
	}
	if s.Alpha <= 0 {
		s.Alpha = 2 * s.Rank
	}
	if s.LR <= 0 {
		s.LR = d.LR
	}
	if s.Warmup < 0 {
		s.Warmup = 0
	}
	if s.SegmentSeconds <= 0 {
		s.SegmentSeconds = d.SegmentSeconds
	}
	if s.CheckpointEvery <= 0 {
		s.CheckpointEvery = d.CheckpointEvery
	}
	if s.SampleEvery < 0 {
		s.SampleEvery = 0
	}
	if s.SampleSeconds <= 0 {
		s.SampleSeconds = d.SampleSeconds
	}
	if s.Seed == 0 {
		s.Seed = d.Seed
	}
	return s
}

// Check says what is wrong with settings (after Filled).
func (s Settings) Check() error {
	switch {
	case s.Steps > 20000:
		return errors.New("steps: 20000 at most")
	case s.Rank > 256:
		return errors.New("rank: 256 at most")
	case s.LR > 0.01:
		return errors.New("learning rate: 0.01 at most")
	case s.SegmentSeconds < 10 || s.SegmentSeconds > 120:
		return errors.New("segments: 10 to 120 seconds")
	case s.SampleSeconds > 60:
		return errors.New("samples: 60 seconds at most")
	}
	return nil
}

// Point is the loss at a step.
type Point struct {
	Step int     `json:"step"`
	Loss float64 `json:"loss"`
}

// Sample is a song rendered during training.
type Sample struct {
	Step int    `json:"step"`
	File string `json:"file"`
}

// Progress of the training (kept after it, for the chart and the samples).
type Progress struct {
	Phase          string    `json:"phase,omitempty"` // what it does now ("" when not training)
	Step           int       `json:"step"`
	Total          int       `json:"total"`
	Loss           float64   `json:"loss,omitempty"`
	Losses         []Point   `json:"losses,omitempty"` // at most MaxPoints (older ones averaged in pairs)
	Samples        []Sample  `json:"samples,omitempty"`
	Checkpoint     int       `json:"checkpoint,omitempty"`     // the step of the last checkpoint (0: none)
	SecondsPerStep float64   `json:"secondsPerStep,omitempty"` // measured
	ETA            float64   `json:"eta,omitempty"`            // seconds left (while training)
	Started        time.Time `json:"started,omitzero"`
	Updated        time.Time `json:"updated,omitzero"`
}

// MaxPoints is how many loss points a style keeps.
const MaxPoints = 2000

// AddLoss appends a point; past MaxPoints the older half is averaged in pairs.
func (p *Progress) AddLoss(step int, loss float64) {
	for len(p.Losses) > 0 && p.Losses[len(p.Losses)-1].Step >= step {
		p.Losses = p.Losses[:len(p.Losses)-1] // a resumed run repeats steps after the checkpoint
	}
	p.Losses = append(p.Losses, Point{step, loss})
	if len(p.Losses) > MaxPoints {
		half := len(p.Losses) / 2
		var merged []Point
		for i := 0; i+1 < half; i += 2 {
			merged = append(merged, Point{p.Losses[i+1].Step, (p.Losses[i].Loss + p.Losses[i+1].Loss) / 2})
		}
		p.Losses = append(merged, p.Losses[half:]...)
	}
}

// Style is one style.
type Style struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	BaseModel string `json:"baseModel"` // the song model it is trained for (and works with)
	Trigger   string `json:"trigger"`   // a made-up word the captions start with; a song asks for the style with it
	// Description is what the tracks sound like in tags ("uplifting trance, supersaw lead, rolling bass"): the
	// caption of every track for trainers without a captioner of their own (Stable Audio 3)
	Description string    `json:"description,omitempty"`
	Strength    float64   `json:"strength"` // the LoRA's default scale (0..2)
	Tracks      []Track   `json:"tracks"`
	Settings    Settings  `json:"settings"`
	Created     time.Time `json:"created"`
	Status      Status    `json:"status"`
	Error       string    `json:"error,omitempty"`
	Job         string    `json:"job,omitempty"` // the last training job
	Progress    Progress  `json:"progress"`
	Trained     time.Time `json:"trained,omitzero"`
	Weights     string    `json:"weights,omitempty"` // lora/ when trained
	// Captions the trainer's auto-labelling wrote, by segment file name (what the model heard in each segment)
	Captions map[string]string `json:"captions,omitempty"`

	// A community style (not trained here: a LoRA from a Hugging Face repository, pinned): where it comes from. Its
	// weights live in the model store (fetched and checked at the first song), not in the style's folder; it cannot
	// be trained (only renamed, its strength changed, removed).
	Source *Source `json:"source,omitempty"`
	// WeightsFile is the LoRA's file in Weights ("": adapter_model.safetensors for ACE-Step, style.safetensors for
	// Stable Audio 3)
	WeightsFile string `json:"weightsFile,omitempty"`
	// Recommended are the settings the LoRA was made for: a song that leaves them on Auto gets them
	Recommended *Recommended `json:"recommended,omitempty"`
	// Examples are prompts the LoRA's author suggests (without the trigger word: it is added anyway)
	Examples []Example `json:"examples,omitempty"`
}

// Source is where a community style comes from: a Hugging Face repository at a commit.
type Source struct {
	Repo    string `json:"repo"`
	Rev     string `json:"rev"`
	URL     string `json:"url"`               // its page
	Author  string `json:"author,omitempty"`  // as the LoRA's metadata names them
	Licence string `json:"licence,omitempty"` // "": the repository states none
	// BaseText is the base model as the repository names it ("AceStep v1.5 Turbo (2B)")
	BaseText string `json:"baseText,omitempty"`
	Size     int64  `json:"size,omitempty"` // bytes of the weights
	Rank     int    `json:"rank,omitempty"`
	Alpha    int    `json:"alpha,omitempty"`
}

// Spec is repo@rev.
func (s Source) Spec() string { return s.Repo + "@" + s.Rev }

// Recommended settings of a style (0: the model's own).
type Recommended struct {
	Steps    int     `json:"steps,omitempty"`
	Guidance float64 `json:"guidance,omitempty"`
	Shift    float64 `json:"shift,omitempty"`
}

// Example is a suggested prompt with its tempo and key.
type Example struct {
	Prompt string  `json:"prompt"`
	BPM    float64 `json:"bpm,omitempty"`
	Key    string  `json:"key,omitempty"`
}

// Community: the style is a LoRA from Hugging Face, not trained here.
func (s Style) Community() bool { return s.Source != nil }

// ErrCommunity: the style comes from Hugging Face and cannot be trained.
var ErrCommunity = errors.New("a community style comes from Hugging Face as it is: it cannot be trained or given tracks (make a new style to train your own)")

// Seconds is the length of all tracks.
func (s Style) Seconds() float64 {
	var t float64
	for _, tr := range s.Tracks {
		t += tr.Seconds
	}
	return t
}

// SegmentCount is how many segments the tracks give.
func (s Style) SegmentCount() int {
	n := 0
	for _, tr := range s.Tracks {
		n += tr.Segments
	}
	return n
}

// Segment is a part of a track the trainer learns from.
type Segment struct {
	Track   string  `json:"track"`
	File    string  `json:"file"` // work/segments/<track>-<n>.wav
	Start   float64 `json:"start"`
	Seconds float64 `json:"seconds"`
	Vocals  bool    `json:"vocals,omitempty"`
}

// Split plans the segments of a track of the given length: full segments from the start, and one more ending at
// the end when at least a third of a segment is left over (it overlaps the one before). A track shorter than a
// segment is one segment of its own length (none under MinTrackSeconds). At most 30 segments.
func Split(seconds, segment float64) [][2]float64 {
	if seconds < MinTrackSeconds || segment <= 0 {
		return nil
	}
	if seconds <= segment {
		return [][2]float64{{0, seconds}}
	}
	var out [][2]float64
	n := int(seconds / segment)
	for i := 0; i < n; i++ {
		out = append(out, [2]float64{float64(i) * segment, segment})
	}
	if rest := seconds - float64(n)*segment; rest >= segment/3 {
		out = append(out, [2]float64{seconds - segment, segment})
	}
	if len(out) > 30 { // a very long track: 30 spread over it
		step := (seconds - segment) / 29
		out = out[:0]
		for i := 0; i < 30; i++ {
			out = append(out, [2]float64{math.Round(float64(i)*step*100) / 100, segment})
		}
	}
	return out
}

// mu guards every style.json of this process.
var mu sync.Mutex

// Library is the styles of one folder.
type Library struct {
	Dir string
}

// Open is the library in dir.
func Open(dir string) *Library { return &Library{Dir: dir} }

// Path is a style's folder.
func (l *Library) Path(id string) string { return filepath.Join(l.Dir, id) }

func validID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`) && !strings.HasPrefix(id, ".")
}

func (l *Library) load(id string) (Style, error) {
	if !validID(id) {
		return Style{}, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(l.Path(id), "style.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Style{}, ErrNotFound
	}
	if err != nil {
		return Style{}, err
	}
	var s Style
	if err := json.Unmarshal(b, &s); err != nil {
		return Style{}, fmt.Errorf("%s: %w", id, err)
	}
	if s.Tracks == nil {
		s.Tracks = []Track{}
	}
	return s, nil
}

func (l *Library) save(s Style) error {
	dir := l.Path(s.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "style.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "style.json"))
}

// List is every style, oldest first.
func (l *Library) List() ([]Style, error) {
	mu.Lock()
	defer mu.Unlock()
	ents, err := os.ReadDir(l.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Style{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Style{}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		s, err := l.load(e.Name())
		if err != nil {
			continue // a folder without style.json (half made, or not ours)
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Style) int { return a.Created.Compare(b.Created) })
	return out, nil
}

// Get is one style.
func (l *Library) Get(id string) (Style, error) {
	mu.Lock()
	defer mu.Unlock()
	return l.load(id)
}

// Mutate changes a style under the lock and saves it.
func (l *Library) Mutate(id string, f func(*Style) error) (Style, error) {
	mu.Lock()
	defer mu.Unlock()
	s, err := l.load(id)
	if err != nil {
		return Style{}, err
	}
	if err := f(&s); err != nil {
		return Style{}, err
	}
	return s, l.save(s)
}

func cleanName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", errors.New("a name for the style")
	}
	if len([]rune(name)) > 60 {
		return "", errors.New("the name is too long (60 characters at most)")
	}
	return name, nil
}

// slug is the name in lower-case letters and digits, words joined by "-".
func slug(name string, max int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == 'ä':
			b.WriteString("ae")
		case r == 'ö':
			b.WriteString("oe")
		case r == 'ü':
			b.WriteString("ue")
		case r == 'ß':
			b.WriteString("ss")
		case unicode.IsSpace(r) || r == '-' || r == '_':
			if s := b.String(); s != "" && !strings.HasSuffix(s, "-") {
				b.WriteByte('-')
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > max {
		s = strings.Trim(s[:max], "-")
	}
	return s
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// trigger is a made-up word for the style (the captions start with it; it should mean nothing to the model yet):
// the name's letters and a random suffix, "deep-trance-x7k2".
func trigger(name string) string {
	s := slug(name, 16)
	if s == "" {
		s = "style"
	}
	return s + "-" + randomHex(2)
}

// Create makes a style for a base model with its training settings (the caller checks that the model can be
// trained and picks the settings for it; zero values take Defaults).
func (l *Library) Create(name, base string, set Settings) (Style, error) {
	name, err := cleanName(name)
	if err != nil {
		return Style{}, err
	}
	if strings.TrimSpace(base) == "" {
		return Style{}, errors.New("a base model")
	}
	id := slug(name, 24)
	if id == "" {
		id = "style"
	}
	s := Style{ID: id + "-" + randomHex(3), Name: name, BaseModel: base, Trigger: trigger(name), Strength: 1,
		Tracks: []Track{}, Settings: set.Filled(), Created: time.Now().UTC(), Status: New}
	mu.Lock()
	defer mu.Unlock()
	return s, l.save(s)
}

// AddCommunity adds a community style (Source set, Weights the LoRA's folder): a new id from its name, Trained.
// The same repository at the same commit for the same model twice is one style (the one there is returned).
func (l *Library) AddCommunity(s Style) (Style, error) {
	if s.Source == nil || s.Source.Repo == "" || s.Source.Rev == "" {
		return Style{}, errors.New("a community style needs its repository and commit")
	}
	name, err := cleanName(s.Name)
	if err != nil {
		return Style{}, err
	}
	s.Trigger = strings.TrimSpace(s.Trigger)
	if s.Trigger == "" {
		return Style{}, errors.New("the LoRA's trigger word (its model card names it)")
	}
	if len(s.Trigger) > 60 || strings.ContainsAny(s.Trigger, ",\n") {
		return Style{}, errors.New("the trigger word: one word or phrase, no commas, 60 characters at most")
	}
	if strings.TrimSpace(s.BaseModel) == "" {
		return Style{}, errors.New("a base model")
	}
	if s.Strength <= 0 || s.Strength > 2 || math.IsNaN(s.Strength) {
		s.Strength = 1
	}
	all, err := l.List()
	if err != nil {
		return Style{}, err
	}
	for _, o := range all {
		if o.Source != nil && o.Source.Repo == s.Source.Repo && o.Source.Rev == s.Source.Rev && o.BaseModel == s.BaseModel {
			return o, nil
		}
	}
	id := slug(name, 24)
	if id == "" {
		id = "style"
	}
	s.ID, s.Name = id+"-"+randomHex(3), name
	s.Tracks, s.Settings, s.Created, s.Status = []Track{}, Settings{}, time.Now().UTC(), Trained
	s.Trained, s.Progress, s.Captions, s.Error, s.Job = s.Created, Progress{}, nil, "", ""
	mu.Lock()
	defer mu.Unlock()
	return s, l.save(s)
}

// Patch is what PATCH /api/styles/{id} may change.
type Patch struct {
	Name        *string   `json:"name,omitempty"`
	Description *string   `json:"description,omitempty"`
	Strength    *float64  `json:"strength,omitempty"`
	Settings    *Settings `json:"settings,omitempty"`
}

// Update changes a style's name, strength or training settings.
func (l *Library) Update(id string, p Patch) (Style, error) {
	return l.Mutate(id, func(s *Style) error {
		if p.Settings != nil && s.Community() {
			return ErrCommunity
		}
		if p.Name != nil {
			n, err := cleanName(*p.Name)
			if err != nil {
				return err
			}
			s.Name = n
		}
		if p.Description != nil {
			d := strings.Join(strings.Fields(*p.Description), " ")
			if len([]rune(d)) > 500 {
				return errors.New("the description is too long (500 characters at most)")
			}
			s.Description = d
		}
		if p.Strength != nil {
			if *p.Strength < 0 || *p.Strength > 2 || math.IsNaN(*p.Strength) {
				return errors.New("strength: 0 to 2")
			}
			s.Strength = *p.Strength
		}
		if p.Settings != nil {
			st := p.Settings.Filled()
			if err := st.Check(); err != nil {
				return err
			}
			if st.SegmentSeconds != s.Settings.SegmentSeconds {
				for i := range s.Tracks {
					s.Tracks[i].Segments = len(Split(s.Tracks[i].Seconds, st.SegmentSeconds))
				}
			}
			s.Settings = st
		}
		return nil
	})
}

// Delete removes a style with its tracks and weights (a community style's weights stay in the model store).
func (l *Library) Delete(id string) error {
	mu.Lock()
	defer mu.Unlock()
	if _, err := l.load(id); err != nil {
		return err
	}
	return os.RemoveAll(l.Path(id))
}

// Converter turns any audio file into the 48 kHz stereo WAV the trainer reads (at most MaxTrackSeconds); a test
// may replace it.
var Converter = func(ctx context.Context, in, out string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-v", "error", "-i", in, "-vn", "-ac", "2", "-ar", strconv.Itoa(SampleRate),
		"-t", strconv.Itoa(MaxTrackSeconds), "-c:a", "pcm_s16le", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("converting the track (ffmpeg): %v %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}

// Duration is a sound file's length in seconds (ffprobe); a test may replace it.
var Duration = func(path string) float64 {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return f
}

// AddTrack adds an uploaded track (any format ffmpeg reads; converted, cut to MaxTrackSeconds). The same audio
// twice is one track.
func (l *Library) AddTrack(ctx context.Context, id, name string, r io.Reader, vocals bool) (Style, Track, error) {
	s, err := l.Get(id)
	if err != nil {
		return Style{}, Track{}, err
	}
	if s.Community() {
		return Style{}, Track{}, ErrCommunity
	}
	if len(s.Tracks) >= MaxTracks {
		return Style{}, Track{}, fmt.Errorf("a style takes %d tracks at most", MaxTracks)
	}
	dir := filepath.Join(l.Path(id), "tracks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Style{}, Track{}, err
	}
	raw, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return Style{}, Track{}, err
	}
	defer os.Remove(raw.Name())
	if _, err := io.Copy(raw, io.LimitReader(r, 512<<20)); err != nil {
		raw.Close()
		return Style{}, Track{}, err
	}
	raw.Close()
	conv := filepath.Join(dir, ".convert-"+randomHex(4)+".wav")
	defer os.Remove(conv)
	if err := Converter(ctx, raw.Name(), conv); err != nil {
		return Style{}, Track{}, err
	}
	sec := Duration(conv)
	if sec < MinTrackSeconds {
		return Style{}, Track{}, fmt.Errorf("the track is too short (%.1f s; at least %d s)", sec, MinTrackSeconds)
	}
	sum, err := hashFile(conv)
	if err != nil {
		return Style{}, Track{}, err
	}
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		name = "Track"
	}
	name = strings.TrimSuffix(name, filepath.Ext(name))
	t := Track{ID: sum[:16], Name: name, File: filepath.Join(dir, sum[:16]+".wav"), Seconds: math.Round(sec*100) / 100,
		Vocals: vocals, Added: time.Now().UTC()}
	s, err = l.Mutate(id, func(s *Style) error {
		for _, o := range s.Tracks {
			if o.ID == t.ID {
				t = o
				return nil // already there
			}
		}
		if err := os.Rename(conv, t.File); err != nil {
			return err
		}
		t.Segments = len(Split(t.Seconds, s.Settings.SegmentSeconds))
		s.Tracks = append(s.Tracks, t)
		if s.Status == New {
			s.Status = Ready
		}
		return nil
	})
	return s, t, err
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DeleteTrack removes a track (and its segments).
func (l *Library) DeleteTrack(id, track string) (Style, error) {
	return l.Mutate(id, func(s *Style) error {
		i := slices.IndexFunc(s.Tracks, func(t Track) bool { return t.ID == track })
		if i < 0 {
			return fmt.Errorf("%w: no track %s", ErrNotFound, track)
		}
		_ = os.Remove(s.Tracks[i].File)
		segs, _ := filepath.Glob(filepath.Join(l.Path(id), "work", "segments", track+"-*.wav"))
		for _, f := range segs {
			_ = os.Remove(f)
		}
		s.Tracks = slices.Delete(s.Tracks, i, i+1)
		if len(s.Tracks) == 0 && s.Status == Ready {
			s.Status = New
		}
		return nil
	})
}

// Segments are the segments of a style's tracks (files in work/segments, made by Prepare).
func (l *Library) Segments(s Style) []Segment {
	var out []Segment
	dir := filepath.Join(l.Path(s.ID), "work", "segments")
	for _, t := range s.Tracks {
		for i, p := range Split(t.Seconds, s.Settings.SegmentSeconds) {
			out = append(out, Segment{Track: t.ID, File: filepath.Join(dir, fmt.Sprintf("%s-%02d-%gs.wav", t.ID, i, p[1])),
				Start: p[0], Seconds: p[1], Vocals: t.Vocals})
		}
	}
	return out
}

// Cutter writes one segment of a track; a test may replace it.
var Cutter = func(ctx context.Context, in string, start, seconds float64, out string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-v", "error", "-ss", strconv.FormatFloat(start, 'f', 3, 64), "-i", in,
		"-t", strconv.FormatFloat(seconds, 'f', 3, 64), "-ac", "2", "-ar", strconv.Itoa(SampleRate), "-c:a", "pcm_s16le", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cutting a segment (ffmpeg): %v %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}

// Prepare cuts the tracks into their segments (those already cut stay; segments no longer planned go).
func (l *Library) Prepare(ctx context.Context, id string) ([]Segment, error) {
	s, err := l.Get(id)
	if err != nil {
		return nil, err
	}
	segs := l.Segments(s)
	if len(segs) == 0 {
		return nil, errors.New("the style has no tracks to learn from (add a few, at least 5 s each)")
	}
	dir := filepath.Join(l.Path(id), "work", "segments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, sg := range segs {
		keep[sg.File] = true
	}
	old, _ := filepath.Glob(filepath.Join(dir, "*.wav"))
	for _, f := range old {
		if !keep[f] {
			_ = os.Remove(f)
		}
	}
	tracks := map[string]Track{}
	for _, t := range s.Tracks {
		tracks[t.ID] = t
	}
	for _, sg := range segs {
		if _, err := os.Stat(sg.File); err == nil {
			continue
		}
		tmp := sg.File + ".tmp.wav"
		if err := Cutter(ctx, tracks[sg.Track].File, sg.Start, sg.Seconds, tmp); err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
		if err := os.Rename(tmp, sg.File); err != nil {
			return nil, err
		}
	}
	return segs, nil
}

// Reset forgets a style's training (checkpoint, LoRA, samples, progress): the next training starts over. The
// captions and the encoded segments stay (they do not depend on the training).
func (l *Library) Reset(id string) (Style, error) {
	return l.Mutate(id, func(s *Style) error {
		if s.Community() {
			return ErrCommunity
		}
		for _, d := range []string{"work/checkpoint", "lora", "samples"} {
			if err := os.RemoveAll(filepath.Join(l.Path(id), filepath.FromSlash(d))); err != nil {
				return err
			}
		}
		s.Progress, s.Weights, s.Trained, s.Error = Progress{}, "", time.Time{}, ""
		s.Status = Ready
		if len(s.Tracks) == 0 {
			s.Status = New
		}
		return nil
	})
}

// Estimate is about how long a training of the style takes (seconds): loading and preparing (perSegment for each
// segment: captions and encoding), the steps, the samples. secondsPerStep: the base model's typical value (a measured
// one of the style wins).
func Estimate(s Style, secondsPerStep, perSegment float64) float64 {
	st := s.Settings.Filled()
	if s.Progress.SecondsPerStep > 0 {
		secondsPerStep = s.Progress.SecondsPerStep
	}
	// a step's time grows with the segment length (measured at 30 s)
	steps := st.Steps - s.Progress.Checkpoint
	if steps < 0 {
		steps = 0
	}
	t := 90.0 + float64(s.SegmentCount())*perSegment
	t += float64(steps) * secondsPerStep * st.SegmentSeconds / 30
	if st.SampleEvery > 0 {
		t += float64(steps/st.SampleEvery) * (15 + st.SampleSeconds)
	}
	return math.Round(t)
}
