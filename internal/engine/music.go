package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/audio"
	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/pyenv"
	"github.com/philipparndt/ai-connector/internal/styles"
)

// hubRepo is one Hugging Face repository a model needs, pinned; the worker finds it under dirs[key].
type hubRepo struct {
	key, repo, rev string
	include        []string // file patterns (models.Matches); none: every file
}

// musicRuntime is a worker that makes songs: its Python environment, its script, and an optional build step (a
// native binary) whose results become the script's arguments.
type musicRuntime struct {
	name   string
	env    pyenv.Env
	script string
	build  func(ctx context.Context, root string, emit Emit) ([]string, error)
}

// musicBuilds are the song runtimes' build steps (a runtime manifest names them: build).
var musicBuilds = map[string]func(ctx context.Context, root string, emit Emit) ([]string, error){"levo2": buildLeVo2, "sa3": fetchSA3}

// MusicRuntimes are the song runtimes (runtimes/<name>.json, each verified on an M1 Max): their environment on this
// machine (the zero Env where they have none: RuntimeRuns), script and build step.
func MusicRuntimes() map[string]*musicRuntime {
	out := map[string]*musicRuntime{}
	for _, n := range []string{"yue2", "acestep", "levo2", "heartmula", "stableaudio3", "minimax3"} {
		rt := runtimeManifest(n)
		out[n] = &musicRuntime{name: n, script: rt.Script, env: runtimeEnv(n), build: musicBuilds[rt.Build]}
	}
	return out
}

// musicLoRA is a LoRA a song model applies (merged into its DiT): the file in the repository keyed "lora", its
// strength, and the trigger word it was trained with (put in front of the style).
type musicLoRA struct {
	file, trigger string
	scale         float64
}

// musicModels are the catalogue's song models.
func musicModels() []*Model { return builtinModels("music", nil, "") }

// MusicBackend runs the song models (ACE-Step, YuE2, HeartMuLa, LeVo 2): lyrics and a style in, a WAV out. One
// runtime's worker runs at a time; it keeps its model loaded between songs.
type MusicBackend struct {
	Root     string
	Store    *models.Store
	Runtimes map[string]*musicRuntime
	Styles   *styles.Library // trained styles (nil: <Root>/styles)

	mu      sync.Mutex
	workers map[string]*pyWorker
	state   status

	// tweak changes a worker request before it is sent (benchmarks: other settings)
	tweak func(req map[string]any)
}

func (b *MusicBackend) Name() string { return "music" }
func (b *MusicBackend) Heavy() bool  { return true }

func (b *MusicBackend) Loaded() string { return b.state.get() }

func (b *MusicBackend) Unload() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, w := range b.workers {
		w.stop()
	}
	b.state.set("")
}

