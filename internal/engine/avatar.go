package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
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

// Talking avatars: a portrait and speech make a lip-synced talking video (docs/voice-avatar.md).
//
// The speech is the request's Audio, or else its Text said by the Speaker into a wav first. Speech longer than a
// model's clip is split at pauses; every clip after the first starts from the last frame of the one before (as a
// movie's continued clips do). The clips are joined without their own sound and the original speech is muxed in.

// longcatCommit is the pinned commit of the port (runtimes/longcat-avatar.json): the worker gets it as its argument.
var longcatCommit = packageCommit("longcat-avatar", "longcat-video-avatar-mlx")

// LongCatEnv is the pinned LongCat-Video-Avatar MLX runtime (github.com/xocialize/longcat-avatar-mlx, MIT). The
// worker adds the package's models/avatar folder from the same commit (its setuptools list leaves it out).
var LongCatEnv = runtimeEnv("longcat-avatar") // runtimes/longcat-avatar.json

// avatarRuntime is how one family of avatar models animates a clip.
type avatarRuntime struct {
	env    pyenv.Env
	script string
	args   []string
	fps    int
	// frames: the clip lengths the model can make (rounds up to the next one)
	frames func(n int) int
	// dropFirst: the clip's first frame is the conditioning image itself (a continuation drops it at the seam)
	dropFirst bool
	// lowRAMRetry: macOS's GPU watchdog can stop it; retry once with --low-ram
	lowRAMRetry bool
	// refPortrait: every clip takes the portrait as its reference (an identity anchor, not frame 0), not the last
	// frame of the clip before (which lets errors pile up)
	refPortrait bool
	// mult: width and height are multiples of it; maxArea: larger sizes are scaled down to this many pixels
	mult, maxArea int
}

var avatarRuntimes = map[string]avatarRuntime{
	// LTX: the two-stage pipeline wants multiples of 64; up to 640×640 (the web app's sizes)
	"ltx-a2v": {env: LTXEnv, script: "avatar_ltx.py", fps: 24, frames: frames8k1Up, dropFirst: true, lowRAMRetry: true,
		mult: 64, maxArea: 640 * 640},
	// LongCat: Wan's VAE (8) × patch (2); slow, so the area of 256×432 at most
	"longcat": {env: LongCatEnv, script: "avatar_longcat.py", args: []string{longcatCommit}, fps: 25, frames: frames4k1Up,
		refPortrait: true, mult: 16, maxArea: 256 * 432},
}

// frames8k1Up rounds up to 8k+1 (LTX's VAE), frames4k1Up to 4k+1 (Wan's VAE).
func frames8k1Up(n int) int { return max(9, (n-2)/8*8+9) }
func frames4k1Up(n int) int { return max(5, (n-2)/4*4+5) }

const avatarPrompt = "A person talks to the camera with natural facial expressions, the lips move in sync with the speech, subtle head movement, steady camera"

// avatarModels are the avatar entries of the catalogue.
func avatarModels(store *models.Store) []*Model { return builtinModels("avatar", store, "") }

// AvatarBackend makes talking avatars (models of kind Avatar).
type AvatarBackend struct {
	Root  string
	Store *models.Store
	// Speaker says a request's Text (nil: only requests with Audio work).
	Speaker Speaker

	// animate replaces the worker in tests (and download: the model's directory)
	animate  func(ctx context.Context, m *Model, c avatarClip, emit Emit) error
	download func(ctx context.Context, m *Model, emit Emit) (string, error)

	mu      sync.Mutex
	workers map[string]*pyWorker // by runtime
	state   status
}

func (b *AvatarBackend) Name() string   { return "avatar" }
func (b *AvatarBackend) Heavy() bool    { return true }
func (b *AvatarBackend) Loaded() string { return b.state.get() }

func (b *AvatarBackend) Unload() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopWorkers("")
	b.state.set("")
}

func (b *AvatarBackend) stopWorkers(except string) {
	for k, w := range b.workers {
		if k != except {
			w.stop()
		}
	}
}

// avatarClip is one clip for the worker.
type avatarClip struct {
	Model, Audio, Image, Prompt string
	Width, Height, Frames, FPS  int
	Seed                        int64
	LowRAM                      bool
	Out                         string
}

