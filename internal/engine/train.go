package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/pyenv"
	"github.com/philipparndt/ai-connector/internal/styles"
)

// Train your own style (docs/architecture.md): a LoRA of a song model's DiT, trained on the user's own tracks by
// workers/train_acestep.py, kept in the styles library (internal/styles) and merged into the model when a song asks
// for the style (workers/acestep_worker.py).

// TrainStyle is the music task that trains Request.Style (a job like a song: heavy, one at a time).
const TrainStyle = "train-style"

// trainable are the song models whose styles can be trained, with the seconds a training step of a 30 s segment
// takes (measured on an M1 Max with the GPU to itself: ACE-Step 2B turbo ~1.9–2.4 s, the 4B XL about twice that
// (estimated); Stable Audio 3 Medium ~1.1 s with rank 16 and gradient checkpointing), the preparation per segment
// (ACE-Step: the 5 Hz LM listens to each (~15 s) and encodes it; Stable Audio 3: encoding only), and the defaults.
var trainable = map[string]struct {
	stepSeconds, perSegment float64
	sa3                     bool
}{
	"ace-step-1.5-turbo":    {2.5, 20, false},
	"ace-step-1.5-base":     {2.5, 20, false},
	"ace-step-1.5-xl-turbo": {5, 25, false},
	"ace-step-1.5-xl-base":  {5, 25, false},
	"stable-audio-3-medium": {1.2, 2, true},
}

// CanTrain says whether a model can learn styles.
func CanTrain(model string) bool { _, ok := trainable[model]; return ok }

// StepSeconds is a model's typical time for one training step of a 30 s segment (0: not trainable).
func StepSeconds(model string) float64 { return trainable[model].stepSeconds }

// StyleDescribed: the model's trainer has no captioner, so the style's description is every track's caption
// (Stable Audio 3); ACE-Step's 5 Hz LM writes captions itself.
func StyleDescribed(model string) bool { return trainable[model].sa3 }

// StyleDefaults are a new style's training settings for a model: ACE-Step's "recommended" preset (PEFT LoRA rank
// 64 / alpha 128 on q, k, v, o); for Stable Audio 3 underfit's (DoRA-rows rank 16, alpha = rank).
func StyleDefaults(model string) styles.Settings {
	s := styles.Defaults()
	if trainable[model].sa3 {
		s.Rank, s.Alpha, s.Warmup = 16, 16, 0
	}
	return s
}

// StyleEstimate is about how long training the style takes on its model (seconds).
func StyleEstimate(st styles.Style) float64 {
	t := trainable[st.BaseModel]
	return styles.Estimate(st, t.stepSeconds, t.perSegment)
}

func withStyles(ms []*Model) []*Model {
	for _, m := range ms {
		m.Styles = CanTrain(m.ID)
		m.Audio = musicCaps(m)
	}
	return ms
}

// StyleLibrary is the backend's styles library.
func (b *MusicBackend) StyleLibrary() *styles.Library {
	if b.Styles != nil {
		return b.Styles
	}
	return styles.Open(filepath.Join(b.Root, "styles"))
}

// styleLoRA is the LoRA a song's style puts on the model: a PEFT folder, its weights file, the scale, and the
// settings the style recommends (nil: none).
type styleLoRA struct {
	dir, file   string
	scale       float64
	recommended *styles.Recommended
}

