package engine

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/philipparndt/ai-connector/internal/models"
)

func TestSplitSpeech(t *testing.T) {
	// short: one stretch
	if got := splitSpeech(4, nil, 10); fmt.Sprint(got) != "[0 4]" {
		t.Fatalf("short: %v", got)
	}
	// cut at the middle of the latest pause that fits
	got := splitSpeech(25, [][2]float64{{3, 3.4}, {7.8, 8.2}, {12, 12.6}, {16, 16.4}, {23, 23.2}}, 10)
	if fmt.Sprint(got) != "[0 8 16.2 25]" {
		t.Fatalf("pauses: %v", got)
	}
	// no pause: hard cuts
	if got := splitSpeech(21, nil, 10); fmt.Sprint(got) != "[0 10 20 21]" {
		t.Fatalf("hard: %v", got)
	}
	// a pause too early (under a third of the clip) is not taken
	if got := splitSpeech(15, [][2]float64{{1, 1.2}}, 10); fmt.Sprint(got) != "[0 10 15]" {
		t.Fatalf("early pause: %v", got)
	}
}

func TestPlanClips(t *testing.T) {
	cuts := []float64{0, 8, 16.2, 25.01}
	p := planClips(cuts, 24, frames8k1Up, true)
	if len(p) != 3 {
		t.Fatal(p)
	}
	keep := 0
	for i, c := range p {
		keep += c.Keep
		if (c.Render-1)%8 != 0 || c.Render < c.Skip+c.Keep {
			t.Errorf("clip %d renders %d frames for %d+%d", i, c.Render, c.Skip, c.Keep)
		}
		if i > 0 && (c.Skip != 1 || math.Abs(c.AudioStart-(float64(int(math.Round(cuts[i]*24)))/24-1.0/24)) > 1e-9) {
			t.Errorf("continuation %d: %+v", i, c)
		}
	}
	if p[0].Skip != 0 || p[0].AudioStart != 0 {
		t.Errorf("first clip: %+v", p[0])
	}
	if want := int(math.Ceil(25.01 * 24)); keep != want {
		t.Errorf("kept %d frames, want %d (the whole speech, no drift)", keep, want)
	}
	// LongCat: 4k+1, nothing dropped
	q := planClips([]float64{0, 3.6, 5}, 25, frames4k1Up, false)
	if q[1].Skip != 0 || q[1].AudioStart != 3.6 || (q[1].Render-1)%4 != 0 || q[0].Keep+q[1].Keep != 125 {
		t.Errorf("longcat: %+v", q)
	}
}

func TestFrameRounding(t *testing.T) {
	for _, c := range [][3]int{{1, 9, 5}, {9, 9, 9}, {10, 17, 13}, {97, 97, 97}, {98, 105, 101}} {
		if a, b := frames8k1Up(c[0]), frames4k1Up(c[0]); a != c[1] || b != c[2] {
			t.Errorf("%d: %d %d, want %d %d", c[0], a, b, c[1], c[2])
		}
	}
}

func TestParseSilences(t *testing.T) {
	log := "[silencedetect @ 0x1] silence_start: -0.01\n[silencedetect @ 0x1] silence_end: 0.3 | silence_duration: 0.3\n" +
		"silence_start: 2.5\nsilence_end: 3.1 | silence_duration: 0.6\nsilence_start: 9.9\n"
	if got := parseSilences(log); fmt.Sprint(got) != "[[0 0.3] [2.5 3.1]]" {
		t.Fatal(got)
	}
}

func TestLocalAudio(t *testing.T) {
	out := t.TempDir()
	_ = os.MkdirAll(filepath.Join(out, "uploads"), 0o755)
	_ = os.WriteFile(filepath.Join(out, "uploads", "a.wav"), []byte("x"), 0o644)
	if p, err := localAudio("/files/uploads/a.wav", out); err != nil || p != filepath.Join(out, "uploads", "a.wav") {
		t.Fatal(p, err)
	}
	if _, err := localAudio("/files/../secret.wav", out); err == nil {
		t.Fatal("escaped the outputs")
	}
	if _, err := localAudio("/files/uploads/b.wav", out); err == nil {
		t.Fatal("a missing file")
	}
}

