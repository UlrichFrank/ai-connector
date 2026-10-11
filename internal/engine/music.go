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

// MusicRuntimes are the song runtimes, pinned (each verified on an M1 Max).
func MusicRuntimes() map[string]*musicRuntime {
	return map[string]*musicRuntime{
		// the pure-MLX port of YuE2 (huggingface.co/ahmadw/YuE2-3B-MLX): its inference code comes with the weights
		"yue2": {name: "yue2", script: "yue2_worker.py", env: pyenv.Env{Name: "yue2", Python: "3.12",
			Packages: []string{"mlx==0.32.3", "numpy==2.5.3", "tiktoken==0.14.0"}}},
		// ACE-Step 1.5 (github.com/ace-step/ACE-Step-1.5, MIT): MLX diffusion and MLX language model on the Mac;
		// lego and extract run on PyTorch/MPS (the MLX path got them wrong)
		"acestep": {name: "acestep", script: "acestep_worker.py", env: pyenv.Env{Name: "acestep", Python: "3.12",
			Packages: []string{"ace-step @ git+https://github.com/ace-step/ACE-Step-1.5@ca1e85fe9430179831e6bc6be790c332190a3866"}}},
		// LeVo 2 / SongGeneration v2 through its ggml port (github.com/ckadirt/LeVo2.cpp), built once with Metal;
		// the worker (standard library only) runs its two programs: lyrics → tokens → song
		"levo2": {name: "levo2", script: "levo2_worker.py", env: pyenv.Env{Name: "levo2", Python: "3.12"}, build: buildLeVo2},
		// HeartMuLa (github.com/HeartMuLa/heartlib, Apache-2.0) on PyTorch/MPS: bf16 language model, fp32 codec
		"heartmula": {name: "heartmula", script: "heartmula_worker.py", env: pyenv.Env{Name: "heartmula", Python: "3.12",
			Packages: []string{"heartlib @ git+https://github.com/HeartMuLa/heartlib@a18c8cb54a55b4c24d48a2842542f81da7dedcd2"}}},
		// Stable Audio 3 (github.com/Stability-AI/stable-audio-3): its own pure-MLX runtime (optimized/mlx, no
		// PyTorch), fetched once at a pinned commit; the worker imports it
		"stableaudio3": {name: "stableaudio3", script: "stableaudio3.py", env: pyenv.Env{Name: "stableaudio3", Python: "3.12",
			Packages: []string{"mlx==0.32.3", "numpy==2.5.3", "sentencepiece==0.2.2"}}, build: fetchSA3},
		// MiniMax Music 3 through the pure-MLX port github.com/mikolaj92/minimax-music3-mlx (Apache-2.0), pinned;
		// its own quantised weights (mikoy92/MiniMax-Music3-MLX-8bit, -4bit)
		"minimax3": {name: "minimax3", script: "minimax3.py", env: pyenv.Env{Name: "minimax3", Python: "3.12",
			Packages: append([]string{"minimax-music3-mlx @ git+https://github.com/mikolaj92/minimax-music3-mlx@" + minimax3Commit},
				minimax3Pins...)}},
	}
}

// musicLoRA is a LoRA a song model applies (merged into its DiT): the file in the repository keyed "lora", its
// strength, and the trigger word it was trained with (put in front of the style).
type musicLoRA struct {
	file, trigger string
	scale         float64
}

