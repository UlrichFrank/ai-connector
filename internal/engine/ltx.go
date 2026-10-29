package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/pyenv"
)

// LTXEnv is the pinned ltx-2-mlx runtime (github.com/dgrauet/ltx-2-mlx, MIT; LTX-2.5 packs since 0.15).
var LTXEnv = runtimeEnv("ltx-2-mlx") // runtimes/ltx-2-mlx.json

// LTXBackend runs LTX-2.5 through ltx-2-mlx, one process per video (the pipeline loads and frees its parts stage by
// stage anyway: Gemma, the transformer, the upscaler, the decoders): the distilled two-stage pipeline, or for the HQ
// models the two-stage HQ one (the dev transformer with CFG at half size, then the distilled LoRA at full size).
type LTXBackend struct {
	Root  string
	Store *models.Store
	// renderClip replaces the real render in tests
	renderClip func(prompt string, frames int, images []string, seed int64, out string) error

	mu sync.Mutex
}

func (b *LTXBackend) Name() string   { return "ltx" }
func (b *LTXBackend) Heavy() bool    { return true }
func (b *LTXBackend) Loaded() string { return "" }
func (b *LTXBackend) Unload()        {}

var (
	tqdmStep = regexp.MustCompile(`(\d+)/(\d+) \[`)
	tqdmDesc = regexp.MustCompile(`^([A-Za-z][^:]*): +\d+%`)
	phase    = regexp.MustCompile(`^\[([^\]]+)\] \.\.\.$`)
)

// ltxRun is how a clip is made: the pack's folder and the pipeline.
type ltxRun struct {
	dir string
	hq  bool // the two-stage HQ pipeline (the dev transformer); else the distilled one
}

// download fetches the pack's files the model's pipeline needs into the store (resumable, checked) and returns its
// directory.
func (b *LTXBackend) download(ctx context.Context, m *Model, emit Emit) (string, error) {
	hm, err := hubModel(ctx, b.Store, m, func(p string) bool { return !models.Matches(p, m.ignore...) })
	if err != nil {
		return "", err
	}
	m.Size = hm.Size()
	dir, err := fetch(ctx, b.Store, hm, emit)
	if err != nil {
		return "", err
	}
	m.Here, m.Where = true, dir
	return dir, nil
}

func (b *LTXBackend) Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Prompt == "" && len(r.Script) == 0 {
		return errors.New("no prompt")
	}
	d := m.Defaults
	r.Width, r.Height = orInt(r.Width, d.Width)/64*64, orInt(r.Height, d.Height)/64*64 // the two-stage pipeline: multiples of 64
	r.FPS = orInt(r.FPS, d.FPS)
	r.Frames = frames8k1(r.Frames)
	if m.hq {
		r.Steps, r.Guidance = orInt(r.Steps, d.Steps), cmp.Or(r.Guidance, d.Guidance)
	} else {
		r.Steps, r.Guidance = 0, 0 // the distilled pipeline has its own schedule and no CFG
	}
	if r.Seed == 0 {
		r.Seed = rand.Int64N(1_000_000_000)
	}
	dir, err := b.download(ctx, m, emit)
	if err != nil {
		return err
	}
	if err := LTXEnv.Ensure(ctx, b.Root, emit.Log); err != nil {
		return err
	}
	start := time.Now()
	name := fmt.Sprintf("%s-%d", time.Now().Format("20060102-150405"), r.Seed)
	stats := map[string]any{"seed": r.Seed, "width": r.Width, "height": r.Height, "frames": r.Frames, "fps": r.FPS}
	if m.hq {
		stats["steps"], stats["guidance"] = r.Steps, r.Guidance
	}
	emit(Event{Type: "stats", Data: stats})
	p := ltxRun{dir: dir, hq: m.hq}
	var out string
	if len(r.Script) > 0 {
		out, err = b.movie(ctx, p, m.ID, r, outDir, name, emit)
	} else {
		out = filepath.Join(outDir, name+".mp4")
		err = b.render(ctx, p, r, r.Prompt, r.Frames, r.Images, r.Seed, out, emit)
	}
	if err != nil {
		return err
	}
	emit(Event{Type: "file", File: out})
	emit(Event{Type: "stats", Data: map[string]any{"seconds": time.Since(start).Seconds()}})
	emit.Log(fmt.Sprintf("video in %.0f s", time.Since(start).Seconds()))
	return nil
}

// frames8k1 rounds a frame count down to 8k+1 (what the VAE's time compression wants); 0 stays 0 (LTX-2.5 predicts
// the length).
func frames8k1(n int) int {
	if n <= 0 {
		return 0
	}
	return max(9, (n-1)/8*8+1)
}

// gpuWatchdog: macOS stopped the job because it held the GPU too long while the display was on (Metal's command
// buffer timeout; ltx-2-mlx then names AGX_RELAX_CDM_CTXSTORE_TIMEOUT in its message).
func gpuWatchdog(s string) bool {
	return strings.Contains(s, "AGX_RELAX_CDM_CTXSTORE_TIMEOUT") || strings.Contains(s, "ImpactingInteractivity") ||
		strings.Contains(s, "GPU Timeout") || strings.Contains(s, "watchdog")
}