func (b *MusicBackend) Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rt := b.Runtimes[m.runtime]
	if rt == nil {
		return fmt.Errorf("%s: no runtime %q", m.ID, m.runtime)
	}
	task := r.Task
	if task == "" {
		task = "generate"
	}
	if task == TrainStyle {
		return b.trainStyle(ctx, m, r, emit)
	}
	if task != "generate" {
		if !slices.Contains(m.Tasks, task) {
			return fmt.Errorf("%s cannot %s a song (it can: %s)", m.Name, task, strings.Join(append([]string{"generate"}, m.Tasks...), ", "))
		}
		if r.Source == "" {
			return fmt.Errorf("%s needs a song to work on", task)
		}
		if _, err := os.Stat(r.Source); err != nil {
			return fmt.Errorf("the song to %s: %w", task, err)
		}
	} else if m.runtime == "stableaudio3" && strings.TrimSpace(r.Prompt) == "" {
		return errors.New("describe the music: this model makes instrumentals from the style alone")
	} else if m.runtime == "minimax3" && strings.TrimSpace(r.Prompt) == "" {
		return errors.New("describe the music (genre, tempo, vocals, arrangement): MiniMax Music 3 needs a music description")
	} else if strings.TrimSpace(r.Prompt) == "" && strings.TrimSpace(r.Lyrics) == "" {
		return errors.New("a style or lyrics")
	}
	// the style first: what it recommends (a community LoRA's steps, guidance, shift) fills what the request left
	// on Auto, before the model's defaults
	lora, err := b.styleFor(ctx, m, &r, emit)
	if err != nil {
		return err
	}
	if lora != nil && lora.recommended != nil {
		rec, used := lora.recommended, []string{}
		if r.Steps == 0 && rec.Steps > 0 {
			r.Steps = rec.Steps
			used = append(used, fmt.Sprintf("%d steps", rec.Steps))
		}
		if r.Guidance == 0 && rec.Guidance > 0 {
			r.Guidance = rec.Guidance
			used = append(used, fmt.Sprintf("guidance %g", rec.Guidance))
		}
		if r.Shift == 0 && rec.Shift > 0 {
			r.Shift = rec.Shift
			used = append(used, fmt.Sprintf("shift %g", rec.Shift))
		}
		if len(used) > 0 {
			emit.Log("the style's recommended settings: " + strings.Join(used, ", "))
		}
	}
	d := m.Defaults
	if r.Duration <= 0 {
		r.Duration = d.Duration
	}
	if m.maxSeconds > 0 && r.Duration > m.maxSeconds {
		emit.Log(fmt.Sprintf("%s makes at most %g s: %g s instead of %g s", m.Name, m.maxSeconds, m.maxSeconds, r.Duration))
		r.Duration = m.maxSeconds
	}
	if r.Steps == 0 {
		r.Steps = d.Steps
	}
	if r.Guidance == 0 {
		r.Guidance = d.Guidance
	}
	if r.Seed == 0 {
		r.Seed = rand.Int64N(1_000_000_000)
	}
	if m.runtime == "stableaudio3" && strings.TrimSpace(r.Lyrics) != "" && !r.Instrumental {
		emit.Log(m.Name + " makes instrumental music only: the lyrics are not used")
	}

	// the files: every repository the model needs, pinned and checked
	dirs := map[string]string{}
	var total int64
	repos := m.repos
	if m.runtime == "acestep" && r.Planner == "4b" {
		repos = append(slices.Clone(repos), lm4B)
	}
	for _, hr := range repos {
		var keep func(string) bool
		if len(hr.include) > 0 {
			inc := hr.include
			keep = func(p string) bool { return models.Matches(p, inc...) }
		}
		hm, err := b.Store.FromHub(ctx, m.Name, hr.repo, hr.rev, keep)
		if errors.Is(err, models.ErrGated) {
			return GatedError(hr.repo)
		}
		if err != nil {
			return err
		}
		total += hm.Size()
		dir, err := fetch(ctx, b.Store, hm, emit)
		if err != nil {
			return err
		}
		dirs[hr.key] = dir
	}
	if len(repos) == len(m.repos) { // (with an optional extra the total is not the model's size)
		m.Size, m.Here, m.Where = total, true, b.Store.Dir
	}

	// one runtime at a time: another one's model leaves memory first
	if b.workers == nil {
		b.workers = map[string]*pyWorker{}
	}
	for name, w := range b.workers {
		if name != rt.name && w.running() {
			emit.Status("unloading " + name + " …")
			w.stop()
		}
	}
	w := b.workers[rt.name]
	if w == nil || !w.running() {
		var args []string
		if rt.build != nil {
			var err error
			if args, err = rt.build(ctx, b.Root, emit); err != nil {
				return err
			}
		}
		w = &pyWorker{env: rt.env, script: rt.script, args: args}
		b.workers[rt.name] = w
		if err := w.start(ctx, b.Root, emit); err != nil {
			return err
		}
	}

	out := filepath.Join(outDir, fmt.Sprintf("%s-%d.wav", time.Now().Format("20060102-150405"), r.Seed))
	req, err := musicRequest(m, r, task, dirs, out)
	if lora != nil { // a trained style: its LoRA folder as dirs.lora (the worker merges it, workers/acestep_worker.py)
		dirs["lora"] = lora.dir
		req["lora"] = map[string]any{"file": lora.file, "scale": lora.scale}
	}
	if err != nil {
		return err
	}
	if b.tweak != nil {
		b.tweak(req)
	}
	bpm, _ := req["bpm"].(int)
	tp := tempoPlan(r, task, bpm)
	stats := map[string]any{"seed": r.Seed, "duration": r.Duration, "steps": r.Steps, "lyricsSent": req["lyrics"], "styleSent": req["prompt"]}
	if bpm > 0 {
		stats["bpm"], stats["bpmAsked"] = bpm, bpm
	}
	emit(Event{Type: "stats", Data: stats})
	b.state.set(m.ID) // loading counts as loaded: it takes the memory

	// one song, or the best of several takes (the one closest to the tempo asked for)
	var ev workerEvent
	var takes []takeResult
	for i := 0; i < tp.takes; i++ {
		treq := req
		if tp.takes > 1 {
			treq = maps.Clone(req)
			treq["seed"] = r.Seed + int64(i)
			treq["output"] = strings.TrimSuffix(out, ".wav") + fmt.Sprintf("-take%d.wav", i+1)
			emit.Status(fmt.Sprintf("take %d of %d …", i+1, tp.takes))
		}
		tev, err := w.call(ctx, treq, emit)
		if err != nil {
			if !w.running() {
				b.state.set("")
			}
			for _, t := range takes {
				_ = os.Remove(t.file)
			}
			return err
		}
		if i == 0 {
			ev = tev
		}
		takes = append(takes, takeResult{file: tev.File, seed: treq["seed"].(int64), ev: tev})
	}
	var tempo map[string]any
	if tp.measure {
		var err error
		if ev, tempo, err = b.settleTempo(ctx, takes, bpm, tp.lock, out, emit); err != nil {
			return err
		}
	}
	// a song cut at its length (still at full loudness in its last second) fades out over its last 3 s; one that
	// ends quietly stays as it is (audio.FadeCut). After the tempo lock: the stretched song is the one that goes out.
	faded := false
	if fadesEnd(task) {
		var err error
		if faded, err = audio.FadeCut(ev.File); err != nil {
			emit.Log("fade-out: " + err.Error()) // the song is there: it goes out as made
		} else if faded {
			emit.Log("the song was still going at its length: faded out over its last seconds")
		}
	}
	emit(Event{Type: "file", File: ev.File})
	if f, ok := tempo["original"].(string); ok {
		emit(Event{Type: "file", File: f}) // the song as the model made it, before the tempo lock
		delete(tempo, "original")
	}
	stats = map[string]any{"seconds": ev.Seconds}
	if ev.Duration > 0 {
		stats["duration"] = ev.Duration
	}
	if ev.PeakGB > 0 {
		stats["peakGB"] = ev.PeakGB
	}
	if fadesEnd(task) {
		stats["faded"] = faded
	}
	maps.Copy(stats, tempo)
	emit(Event{Type: "stats", Data: stats})
	emit.Log(fmt.Sprintf("%.0f s of audio in %.1f s", ev.Duration, ev.Seconds))
	return nil
}