func needFFmpeg(t *testing.T) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("no ffprobe")
	}
	return ffmpeg
}

// tone writes a wav of dur seconds: a tone with a pause at [pause, pause+0.6) (none if pause < 0).
func tone(t *testing.T, ffmpeg, out string, dur, pause float64) {
	t.Helper()
	expr := fmt.Sprintf("if(between(t\\,%g\\,%g)\\,0\\,0.5*sin(2*PI*440*t))", pause, pause+0.6)
	if err := ffmpegRun(context.Background(), ffmpeg, "-f", "lavfi", "-i", fmt.Sprintf("aevalsrc=%s:s=24000:d=%g", expr, dur), out); err != nil {
		t.Fatal(err)
	}
}

func probe(t *testing.T, file, stream, entry string) string {
	t.Helper()
	b, err := exec.Command("ffprobe", "-v", "error", "-select_streams", stream, "-count_frames", "-show_entries",
		"stream="+entry, "-of", "csv=p=0", file).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

type fakeSpeaker struct {
	ffmpeg string
	t      *testing.T
	dur    float64
	got    []Request
}

func (f *fakeSpeaker) Speak(ctx context.Context, model string, r Request, out string, emit Emit) error {
	f.got = append(f.got, r)
	emit.Status("loading qwen3-tts …")
	emit(Event{Type: "stats", Data: map[string]any{"seconds": 1.0}}) // must not reach the avatar job
	tone(f.t, f.ffmpeg, out, f.dur, 2)
	emit(Event{Type: "file", File: out})
	return nil
}

// fakeAvatar is a backend whose clips are test patterns of the asked length.
func fakeAvatar(t *testing.T, ffmpeg string, sp Speaker) (*AvatarBackend, *[]avatarClip) {
	var mu sync.Mutex
	var clips []avatarClip
	b := &AvatarBackend{Speaker: sp,
		download: func(context.Context, *Model, Emit) (string, error) { return "/models/pack", nil },
		animate: func(ctx context.Context, m *Model, c avatarClip, emit Emit) error {
			mu.Lock()
			clips = append(clips, c)
			mu.Unlock()
			if !fileExists(c.Image) || !fileExists(c.Audio) {
				t.Errorf("clip inputs missing: %+v", c)
			}
			emit.Progress(1, 1)
			return ffmpegRun(ctx, ffmpeg, "-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%dx%d:rate=%d", c.Width, c.Height, c.FPS),
				"-frames:v", strconv.Itoa(c.Frames), "-pix_fmt", "yuv420p", c.Out)
		}}
	return b, &clips
}

func portraitPNG(t *testing.T, ffmpeg, dir string) string {
	p := filepath.Join(dir, "portrait.png")
	if err := ffmpegRun(context.Background(), ffmpeg, "-f", "lavfi", "-i", "color=c=gray:s=256x320", "-frames:v", "1", p); err != nil {
		t.Fatal(err)
	}
	return p
}

func ltxAvatar(t *testing.T) *Model {
	for _, m := range avatarModels(&models.Store{Dir: t.TempDir()}) {
		if m.ID == "ltx-2.5-avatar" {
			return m
		}
	}
	t.Fatal("no ltx-2.5-avatar")
	return nil
}