func (b *AvatarBackend) Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rt, ok := avatarRuntimes[m.runtime]
	if !ok {
		return fmt.Errorf("%s is not an avatar model", m.ID)
	}
	if len(r.Images) == 0 || r.Images[0] == "" {
		return errors.New("an avatar needs a portrait (the first image)")
	}
	if !fileExists(r.Images[0]) {
		return fmt.Errorf("no portrait at %s", r.Images[0])
	}
	if r.Audio == "" && strings.TrimSpace(r.Text) == "" {
		return errors.New("an avatar needs speech: an audio file, or a text and a voice")
	}
	if r.Audio == "" && b.Speaker == nil {
		return errors.New("saying a text needs a speech model, and this mlxlab has no speech backend: give an audio file instead")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return errors.New("avatars need ffmpeg (brew install ffmpeg)")
	}
	d := m.Defaults
	r.Width, r.Height = avatarSize(orInt(r.Width, d.Width), orInt(r.Height, d.Height), rt.mult, rt.maxArea)
	r.Frames = rt.frames(orInt(r.Frames, d.Frames)) // the longest clip
	if strings.TrimSpace(r.Prompt) == "" {
		r.Prompt = orStr(d.Prompt, avatarPrompt)
	}
	if r.Seed == 0 {
		r.Seed = rand.Int64N(1_000_000_000)
	}
	start := time.Now()
	name := fmt.Sprintf("%s-%d", time.Now().Format("20060102-150405"), r.Seed)
	work := filepath.Join(outDir, "avatars", name)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	emit(Event{Type: "stats", Data: map[string]any{"seed": r.Seed, "width": r.Width, "height": r.Height, "fps": rt.fps, "clipsDir": work}})

	// 1. the speech
	var spoken string
	src := r.Audio
	if src == "" {
		spoken = filepath.Join(outDir, name+".wav")
		emit.Status("speaking …")
		if err := b.Speaker.Speak(ctx, r.SpeechModel, r, spoken, speakerEmit(emit)); err != nil {
			return fmt.Errorf("speaking the text: %w", err)
		}
		// the speech model's memory (7–13 GB) is better spent on the animation
		if u, ok := b.Speaker.(interface{ Unload() }); ok {
			u.Unload()
		}
		if !fileExists(spoken) {
			return errors.New("the speech model wrote no audio")
		}
		src = spoken // a job file after the video
	} else if src, err = localAudio(src, outDir); err != nil {
		return err
	}
	speech := filepath.Join(work, "speech.wav")
	if err := ffmpegRun(ctx, ffmpeg, "-i", src, "-vn", "-ac", "1", "-ar", "48000", "-c:a", "pcm_s16le", speech); err != nil {
		return fmt.Errorf("reading the audio: %w", err)
	}
	dur, err := wavDuration(speech)
	if err != nil {
		return err
	}
	if dur < 0.2 {
		return errors.New("the speech is too short (under 0.2 s)")
	}

	// 2. the clips
	// the longest stretch of speech one clip shows (a frame of slack for the rounding, one for a dropped first frame)
	maxSec := float64(r.Frames-2) / float64(rt.fps)
	if rt.dropFirst {
		maxSec = float64(r.Frames-3) / float64(rt.fps)
	}
	var silences [][2]float64
	if dur > maxSec {
		emit.Status("finding pauses in the speech …")
		if silences, err = detectSilences(ctx, ffmpeg, speech); err != nil {
			return err
		}
	}
	plan := planClips(splitSpeech(dur, silences, maxSec), rt.fps, rt.frames, rt.dropFirst)
	emit.Log(fmt.Sprintf("speech %.1f s in %d clip(s) at %d fps", dur, len(plan), rt.fps))
	dir := ""
	if b.download != nil {
		dir, err = b.download(ctx, m, emit)
	} else {
		dir, err = b.fetch(ctx, m, emit)
	}
	if err != nil {
		return err
	}
	var peak float64
	frames := 0
	clips := make([]string, len(plan))
	for i, p := range plan {
		cEmit := emit
		if len(plan) > 1 {
			cEmit = func(ev Event) {
				if ev.Type == "status" {
					ev.Text = fmt.Sprintf("clip %d/%d · %s", i+1, len(plan), ev.Text)
				}
				emit(ev)
			}
		}
		c := avatarClip{Model: dir, Prompt: r.Prompt, Width: r.Width, Height: r.Height, Frames: p.Render, FPS: rt.fps,
			Seed: r.Seed + int64(i), LowRAM: r.LowRAM, Image: r.Images[0],
			Audio: filepath.Join(work, fmt.Sprintf("clip-%02d.wav", i+1)), Out: filepath.Join(work, fmt.Sprintf("clip-%02d-raw.mp4", i+1))}
		if i > 0 && !rt.refPortrait {
			c.Image = filepath.Join(work, fmt.Sprintf("clip-%02d-last.png", i))
			if err := lastFrame(ctx, ffmpeg, clips[i-1], c.Image); err != nil {
				return fmt.Errorf("clip %d: the last frame of clip %d: %w", i+1, i, err)
			}
		}
		secs := float64(p.Render) / float64(rt.fps)
		if err := ffmpegRun(ctx, ffmpeg, "-ss", ff(max(p.AudioStart, 0)), "-t", ff(secs), "-i", speech,
			"-af", "apad=whole_dur="+ff(secs), "-c:a", "pcm_s16le", c.Audio); err != nil {
			return fmt.Errorf("clip %d: cutting the audio: %w", i+1, err)
		}
		emit.Log(fmt.Sprintf("clip %d/%d: %.2f–%.2f s, %d frames (keeps %d)", i+1, len(plan), p.Start, p.End, p.Render, p.Keep))
		if len(plan) > 1 {
			emit.Status(fmt.Sprintf("animating %d/%d …", i+1, len(plan)))
		} else {
			emit.Status("animating …")
		}
		pk, err := b.animateClip(ctx, m, rt, c, cEmit)
		if err != nil {
			if len(plan) > 1 {
				return fmt.Errorf("clip %d: %w", i+1, err)
			}
			return err
		}
		peak = max(peak, pk)
		frames += p.Keep
		clips[i] = filepath.Join(work, fmt.Sprintf("clip-%02d.mp4", i+1))
		if err := trimClip(ctx, ffmpeg, c.Out, p.Skip, p.Keep, rt.fps, clips[i]); err != nil {
			return fmt.Errorf("clip %d: %w", i+1, err)
		}
		emit(Event{Type: "stats", Data: map[string]any{"clipsDone": i + 1, "clips": len(plan)}})
	}

	// 3. the video with the speech
	emit.Status("muxing the speech …")
	emit.Progress(0, 0)
	out := filepath.Join(outDir, name+".mp4")
	if err := muxAvatar(ctx, ffmpeg, clips, speech, out); err != nil {
		return err
	}
	if m.runtime != "longcat" { // LTX builds its pipeline per clip: nothing worth keeping in memory
		b.stopWorkers("longcat")
		if b.state.get() == m.ID {
			b.state.set("")
		}
	}
	emit(Event{Type: "file", File: out}) // the video first, then the speech it says
	if spoken != "" {
		emit(Event{Type: "file", File: spoken})
	}
	stats := map[string]any{"seconds": time.Since(start).Seconds(), "duration": float64(frames) / float64(rt.fps),
		"speechSeconds": dur, "clips": len(plan)}
	if peak > 0 {
		stats["peakGB"] = peak
	}
	emit(Event{Type: "stats", Data: stats})
	emit.Log(fmt.Sprintf("talking video (%.1f s of speech, %d clip(s)) in %.0f s, peak %.1f GB", dur, len(plan), time.Since(start).Seconds(), peak))
	return nil
}