// musicModels are the catalogue's song models.
func musicModels() []*Model {
	yue2 := []hubRepo{{key: "yue2", repo: "ahmadw/YuE2-3B-MLX", rev: "fe0a9050fd658257b486b880422d8872ee1f81e3", include: []string{"*.py", "8bit/"}}}
	heart := []hubRepo{
		{key: "gen", repo: "HeartMuLa/HeartMuLaGen", rev: "9906b2bcd4598772a32cad4aec0760170fe0d177", include: []string{"*.json"}},
		{key: "mula", repo: "HeartMuLa/HeartMuLa-oss-3B-happy-new-year", rev: "41f6fc68490e11dc43fdabaa6b5767946408c903", include: []string{"*.json", "*.safetensors"}},
		{key: "codec", repo: "HeartMuLa/HeartCodec-oss-20260123", rev: "f889dab0532cfa4bf459f2a3367eb6d346b8eeda", include: []string{"*.json", "*.safetensors"}},
	}
	levo := func(size string) []hubRepo {
		return []hubRepo{{key: "levo2", repo: "ckadirt/LeVo2-GGUF", rev: "9d7b5746fdc74fdc80f85295e7b6c783be3703da",
			include: []string{"LeVo2-v2-" + size + "-Q6_K.gguf*", "LeVo2-v2-flow-Q6_K.gguf*", "LeVo2-v2-vae-F16.gguf*"}}}
	}
	aceMain := hubRepo{key: "main", repo: "ACE-Step/Ace-Step1.5", rev: "19671f406d603126926c1b7e2adc169acbcade22"}
	xlTurbo := hubRepo{key: "xl-turbo", repo: "ACE-Step/acestep-v15-xl-turbo", rev: "d4a0b288b83ebb7e25a8c0b32c573c22e134e8ee"}
	edit := []string{"cover", "repaint", "extend"}
	// Stable Audio 3: one repository holds every runtime's weights; a model needs its DiT, its codec and the
	// shared text encoder (T5Gemma) of the MLX folder
	sa3 := func(dit, codec string) []hubRepo {
		return []hubRepo{{key: "weights", repo: "stabilityai/stable-audio-3-optimized", rev: "da6edc54ddba10bfd79a077102ded687f80e882b",
			include: []string{"MLX/t5gemma_f16.npz", "MLX/dit_" + dit + "_f16.npz", "MLX/" + codec + "_decoder_f32.npz", "MLX/" + codec + "_encoder_f32.npz"}}}
	}
	// MiniMax Music 3: one repository per quantisation, all of it (tokenizer, configs and the five components)
	mm3 := func(repo, rev string) []hubRepo { return []hubRepo{{key: "weights", repo: repo, rev: rev}} }
	return withStyles([]*Model{
		{ID: "ace-step-1.5-turbo", Name: "ACE-Step 1.5 turbo", Kind: Music, Backend: "music", runtime: "acestep", workerModel: "ace-step-1.5-turbo",
			repos: []hubRepo{aceMain}, Tasks: edit,
			About: "ACE-Step 1.5 (MIT): songs with vocals in 50+ languages, on MLX — about 11 s for 30 s; remix, replace a part, extend",
			Size:  10090 << 20, Defaults: Request{Duration: 60}},
		{ID: "ace-step-1.5-xl-turbo", Name: "ACE-Step 1.5 XL turbo", Kind: Music, Backend: "music", runtime: "acestep", workerModel: "ace-step-1.5-xl-turbo",
			repos: []hubRepo{aceMain, xlTurbo}, Tasks: edit,
			About: "ACE-Step 1.5 with the 4B XL DiT: better sound, about 21 s for 30 s, 28 GB peak",
			Size:  30040 << 20, Defaults: Request{Duration: 60}},
		{ID: "ace-step-1.5-base", Name: "ACE-Step 1.5 base", Kind: Music, Backend: "music", runtime: "acestep", workerModel: "ace-step-1.5-base",
			repos: []hubRepo{aceMain, {key: "base", repo: "ACE-Step/acestep-v15-base", rev: "e432212fec32b8965a14ffa57ae653438d6abd14"}},
			Tasks: []string{"cover", "repaint", "extend", "lego", "extract"},
			About: "ACE-Step 1.5 base (50 steps, CFG 7.5): slower, follows the style and tempo more closely; also adds an instrument track and pulls out stems",
			Size:  14880 << 20, Defaults: Request{Duration: 60, Steps: 50, Guidance: 7.5}},
		{ID: "ace-step-1.5-xl-base", Name: "ACE-Step 1.5 XL base", Kind: Music, Backend: "music", runtime: "acestep", workerModel: "ace-step-1.5-xl-base",
			repos: []hubRepo{aceMain, {key: "xl-base", repo: "ACE-Step/acestep-v15-xl-base", rev: "220c1166efbdd9583eafcb12eb160594bbfcb241"}}, Tasks: edit,
			About: "ACE-Step 1.5 with the undistilled 4B XL DiT (50 steps, CFG 7.5): the slowest ACE-Step, follows the style most closely",
			Size:  29120 << 20, Defaults: Request{Duration: 60, Steps: 50, Guidance: 7.5}},
		{ID: "ace-step-1.5-xl-turbo-techno", Name: "ACE-Step 1.5 XL turbo + techno LoRA", Kind: Music, Backend: "music", runtime: "acestep", workerModel: "ace-step-1.5-xl-turbo",
			repos: []hubRepo{aceMain, xlTurbo, {key: "lora", repo: "ryanontheinside/techno-acestep1.5-xl-v1", rev: "5154315313eb08bf9ff4d685b611706bfb4a2832",
				include: []string{"adapter_config.json", "techno-xl-v1.safetensors"}}},
			lora: &musicLoRA{file: "techno-xl-v1.safetensors", trigger: "roti-t3kn0", scale: 1}, Tasks: edit,
			About: "ACE-Step 1.5 XL turbo with a community techno LoRA (RyanOnTheInside, rank 64, trained on 15 tracks): driving, dark and minimal techno; the LoRA states no licence",
			Size:  30200 << 20, Defaults: Request{Duration: 60}},
		{ID: "stable-audio-3-small", Name: "Stable Audio 3 Small (music)", Kind: Music, Backend: "music", runtime: "stableaudio3", workerModel: "sm-music",
			repos: sa3("sm-music", "same_s"), Tasks: edit, maxSeconds: 120,
			About: "Stable Audio 3 Small-Music (433M), pure MLX: instrumental only (no vocals; lyrics are ignored), up to 2 min, " +
				"a few seconds per song; the tempo goes in the style (\"128 BPM\"). Remix, replace a part, extend. Restricted licence: " + sa3Licence,
			Licence: sa3Licence, Size: 1831 << 20, Defaults: Request{Duration: 60, Steps: 8}},
		{ID: "stable-audio-3-medium", Name: "Stable Audio 3 Medium", Kind: Music, Backend: "music", runtime: "stableaudio3", workerModel: "medium",
			repos: sa3("medium", "same_l"), Tasks: edit, maxSeconds: 380,
			About: "Stable Audio 3 Medium (1.4B), pure MLX: instrumental only (no vocals; lyrics are ignored), up to 6 min 20 s, " +
				"better sound than Small; the tempo goes in the style (\"128 BPM\"). Remix, replace a part, extend. Restricted licence: " + sa3Licence,
			Licence: sa3Licence, Size: 6565 << 20, Defaults: Request{Duration: 60, Steps: 8}},
		{ID: "minimax-music3-8bit", Name: "MiniMax Music 3 (8-bit)", Kind: Music, Backend: "music", runtime: "minimax3", workerModel: "8bit",
			repos: mm3("mikoy92/MiniMax-Music3-MLX-8bit", "a21ab36ebe882b9fcb927bb23c5a914a41efcb96"), maxSeconds: minimax3Longest,
			About: "MiniMax Music 3 (8B song LM + 2.4B flow matching): lyrics and a music description to a full song with vocals, " +
				"up to 5 min, pure MLX (8-bit). " + minimax3Speed + " The length is a ceiling (the model may end the song " +
				"earlier); describe genre, BPM, key, vocals and arrangement. " + minimax3Licence,
			Licence: minimax3Licence, Size: 13363585426, Defaults: Request{Duration: 60, Steps: 30}},
		{ID: "minimax-music3-4bit", Name: "MiniMax Music 3 (4-bit)", Kind: Music, Backend: "music", runtime: "minimax3", workerModel: "4bit",
			repos: mm3("mikoy92/MiniMax-Music3-MLX-4bit", "1d9d9ca4885323e0785c777abb13d25cb37ce4d8"), maxSeconds: minimax3Longest,
			About: "MiniMax Music 3 with the song LM and the flow DiT in 4 bits: for Macs with 16–32 GB; " +
				"rougher than the 8-bit one. " + minimax3Licence,
			Licence: minimax3Licence, Size: 7960010136, Defaults: Request{Duration: 60, Steps: 30}},
		{ID: "levo2-large", Name: "LeVo 2 large (Q6_K)", Kind: Music, Backend: "music", runtime: "levo2", workerModel: "levo2-large", repos: levo("large"),
			About: "Tencent's LeVo 2 / SongGeneration v2, lyrics to song with clear vocals, ggml on Metal: about 5–7 min for 30 s. Research use only",
			Size:  4980 << 20, Defaults: Request{Duration: 30}},
		{ID: "levo2-medium", Name: "LeVo 2 medium (Q6_K)", Kind: Music, Backend: "music", runtime: "levo2", workerModel: "levo2-medium", repos: levo("medium"),
			About: "LeVo 2 with the smaller language model: faster (about 4 min for 30 s), vocals less reliable. Research use only",
			Size:  3080 << 20, Defaults: Request{Duration: 30}},
		{ID: "heartmula-3b", Name: "HeartMuLa 3B", Kind: Music, Backend: "music", runtime: "heartmula", workerModel: "heartmula-3b", repos: heart,
			About: "HeartMuLa (Apache-2.0), lyrics and tags to song, PyTorch on the GPU (MPS): slow, about 5 min for 30 s",
			Size:  21 << 30, Defaults: Request{Duration: 30}},
		{ID: "yue2-3b", Name: "YuE2 3B (8-bit)", Kind: Music, Backend: "music", runtime: "yue2", workerModel: "yue2-3b-8bit", repos: yue2,
			Tasks: []string{"restyle"},
			About: "YuE2 (m-a-p), lyrics to song with vocals, pure MLX; plans an ABC score first, so the lyrics set the length (fewer lines: a shorter song). About 1.5 min for 30 s. Non-commercial (CC BY-NC 4.0)",
			Size:  4530 << 20, Defaults: Request{Duration: 30}},
	})
}

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