// A text is spoken first, then animated; long speech is split at its pause into chained clips; the original
// speech is the sound of the final video, which is exactly as long as the speech.
func TestAvatarTextFlow(t *testing.T) {
	ffmpeg := needFFmpeg(t)
	dir := t.TempDir()
	sp := &fakeSpeaker{ffmpeg: ffmpeg, t: t, dur: 4.5}
	b, clips := fakeAvatar(t, ffmpeg, sp)
	var evs []Event
	emit := func(ev Event) { evs = append(evs, ev) }
	r := Request{Images: []string{portraitPNG(t, ffmpeg, dir)}, Text: "Hallo zusammen", Voice: "anna", SpeechModel: "qwen3-tts-0.6b",
		Width: 256, Height: 320, Frames: 73, Seed: 5} // clips of at most 70 frames ≈ 2.9 s: two clips, cut in the pause at 2–2.6 s
	if err := b.Run(context.Background(), ltxAvatar(t), r, dir, emit); err != nil {
		t.Fatal(err)
	}
	if len(sp.got) != 1 || sp.got[0].Text != "Hallo zusammen" || sp.got[0].Voice != "anna" {
		t.Fatalf("speaker got %+v", sp.got)
	}
	var files, statuses []string
	duration := 0.0
	for _, ev := range evs {
		switch ev.Type {
		case "file":
			files = append(files, ev.File)
		case "status":
			statuses = append(statuses, ev.Text)
		case "stats":
			if s, ok := ev.Data["seconds"].(float64); ok && s == 1.0 {
				t.Error("the speaker's stats reached the job")
			}
			if d, ok := ev.Data["duration"].(float64); ok {
				duration = d
			}
		}
	}
	if len(files) != 2 || !strings.HasSuffix(files[0], ".mp4") || !strings.HasSuffix(files[1], ".wav") {
		t.Fatalf("files %v (the video first)", files)
	}
	if st := strings.Join(statuses, "|"); !strings.HasPrefix(st, "speaking …") || !strings.Contains(st, "animating 1/2 …") {
		t.Errorf("statuses %v", statuses)
	}
	if want := math.Ceil(4.5*24) / 24; math.Abs(duration-want) > 1e-9 {
		t.Errorf("stats.duration %v, want %v", duration, want)
	}
	if len(*clips) != 2 {
		t.Fatalf("%d clips, want 2", len(*clips))
	}
	c0, c1 := (*clips)[0], (*clips)[1]
	if c0.Image != r.Images[0] || !strings.HasSuffix(c1.Image, "clip-01-last.png") || c1.Seed != c0.Seed+1 {
		t.Errorf("chaining: %+v %+v", c0, c1)
	}
	if c0.Width != 256 || c0.Height != 320 || c0.FPS != 24 || (c0.Frames-1)%8 != 0 || c0.Prompt == "" {
		t.Errorf("clip 0: %+v", c0)
	}
	out := files[0]
	if n, _ := strconv.Atoi(probe(t, out, "v:0", "nb_read_frames")); n != int(math.Ceil(4.5*24)) {
		t.Errorf("%d video frames, want %d", n, int(math.Ceil(4.5*24)))
	}
	if codec := probe(t, out, "a:0", "codec_name"); codec != "aac" {
		t.Errorf("audio codec %q", codec)
	}
	if d, _ := strconv.ParseFloat(probe(t, out, "a:0", "duration"), 64); math.Abs(d-4.5) > 0.1 {
		t.Errorf("audio %.2f s, want 4.5", d)
	}
}