// styleFor is the LoRA a song request's style puts on the model (nil without a style); it adds the style's trigger
// word to the request's style text. A community style's weights are fetched first (model store, pinned, checked).
func (b *MusicBackend) styleFor(ctx context.Context, m *Model, r *Request, emit Emit) (*styleLoRA, error) {
	if r.Style == "" {
		return nil, nil
	}
	if !m.Styles {
		return nil, fmt.Errorf("%s cannot use a trained style (styles work with ACE-Step 1.5)", m.Name)
	}
	st, err := b.StyleLibrary().Get(r.Style)
	if errors.Is(err, styles.ErrNotFound) {
		return nil, fmt.Errorf("no style %q on this Mac", r.Style)
	}
	if err != nil {
		return nil, err
	}
	if st.BaseModel != m.ID {
		verb := "trained"
		if st.Community() {
			verb = "made"
		}
		return nil, fmt.Errorf("the style %q was %s for %s, not for %s: make the song with that model", st.Name, verb, st.BaseModel, m.ID)
	}
	if st.Weights == "" {
		return nil, fmt.Errorf("the style %q is not trained yet", st.Name)
	}
	file := st.WeightsFile
	if file == "" {
		file = "adapter_model.safetensors" // a PEFT folder (ACE-Step); Stable Audio 3: underfit's one file
		if trainable[m.ID].sa3 {
			file = "style.safetensors"
		}
	}
	dir := st.Weights
	if st.Community() {
		if !CommunityModel(m.ID) {
			return nil, fmt.Errorf("%s cannot use a community LoRA", m.Name)
		}
		if dir, err = b.communityWeights(ctx, st, emit); err != nil {
			return nil, fmt.Errorf("the style %q (%s): %w", st.Name, st.Source.Spec(), err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
		return nil, fmt.Errorf("the style %q has no weights (train it again): %w", st.Name, err)
	}
	scale := r.StyleStrength
	if scale <= 0 {
		scale = st.Strength
	}
	if scale > 2 {
		scale = 2
	}
	if st.Trigger != "" && !strings.Contains(strings.ToLower(r.Prompt), strings.ToLower(st.Trigger)) {
		r.Prompt = strings.TrimSpace(st.Trigger + ", " + r.Prompt)
	}
	return &styleLoRA{dir: dir, file: file, scale: scale, recommended: st.Recommended}, nil
}

// trainEvent is what the trainer reports (see workers/train_acestep.py).
type trainEvent struct {
	Type     string            `json:"type"`
	Text     string            `json:"text"`
	Step     int               `json:"step"`
	Total    int               `json:"total"`
	Loss     float64           `json:"loss"`
	LR       float64           `json:"lr"`
	ETA      float64           `json:"eta"`
	SPS      float64           `json:"sps"`
	File     string            `json:"file"`
	Dir      string            `json:"dir"`
	Stopped  bool              `json:"stopped"`
	Seconds  float64           `json:"seconds"`
	Captions map[string]string `json:"captions"`
}

// stopGrace is how long a canceled training may take to write its checkpoint before it is killed.
var stopGrace = 3 * time.Minute

// trainStyle runs a style's training: the tracks cut into segments, then the trainer (captions, encoding, the LoRA
// steps with checkpoints and samples). A cancel stops it at the next step with a checkpoint (the style is
// "paused"); the next training of the style resumes from there.
func (b *MusicBackend) trainStyle(ctx context.Context, m *Model, r Request, emit Emit) error {
	lib := b.StyleLibrary()
	st, err := lib.Get(r.Style)
	if errors.Is(err, styles.ErrNotFound) {
		return fmt.Errorf("no style %q on this Mac", r.Style)
	}
	if err != nil {
		return err
	}
	if !m.Styles {
		return fmt.Errorf("%s cannot learn styles", m.Name)
	}
	if st.Community() {
		return styles.ErrCommunity
	}
	if st.BaseModel != m.ID {
		return fmt.Errorf("the style %q is for %s, not %s", st.Name, st.BaseModel, m.ID)
	}
	set := st.Settings.Filled()
	if err := set.Check(); err != nil {
		return err
	}
	fail := func(err error) error {
		_, _ = lib.Mutate(st.ID, func(s *styles.Style) error {
			s.Progress.Phase, s.Progress.ETA = "", 0
			switch {
			case ctx.Err() != nil && s.Progress.Checkpoint > 0:
				s.Status, s.Error = styles.Paused, ""
			case ctx.Err() != nil:
				s.Status, s.Error = styles.Ready, ""
				if s.Weights != "" {
					s.Status = styles.Trained
				}
			default:
				s.Status, s.Error = styles.Failed, err.Error()
			}
			return nil
		})
		return err
	}
	// what the backend does before the trainer runs (downloads, tempo, segments) is the style's phase too
	jobEmit := emit
	emit = func(ev Event) {
		if ev.Type == "status" {
			_, _ = lib.Mutate(st.ID, func(s *styles.Style) error {
				if s.Progress.Phase != "training" {
					s.Progress.Phase = strings.TrimSuffix(strings.TrimSpace(ev.Text), " …")
				}
				return nil
			})
		}
		jobEmit(ev)
	}
	resume := st.Progress.Checkpoint > 0
	if _, err := lib.Mutate(st.ID, func(s *styles.Style) error {
		s.Status, s.Error, s.Progress.Phase = styles.Training, "", "preparing the tracks"
		s.Progress.Total, s.Progress.Started, s.Progress.ETA = set.Steps, time.Now().UTC(), 0
		if !resume {
			s.Progress.Losses, s.Progress.Samples, s.Progress.Step = nil, nil, 0
		}
		return nil
	}); err != nil {
		return err
	}

	// the GPU and the memory to itself: every song worker stops
	stopAll := func() {
		if b.workers == nil {
			b.workers = map[string]*pyWorker{}
		}
		for name, w := range b.workers {
			if w.running() {
				emit.Status("unloading " + name + " …")
				w.stop()
			}
		}
		b.state.set("")
	}
	var w *pyWorker
	var req map[string]any
	switch m.runtime {
	case "acestep":
		w, req, err = b.aceTrainer(ctx, m, st, set, stopAll, emit)
	case "stableaudio3":
		w, req, err = b.sa3Trainer(ctx, m, st, set, stopAll, emit)
	default:
		err = fmt.Errorf("%s cannot learn styles", m.Name)
	}
	if err != nil {
		return fail(err)
	}
	b.workers["train"] = w
	defer func() {
		w.stop()
		delete(b.workers, "train")
		b.state.set("")
	}()
	emit.Status("starting the trainer …")
	if err := w.start(ctx, b.Root, emit); err != nil {
		return fail(err)
	}
	b.state.set(m.ID)
	emit(Event{Type: "stats", Data: map[string]any{"style": st.ID, "steps": set.Steps}})
	started := time.Now()
	var last time.Time
	res, err := runTrainer(ctx, w, req, emit, func(ev trainEvent) {
		switch ev.Type {
		case "train":
			emit.Progress(ev.Step, ev.Total)
			if time.Since(last) > 5*time.Second || ev.Step == ev.Total {
				last = time.Now()
				emit(Event{Type: "stats", Data: map[string]any{"loss": ev.Loss, "eta": ev.ETA, "step": ev.Step}})
			}
		case "sample":
			emit.Log(fmt.Sprintf("sample at step %d: %s", ev.Step, ev.File))
		}
		_, _ = lib.Mutate(st.ID, func(s *styles.Style) error {
			p := &s.Progress
			switch ev.Type {
			case "train":
				p.Phase, p.Step, p.Total, p.Loss = "training", ev.Step, ev.Total, ev.Loss
				p.SecondsPerStep, p.ETA = ev.SPS, ev.ETA
				p.AddLoss(ev.Step, ev.Loss)
			case "checkpoint":
				p.Checkpoint = ev.Step
			case "sample":
				p.Samples = slices.DeleteFunc(p.Samples, func(x styles.Sample) bool { return x.Step == ev.Step })
				p.Samples = append(p.Samples, styles.Sample{Step: ev.Step, File: ev.File})
			case "captions":
				s.Captions = ev.Captions
			case "status":
				if p.Phase != "training" || !strings.HasPrefix(ev.Text, "training") {
					p.Phase = strings.TrimSuffix(strings.TrimSpace(ev.Text), " …")
				}
			}
			p.Updated = time.Now().UTC()
			return nil
		})
	})
	if err != nil {
		return fail(err)
	}
	_, err = lib.Mutate(st.ID, func(s *styles.Style) error {
		s.Progress.Phase, s.Progress.ETA = "", 0
		if res.Stopped {
			s.Status = styles.Paused
			return nil
		}
		s.Status, s.Weights, s.Trained, s.Error = styles.Trained, res.File, time.Now().UTC(), ""
		return nil
	})
	if err != nil {
		return err
	}
	emit(Event{Type: "stats", Data: map[string]any{"seconds": time.Since(started).Seconds(), "step": res.Step}})
	if res.Stopped {
		emit.Log(fmt.Sprintf("stopped at step %d (checkpoint kept)", res.Step))
		return ctx.Err()
	}
	emit.Log(fmt.Sprintf("style %q trained: %d steps in %s", st.Name, res.Step, time.Since(started).Round(time.Second)))
	return nil
}

// aceTrainer prepares an ACE-Step training (workers/train_acestep.py): the tracks cut into segments, the model's
// files, its runtime with the song worker next to the trainer (which imports it).
func (b *MusicBackend) aceTrainer(ctx context.Context, m *Model, st styles.Style, set styles.Settings, stopAll func(), emit Emit) (*pyWorker, map[string]any, error) {
	lib := b.StyleLibrary()
	emit.Status("cutting the tracks into segments …")
	segs, err := lib.Prepare(ctx, st.ID)
	if err != nil {
		return nil, nil, err
	}
	dirs, err := b.modelDirs(ctx, m, emit)
	if err != nil {
		return nil, nil, err
	}
	stopAll()
	rt := b.Runtimes["acestep"]
	if rt == nil {
		return nil, nil, errors.New("no ACE-Step runtime")
	}
	if err := rt.env.Ensure(ctx, b.Root, emit.Log); err != nil {
		return nil, nil, err
	}
	src, err := workerFiles.ReadFile("workers/acestep_worker.py")
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(rt.env.Dir(b.Root), "mlxlab_acestep_worker.py"), src, 0o644); err != nil {
		return nil, nil, err
	}
	var list []map[string]any
	for _, sg := range segs {
		list = append(list, map[string]any{"file": sg.File, "seconds": sg.Seconds, "vocals": sg.Vocals})
	}
	req := map[string]any{"model": m.workerModel, "dirs": dirs, "style_dir": lib.Path(st.ID), "segments": list,
		"trigger": st.Trigger, "steps": set.Steps, "rank": set.Rank, "alpha": set.Alpha, "lr": set.LR, "warmup": set.Warmup,
		"checkpoint_every": set.CheckpointEvery, "sample_every": set.SampleEvery, "sample_seconds": set.SampleSeconds,
		"sample_prompt": set.SamplePrompt, "seed": set.Seed, "captions": true}
	return &pyWorker{env: rt.env, script: "train_acestep.py"}, req, nil
}

// sa3TrainEnv is the Stable Audio 3 trainer's runtime: the song runtime's packages and what the official trainer
// and encoder need besides (tqdm, soundfile), pinned.
var sa3TrainEnv = pyenv.Env{Name: "sa3train", Python: "3.12", Packages: []string{
	"mlx==0.32.3", "numpy==2.5.3", "sentencepiece==0.2.2", "soundfile==0.14.0", "tqdm==4.67.1"}}

// sa3Trainer prepares a Stable Audio 3 training (workers/train_sa3.py driving the runtime's own MLX trainer): the
// model's files and its BASE DiT (training starts from the rectified-flow checkpoint, not the shipped one), the tempo
// of each track (measured once, it goes into the tag prompts), the runtime's code.
func (b *MusicBackend) sa3Trainer(ctx context.Context, m *Model, st styles.Style, set styles.Settings, stopAll func(), emit Emit) (*pyWorker, map[string]any, error) {
	lib := b.StyleLibrary()
	dirs, err := b.modelDirs(ctx, m, emit)
	if err != nil {
		return nil, nil, err
	}
	if len(m.repos) > 0 {
		base := m.repos[0] // the same repository holds the base DiT
		base.key, base.include = "base", []string{"MLX/dit_" + m.workerModel + "-base_f16.npz"}
		baseDir, err := b.fetchRepo(ctx, m, base, emit)
		if err != nil {
			return nil, nil, err
		}
		if baseDir != dirs["weights"] {
			return nil, nil, fmt.Errorf("the base weights landed in %s, not next to the model's in %s", baseDir, dirs["weights"])
		}
	}
	var todo []string
	for _, t := range st.Tracks {
		if t.BPM == 0 {
			todo = append(todo, t.File)
		}
	}
	if len(todo) > 0 {
		emit.Status(fmt.Sprintf("measuring the tempo of %d tracks …", len(todo)))
		bpm, err := trackTempo(ctx, b.Root, todo, emit)
		if err != nil {
			return nil, nil, err
		}
		if st, err = lib.Mutate(st.ID, func(s *styles.Style) error {
			for i := range s.Tracks {
				if v, ok := bpm[s.Tracks[i].File]; ok && v > 0 {
					s.Tracks[i].BPM = v
				}
			}
			return nil
		}); err != nil {
			return nil, nil, err
		}
	}
	rt := b.Runtimes["stableaudio3"]
	if rt == nil || rt.build == nil {
		return nil, nil, errors.New("no Stable Audio 3 runtime")
	}
	args, err := rt.build(ctx, b.Root, emit) // the runtime's code (fetched once)
	if err != nil {
		return nil, nil, err
	}
	stopAll()
	var tracks []map[string]any
	for _, t := range st.Tracks {
		tracks = append(tracks, map[string]any{"file": t.File, "name": t.Name, "bpm": t.BPM})
	}
	req := map[string]any{"model": m.workerModel, "dirs": dirs, "style_dir": lib.Path(st.ID), "tracks": tracks,
		"description": st.Description, "trigger": st.Trigger, "steps": set.Steps, "rank": set.Rank, "alpha": set.Alpha,
		"lr": set.LR, "crop_seconds": set.SegmentSeconds, "checkpoint_every": set.CheckpointEvery,
		"sample_every": set.SampleEvery, "sample_seconds": set.SampleSeconds, "sample_prompt": set.SamplePrompt, "seed": set.Seed}
	return &pyWorker{env: sa3TrainEnv, script: "train_sa3.py", args: args}, req, nil
}

// trackTempo measures the tempo of tracks (librosa's beat tracker, workers/tempo.py), by file; a test replaces it.
var trackTempo = func(ctx context.Context, root string, files []string, emit Emit) (map[string]float64, error) {
	rs, err := measureTempo(ctx, root, 120, files, emit)
	if err != nil {
		return nil, err
	}
	bpm := map[string]float64{}
	for _, r := range rs {
		bpm[r.File] = math.Round(r.Tracked*10) / 10
	}
	return bpm, nil
}

// fetchRepo fetches one more repository of a model (pinned and checked) and returns its folder.
func (b *MusicBackend) fetchRepo(ctx context.Context, m *Model, hr hubRepo, emit Emit) (string, error) {
	var keep func(string) bool
	if len(hr.include) > 0 {
		inc := hr.include
		keep = func(p string) bool { return models.Matches(p, inc...) }
	}
	hm, err := b.Store.FromHub(ctx, m.Name, hr.repo, hr.rev, keep)
	if errors.Is(err, models.ErrGated) {
		return "", GatedError(hr.repo)
	}
	if err != nil {
		return "", err
	}
	return fetch(ctx, b.Store, hm, emit)
}

// runTrainer sends the request and passes the trainer's events on (to on, and status, log and progress to emit)
// until its result. A cancel asks it to stop ({"stop": true}: it writes a checkpoint and answers); it is killed when
// it does not answer within stopGrace.
func runTrainer(ctx context.Context, w *pyWorker, req any, emit Emit, on func(trainEvent)) (trainEvent, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return trainEvent{}, err
	}
	if _, err := w.stdin.Write(append(b, '\n')); err != nil {
		w.stop()
		return trainEvent{}, fmt.Errorf("the trainer: %w", err)
	}
	evs := make(chan trainEvent)
	events := w.events
	go func() {
		defer close(evs)
		for events.Scan() {
			var ev trainEvent
			if json.Unmarshal(events.Bytes(), &ev) == nil {
				evs <- ev
				if ev.Type == "result" || ev.Type == "error" {
					return
				}
			}
		}
	}()
	done := ctx.Done()
	var kill <-chan time.Time
	for {
		select {
		case <-done:
			done = nil
			emit.Status("stopping: writing a checkpoint …")
			_, _ = w.stdin.Write([]byte("{\"stop\": true}\n"))
			kill = time.After(stopGrace)
		case <-kill:
			_ = w.cmd.Process.Kill()
			<-w.exited
			for range evs {
			}
			w.cmd = nil
			return trainEvent{}, ctx.Err()
		case s := <-w.logs:
			emit.Log(s)
		case ev, ok := <-evs:
			if !ok {
				w.stop()
			drain: // its last words (a traceback) are in the log
				for {
					select {
					case s := <-w.logs:
						emit.Log(s)
					case <-time.After(200 * time.Millisecond):
						break drain
					}
				}
				if ctx.Err() != nil {
					return trainEvent{}, ctx.Err()
				}
				return trainEvent{}, errors.New("the trainer stopped (see the log)")
			}
			switch ev.Type {
			case "status":
				emit.Status(ev.Text)
			case "log":
				emit.Log(ev.Text)
			case "progress":
				if ev.Total > 0 {
					emit.Progress(ev.Step, ev.Total)
				}
			case "error":
				return ev, errors.New(ev.Text)
			case "result":
				return ev, nil
			}
			if on != nil {
				on(ev)
			}
		}
	}
}