// avatarSize fits a size to a model: scaled down to maxArea pixels (keeping the shape), then to multiples of mult.
func avatarSize(w, h, mult, maxArea int) (int, int) {
	if maxArea > 0 && w*h > maxArea {
		k := math.Sqrt(float64(maxArea) / float64(w*h))
		w, h = int(float64(w)*k), int(float64(h)*k)
	}
	snap := func(v int) int { return max(mult, (v+mult/2)/mult*mult) }
	w, h = snap(w), snap(h)
	for maxArea > 0 && w*h > maxArea*11/10 && max(w, h) > mult { // rounding up overshot: step the longer side down
		if w >= h {
			w -= mult
		} else {
			h -= mult
		}
	}
	return w, h
}

// speakerEmit passes the speech model's events on, but not its files and stats (the avatar job has its own).
func speakerEmit(emit Emit) Emit {
	return func(ev Event) {
		switch ev.Type {
		case "file", "stats", "preview", "result":
			return
		case "status":
			ev.Text = "speaking · " + ev.Text
		}
		emit(ev)
	}
}

// localAudio is the file of a request's Audio: a /files/ url (under outDir) or a path.
func localAudio(a, outDir string) (string, error) {
	p := a
	if rest, ok := strings.CutPrefix(a, "/files/"); ok {
		p = filepath.Join(outDir, filepath.FromSlash(rest))
		if rel, err := filepath.Rel(outDir, p); err != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("unknown audio %s", a)
		}
	}
	if !fileExists(p) {
		return "", fmt.Errorf("no audio at %s", a)
	}
	return p, nil
}