// render makes one clip; when macOS's GPU watchdog stops it, it tries once more with --low-ram (the transformer runs
// block by block with a sync after each, so no single GPU job runs long; the HQ pipeline then streams the distilled
// transformer for stage 2 instead of fusing the LoRA into the dev one).
func (b *LTXBackend) render(ctx context.Context, p ltxRun, r Request, prompt string, frames int, images []string, seed int64, out string, emit Emit) error {
	if b.renderClip != nil {
		return b.renderClip(prompt, frames, images, seed, out)
	}
	err := b.renderOnce(ctx, p, r, prompt, frames, images, seed, out, emit)
	if err == nil || ctx.Err() != nil || !gpuWatchdog(err.Error()) {
		return err
	}
	if r.LowRAM {
		return fmt.Errorf("macOS stopped the GPU job (its watchdog) even with --low-ram: let the display sleep while it renders, or use a smaller size or fewer frames")
	}
	emit.Log("macOS's GPU watchdog stopped the render; trying again with --low-ram (block by block)")
	emit.Status("GPU watchdog: retrying with low RAM …")
	r.LowRAM = true
	if err := b.renderOnce(ctx, p, r, prompt, frames, images, seed, out, emit); err != nil {
		if gpuWatchdog(err.Error()) {
			return fmt.Errorf("macOS stopped the GPU job (its watchdog), also with --low-ram: let the display sleep while it renders, or use a smaller size or fewer frames")
		}
		return err
	}
	return nil
}

// ltxArgs are ltx-2-mlx's arguments for one clip (images: conditioning frames, the first one at frame 0).
func ltxArgs(p ltxRun, r Request, prompt string, frames int, images []string, seed int64, out string) []string {
	mode := "--distilled"
	if p.hq {
		mode = "--two-stages-hq"
	}
	args := []string{"generate", mode, "--model", p.dir, "--prompt", prompt, "--output", out,
		"--width", strconv.Itoa(r.Width), "--height", strconv.Itoa(r.Height), "--frame-rate", strconv.Itoa(r.FPS),
		"--seed", strconv.FormatInt(seed, 10)}
	if p.hq {
		// stage 1's steps and CFG (stage 2 has the distilled LoRA's 3 steps); the negative prompt is ltx-2-mlx's
		if r.Steps > 0 {
			args = append(args, "--stage1-steps", strconv.Itoa(r.Steps))
		}
		if r.Guidance > 0 {
			args = append(args, "--cfg-scale", strconv.FormatFloat(r.Guidance, 'g', -1, 64))
		}
	}
	if frames > 0 {
		args = append(args, "--frames", strconv.Itoa(frames))
	}
	if r.LowRAM {
		args = append(args, "--low-ram")
	}
	if r.NoAudio {
		args = append(args, "--no-audio")
	}
	for _, img := range images {
		args = append(args, "--image", img)
	}
	return args
}

// hqStage names the stage of the HQ pipeline a denoising progress bar of ltx-2-mlx belongs to (by its tqdm
// description: stage 1 samples with guidance, stage 2 without), "" for other bars.
func hqStage(desc string) string {
	switch {
	case !strings.HasPrefix(desc, "Denoising"):
		return ""
	case strings.Contains(desc, "guided"):
		return "stage 1/2: dev model with CFG at half size"
	}
	return "stage 2/2: refining at full size"
}

// renderOnce makes one clip with ltx-2-mlx.
func (b *LTXBackend) renderOnce(ctx context.Context, p ltxRun, r Request, prompt string, frames int, images []string, seed int64, out string, emit Emit) error {
	args := ltxArgs(p, r, prompt, frames, images, seed, out)
	emit.Status("starting ltx-2-mlx …")
	cmd := exec.CommandContext(ctx, LTXEnv.Bin(b.Root, "ltx-2-mlx"), args...)
	// a longer GPU watchdog for this process (long Metal jobs while the display is on); see gpuWatchdog
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1", "AGX_RELAX_CDM_CTXSTORE_TIMEOUT=1")
	stage := ""
	err := pyenv.Stream(ctx, cmd, func(s string) {
		if sm := phase.FindStringSubmatch(s); sm != nil {
			emit.Status(sm[1] + " …")
		}
		if sm := tqdmDesc.FindStringSubmatch(s); p.hq && sm != nil {
			if st := hqStage(sm[1]); st != "" && st != stage {
				stage = st
				emit.Status(st + " …")
				emit.Log(st)
			}
		}
		if sm := tqdmStep.FindStringSubmatch(s); sm != nil {
			step, _ := strconv.Atoi(sm[1])
			n, _ := strconv.Atoi(sm[2])
			emit.Progress(step, n)
			return // progress bars would flood the log
		}
		emit.Log(s)
	})
	if err != nil {
		return err
	}
	if _, err := os.Stat(out); err != nil {
		return fmt.Errorf("ltx-2-mlx wrote no video (%s)", out)
	}
	return nil
}

// ltxModels are the LTX-2.5 video models (catalog/ltx-2.5-*.json): the distilled two-stage pipeline (8 + 3 steps, no
// CFG) and the HQ one, the open dev model (the closest local thing to Lightricks' hosted LTX-2.5 Pro): stage 1 at half
// size with the dev transformer, the res_2s sampler and real CFG, stage 2 at full size with the distilled LoRA fused
// in. The two of a quantisation share one folder of the store: the HQ model downloads only the dev transformer and
// the LoRA on top.
func ltxModels(store *models.Store) []*Model { return builtinModels("ltx", store, "") }