// An uploaded audio file (a /files/ url) needs no speaker; a text without a speaker explains itself.
func TestAvatarAudioAndNoSpeaker(t *testing.T) {
	ffmpeg := needFFmpeg(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "uploads"), 0o755)
	tone(t, ffmpeg, filepath.Join(dir, "uploads", "speech.mp3"), 1.5, -1)
	b, clips := fakeAvatar(t, ffmpeg, nil)
	img := portraitPNG(t, ffmpeg, dir)
	var files []string
	err := b.Run(context.Background(), ltxAvatar(t), Request{Images: []string{img}, Audio: "/files/uploads/speech.mp3", Width: 256, Height: 320},
		dir, func(ev Event) {
			if ev.Type == "file" {
				files = append(files, ev.File)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(*clips) != 1 || len(files) != 1 || !strings.HasSuffix(files[0], ".mp4") {
		t.Fatalf("clips %d files %v", len(*clips), files)
	}
	err = b.Run(context.Background(), ltxAvatar(t), Request{Images: []string{img}, Text: "Hallo"}, dir, func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "speech model") {
		t.Fatalf("no speaker: %v", err)
	}
	if err := b.Run(context.Background(), ltxAvatar(t), Request{Audio: "/files/uploads/speech.mp3"}, dir, func(Event) {}); err == nil ||
		!strings.Contains(err.Error(), "portrait") {
		t.Fatalf("no portrait: %v", err)
	}
}

// TestAvatarLive renders a real one (MLXLAB_AVATAR_LIVE=<model id>; MLXLAB_HOME, AVATAR_IMAGE, AVATAR_AUDIO,
// AVATAR_SIZE=WxH, AVATAR_FRAMES, AVATAR_SEED, AVATAR_OUT).
func TestAvatarLive(t *testing.T) {
	id := os.Getenv("MLXLAB_AVATAR_LIVE")
	if id == "" {
		t.Skip("MLXLAB_AVATAR_LIVE not set")
	}
	root := ConfigDir()
	srcs, _ := models.Sources("", nil)
	store := &models.Store{Dir: filepath.Join(root, "models"), Sources: srcs}
	var m *Model
	for _, x := range avatarModels(store) {
		if x.ID == id {
			m = x
		}
	}
	if m == nil {
		t.Fatalf("no model %s", id)
	}
	r := Request{Images: []string{os.Getenv("AVATAR_IMAGE")}, Audio: os.Getenv("AVATAR_AUDIO")}
	if s := os.Getenv("AVATAR_SIZE"); s != "" {
		_, _ = fmt.Sscanf(s, "%dx%d", &r.Width, &r.Height)
	}
	r.Frames, _ = strconv.Atoi(os.Getenv("AVATAR_FRAMES"))
	r.Seed, _ = strconv.ParseInt(os.Getenv("AVATAR_SEED"), 10, 64)
	b := &AvatarBackend{Root: root, Store: store}
	defer b.Unload()
	err := b.Run(context.Background(), m, r, os.Getenv("AVATAR_OUT"), func(ev Event) {
		switch ev.Type {
		case "progress":
		case "stats":
			t.Logf("stats %v", ev.Data)
		default:
			t.Logf("%s %s%s", ev.Type, ev.Text, ev.File)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The web app's sizes (multiples of 32 around 640²) snap to what each model takes.
func TestAvatarSize(t *testing.T) {
	ltx, lc := avatarRuntimes["ltx-a2v"], avatarRuntimes["longcat"]
	for _, c := range []struct{ w, h, mult, area, ww, wh int }{
		{640, 640, ltx.mult, ltx.maxArea, 640, 640},
		{480, 832, ltx.mult, ltx.maxArea, 512, 832},
		{832, 480, ltx.mult, ltx.maxArea, 832, 512},
		{544, 736, ltx.mult, ltx.maxArea, 576, 768},
		{384, 512, ltx.mult, ltx.maxArea, 384, 512},
		{640, 640, lc.mult, lc.maxArea, 336, 336},
		{480, 832, lc.mult, lc.maxArea, 256, 432},
		{256, 432, lc.mult, lc.maxArea, 256, 432},
	} {
		w, h := avatarSize(c.w, c.h, c.mult, c.area)
		if w != c.ww || h != c.wh || w%c.mult != 0 || h%c.mult != 0 {
			t.Errorf("%dx%d (×%d, ≤%d px): %dx%d, want %dx%d", c.w, c.h, c.mult, c.area, w, h, c.ww, c.wh)
		}
	}
}

func TestAvatarCatalog(t *testing.T) {
	ms := avatarModels(&models.Store{Dir: t.TempDir()})
	if len(ms) != 3 {
		t.Fatal(len(ms))
	}
	for _, m := range ms {
		if m.Kind != Avatar || m.Backend != "avatar" || m.Licence == "" || m.Defaults.Width == 0 || avatarRuntimes[m.runtime].fps == 0 {
			t.Errorf("%+v", m)
		}
	}
}