func (b *AvatarBackend) fetch(ctx context.Context, m *Model, emit Emit) (string, error) {
	hm, err := hubModel(ctx, b.Store, m, func(p string) bool { return !models.Matches(p, m.ignore...) })
	if err != nil {
		if m.Gated {
			return "", fmt.Errorf("%s needs the LTX-2.5 pack, which is gated (the LTX-2 Community License): %w", m.ID, err)
		}
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

// animateClip renders one clip (retrying once with --low-ram after macOS's GPU watchdog) and returns the peak GB.
func (b *AvatarBackend) animateClip(ctx context.Context, m *Model, rt avatarRuntime, c avatarClip, emit Emit) (float64, error) {
	if b.animate != nil {
		return 0, b.animate(ctx, m, c, emit)
	}
	peak, err := b.workerClip(ctx, m, rt, c, emit)
	if err == nil || ctx.Err() != nil || !rt.lowRAMRetry || c.LowRAM || !gpuWatchdog(err.Error()) {
		return peak, err
	}
	emit.Log("macOS's GPU watchdog stopped the render; trying again with --low-ram (block by block)")
	emit.Status("GPU watchdog: retrying with low RAM …")
	c.LowRAM = true
	return b.workerClip(ctx, m, rt, c, emit)
}

func (b *AvatarBackend) workerClip(ctx context.Context, m *Model, rt avatarRuntime, c avatarClip, emit Emit) (float64, error) {
	if b.workers == nil {
		b.workers = map[string]*pyWorker{}
	}
	b.stopWorkers(m.runtime) // one model in memory
	w := b.workers[m.runtime]
	if w == nil {
		w = &pyWorker{env: rt.env, script: rt.script, args: rt.args}
		b.workers[m.runtime] = w
	}
	if err := w.start(ctx, b.Root, emit); err != nil {
		return 0, err
	}
	b.state.set(m.ID) // loading counts as loaded: it takes the memory (another LongCat variant: the worker reloads)
	ffmpeg, _ := exec.LookPath("ffmpeg")
	ev, err := w.call(ctx, map[string]any{"model": c.Model, "audio": c.Audio, "image": c.Image, "prompt": c.Prompt,
		"width": c.Width, "height": c.Height, "frames": c.Frames, "fps": c.FPS, "seed": c.Seed, "lowRam": c.LowRAM,
		"ffmpeg": ffmpeg, "output": c.Out}, func(ev Event) {
		if ev.Type == "log" && (tqdmStep.MatchString(ev.Text) || strings.HasPrefix(ev.Text, "[estimate]")) {
			return // progress bars (the worker reports the steps itself)
		}
		if sm := phase.FindStringSubmatch(ev.Text); ev.Type == "log" && sm != nil {
			emit.Status(sm[1] + " …") // ltx-2-mlx's "[Loading …] ..." phases
		}
		emit(ev)
	})
	if err != nil {
		if !w.running() {
			b.state.set("")
		}
		return 0, err
	}
	if !fileExists(c.Out) {
		return 0, fmt.Errorf("the worker wrote no video (%s)", c.Out)
	}
	emit.Log(fmt.Sprintf("clip: %d frames %dx%d in %.0f s, peak %.1f GB", c.Frames, c.Width, c.Height, ev.Seconds, ev.PeakGB))
	return ev.PeakGB, nil
}

// clipPlan is one clip of a talking video.
type clipPlan struct {
	Start, End float64 // the stretch of the speech it shows (seconds)
	AudioStart float64 // where its audio starts (a frame before Start when its first frame is the clip before's last)
	Render     int     // frames to make (what the model can make: ≥ Skip+Keep)
	Skip, Keep int     // frames dropped at the start, frames kept
}

// splitSpeech cuts [0, dur) into stretches of at most maxSec, at the middle of a pause where there is one (the
// latest one that fits; a hard cut otherwise). A stretch is never shorter than a third of maxSec unless it is the
// whole speech.
func splitSpeech(dur float64, silences [][2]float64, maxSec float64) []float64 {
	cuts := []float64{0}
	start := 0.0
	for dur-start > maxSec {
		cut := start + maxSec
		for _, s := range silences {
			mid := (s[0] + s[1]) / 2
			if mid > start+maxSec/3 && mid <= start+maxSec && mid < dur {
				cut = mid // the latest pause that fits (silences are in order)
			}
		}
		cuts = append(cuts, cut)
		start = cut
	}
	return append(cuts, dur)
}

// planClips turns the cut points into clips: the frames each one keeps (rounded on the whole timeline, so the
// video never drifts from the speech), and what it renders. A continuation whose first frame repeats the last
// kept frame of the clip before (dropFirst) renders one frame more, its audio starting a frame earlier, and drops it.
func planClips(cuts []float64, fps int, frames func(int) int, dropFirst bool) []clipPlan {
	var out []clipPlan
	f := float64(fps)
	for i := 0; i+1 < len(cuts); i++ {
		a, b := int(math.Round(cuts[i]*f)), int(math.Round(cuts[i+1]*f))
		if i+2 == len(cuts) {
			b = int(math.Ceil(cuts[i+1] * f)) // the last clip covers the whole speech
		}
		p := clipPlan{Start: cuts[i], End: cuts[i+1], Keep: max(1, b-a), AudioStart: float64(a) / f}
		if i > 0 && dropFirst {
			p.Skip = 1
			p.AudioStart -= 1 / f
		}
		p.Render = frames(p.Skip + p.Keep)
		out = append(out, p)
	}
	return out
}

var silenceRe = regexp.MustCompile(`silence_(start|end): (-?[0-9.]+)`)

// detectSilences lists the pauses of a wav ([start, end] seconds) with ffmpeg's silencedetect.
func detectSilences(ctx context.Context, ffmpeg, wav string) ([][2]float64, error) {
	out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostats", "-i", wav, "-af",
		"silencedetect=noise=-35dB:d=0.2", "-f", "null", "-").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("finding pauses: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseSilences(string(out)), nil
}

func parseSilences(log string) [][2]float64 {
	var out [][2]float64
	open := -1.0
	for _, m := range silenceRe.FindAllStringSubmatch(log, -1) {
		v, _ := strconv.ParseFloat(m[2], 64)
		if m[1] == "start" {
			open = max(v, 0)
		} else if open >= 0 {
			out = append(out, [2]float64{open, v})
			open = -1
		}
	}
	return out
}

// wavDuration reads a PCM wav's length from its header.
func wavDuration(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, fmt.Errorf("%s is not a wav file", path)
	}
	var byteRate uint32
	for i := 12; i+8 <= len(b); {
		id, size := string(b[i:i+4]), int(binary.LittleEndian.Uint32(b[i+4:i+8]))
		body := b[i+8:]
		switch id {
		case "fmt ":
			if len(body) >= 12 {
				byteRate = binary.LittleEndian.Uint32(body[8:12])
			}
		case "data":
			if byteRate == 0 {
				return 0, fmt.Errorf("%s: no format before the data", path)
			}
			size = min(size, len(body)) // ffmpeg writing to a pipe leaves the size unset
			return float64(size) / float64(byteRate), nil
		}
		i += 8 + size + size%2
	}
	return 0, fmt.Errorf("%s has no audio data", path)
}

// trimClip keeps frames [skip, skip+keep) of a clip, without sound (padding with the last frame if it is short).
func trimClip(ctx context.Context, ffmpeg, in string, skip, keep, fps int, out string) error {
	vf := fmt.Sprintf("trim=start_frame=%d,setpts=PTS-STARTPTS,tpad=stop_mode=clone:stop=%d,trim=end_frame=%d,setpts=PTS-STARTPTS", skip, keep, keep)
	return ffmpegRun(ctx, ffmpeg, "-i", in, "-an", "-vf", vf, "-r", strconv.Itoa(fps), "-c:v", "libx264", "-crf", "14",
		"-preset", "fast", "-pix_fmt", "yuv420p", out)
}

// muxAvatar joins the clips and puts the speech under them.
func muxAvatar(ctx context.Context, ffmpeg string, clips []string, speech, out string) error {
	var args []string
	var f strings.Builder
	for i, c := range clips {
		args = append(args, "-i", c)
		fmt.Fprintf(&f, "[%d:v]", i)
	}
	fmt.Fprintf(&f, "concat=n=%d:v=1:a=0[v]", len(clips))
	args = append(args, "-i", speech, "-filter_complex", f.String(), "-map", "[v]", "-map", fmt.Sprintf("%d:a", len(clips)),
		"-c:v", "libx264", "-crf", "18", "-preset", "medium", "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "192k",
		"-movflags", "+faststart")
	if err := ffmpegRun(ctx, ffmpeg, append(args, out)...); err != nil {
		return fmt.Errorf("muxing the speech: %w", err)
	}
	return nil
}

// ffmpegRun runs ffmpeg quietly (overwriting the output, the last argument).
func ffmpegRun(ctx context.Context, ffmpeg string, args ...string) error {
	cmd := exec.CommandContext(ctx, ffmpeg, append([]string{"-v", "error", "-y"}, args...)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}

func ff(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