// fadesEnd: a task whose song ends where the length runs out (a new song, a cover, an extension, a restyle) gets
// its end faded when it is cut; repaint and lego keep the source's end, extract makes a stem to mix.
func fadesEnd(task string) bool {
	return task == "generate" || task == "cover" || task == "extend" || task == "restyle"
}

// modelDirs fetches every repository a song model needs (pinned and checked) and names their folders by key.
func (b *MusicBackend) modelDirs(ctx context.Context, m *Model, emit Emit) (map[string]string, error) {
	dirs := map[string]string{}
	var total int64
	for _, hr := range m.repos {
		var keep func(string) bool
		if len(hr.include) > 0 {
			inc := hr.include
			keep = func(p string) bool { return models.Matches(p, inc...) }
		}
		hm, err := b.Store.FromHub(ctx, m.Name, hr.repo, hr.rev, keep)
		if errors.Is(err, models.ErrGated) {
			return nil, GatedError(hr.repo)
		}
		if err != nil {
			return nil, err
		}
		total += hm.Size()
		dir, err := fetch(ctx, b.Store, hm, emit)
		if err != nil {
			return nil, err
		}
		dirs[hr.key] = dir
	}
	m.Size, m.Here, m.Where = total, true, b.Store.Dir
	return dirs, nil
}

// lm4B is ACE-Step's larger song planner (Request.Planner "4b"), fetched only when asked for.
var lm4B = hubRepo{key: "lm", repo: "ACE-Step/acestep-5Hz-lm-4B", rev: "0a3ec94b557aea7d508da38b31cfe7341f6ff737"}

// musicRequest is the worker's request for a song job (r has the model's defaults filled in already).
func musicRequest(m *Model, r Request, task string, dirs map[string]string, out string) (map[string]any, error) {
	// canonical lyrics in this model's format; the directions taken out of them go to the style (lyrics.go)
	lyrics, style := TranslateLyrics(m.runtime, r.Lyrics, r.Prompt)
	req := map[string]any{"model": m.workerModel, "dirs": dirs, "prompt": style, "lyrics": lyrics,
		"instrumental": r.Instrumental, "duration": r.Duration, "steps": r.Steps, "seed": r.Seed, "output": out}
	bpm := r.BPM
	if bpm <= 0 {
		bpm = styleBPM(r.Prompt)
	}
	switch m.runtime {
	case "acestep":
		if bpm > 0 {
			req["bpm"] = bpm
		}
		if r.Guidance > 0 {
			req["guidance"] = r.Guidance // (turbo models ignore it: the library forces 1.0 — no CFG)
		}
		if r.Shift > 0 {
			req["shift"] = r.Shift // the timestep shift (0: the worker's per-model default, 3.0 turbo / 1.0 base)
		}
		// (the planner still rewrites the caption at its default temperature: keeping the caption as written and
		// sampling at 0.65 measured no better for EDM — docs/music.md; the worker still takes cot_caption and
		// lm_temperature)
		switch r.Planner {
		case "4b":
			req["lm_name"] = "acestep-5Hz-lm-4B"
		case "off":
			req["lm"] = false
		}
		if l := m.lora; l != nil {
			req["lora"] = map[string]any{"file": l.file, "scale": l.scale}
			if !strings.Contains(r.Prompt, l.trigger) {
				req["prompt"] = l.trigger + ", " + r.Prompt
			}
		}
	case "stableaudio3":
		req["prompt"] = withBPM(r.Prompt, bpm)
		delete(req, "lyrics")
	case "minimax3":
		// the tempo goes into the description (the model reads "BPM: 96"); an instrumental gets wordless sections
		// with an explicit "(instrumental)" line (tags alone condition it weakly)
		req["prompt"] = withBPM(style, bpm)
		if r.Instrumental || strings.TrimSpace(lyrics) == "" {
			req["lyrics"] = minimax3Instrumental
			if !instrumentalWord.MatchString(style) {
				req["prompt"] = addToStyle(req["prompt"].(string), []string{"instrumental", "no vocals"})
			}
		}
	}
	if bpm > 0 {
		req["bpm"] = bpm
	}
	if task == "generate" {
		return req, nil
	}
	req["task"], req["src_audio"] = task, r.Source
	switch task {
	case "repaint", "lego":
		req["start"], req["end"] = r.Start, r.End
	case "cover":
		req["strength"] = r.Strength
	case "extend":
		req["extend"] = r.Extend
	}
	if r.Track != "" {
		req["track"] = r.Track
	}
	if task == "restyle" {
		// YuE2 writes each song's melody next to it as an ABC score; restyle sings it (or an edited one) anew
		abc := r.ABC
		if strings.TrimSpace(abc) == "" {
			b, err := os.ReadFile(strings.TrimSuffix(r.Source, filepath.Ext(r.Source)) + ".abc")
			if err != nil {
				return nil, errors.New("this song has no melody score: restyle works on songs made by YuE2")
			}
			abc = string(b)
		}
		req["abc_text"] = abc
		delete(req, "task")
		delete(req, "src_audio")
	}
	return req, nil
}

// bpmPattern finds "138 bpm" and, as MiniMax Music 3's structured captions write it, "BPM: 120" / "bpm is 92".
var bpmPattern = regexp.MustCompile(`(?i)\b(\d{2,3})(?:\.\d+)?\s*-?\s*bpm\b|\bbpm(?:\s*[:=]\s*|\s+is\s+|\s+of\s+|\s+)(\d{2,3})(?:\.\d+)?\b`)

// styleBPM is the tempo a style names ("… 138 bpm …", "BPM: 120"), 0 if none (or one outside 30–300).
func styleBPM(style string) int {
	m := bpmPattern.FindStringSubmatch(style)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1] + m[2])
	if n < 30 || n > 300 {
		return 0
	}
	return n
}

// withBPM puts the tempo into a prompt that does not name one yet (Stable Audio 3 takes it from the text).
func withBPM(prompt string, bpm int) string {
	if bpm <= 0 || bpmPattern.MatchString(prompt) {
		return prompt
	}
	return strings.TrimRight(strings.TrimSpace(prompt), ",.") + fmt.Sprintf(", %d BPM", bpm)
}

const levo2Commit = "cfd1635beb5ab400b1c8780a7cfd8eb0629bcafe"

// buildLeVo2 builds LeVo2.cpp (Metal) once into ~/.config/mlxlab/tools/levo2-<commit> and returns its bin directory
// as the worker's argument; the build needs cmake and the Xcode command line tools.
func buildLeVo2(ctx context.Context, root string, emit Emit) ([]string, error) {
	script, err := workerFiles.ReadFile("workers/levo2_build.sh")
	if err != nil {
		return nil, err
	}
	target := filepath.Join(root, "tools", "levo2-"+levo2Commit[:7])
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(target, "build.sh")
	if err := os.WriteFile(path, script, 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(target, ".built-"+levo2Commit)); err != nil {
		emit.Status("building LeVo2.cpp with Metal (once, about a minute) …")
	}
	var bin string
	cmd := exec.CommandContext(ctx, "/bin/bash", path, target, levo2Commit)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	for _, l := range strings.Split(stderr.String(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			emit.Log(l)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("building LeVo2.cpp: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	bin = strings.TrimSpace(lines[len(lines)-1])
	if _, err := os.Stat(filepath.Join(bin, "levo-cli")); err != nil {
		return nil, fmt.Errorf("the LeVo2.cpp build made no levo-cli in %s", bin)
	}
	return []string{bin}, nil
}
