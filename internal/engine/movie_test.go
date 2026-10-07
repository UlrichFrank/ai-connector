package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// clip makes a 1-second 24 fps test clip with a tone.
func clip(t *testing.T, ffmpeg, path string, hz int) {
	t.Helper()
	cmd := exec.Command(ffmpeg, "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=size=128x64:rate=24:duration=1",
		"-f", "lavfi", "-i", "sine=frequency="+strconv.Itoa(hz)+":duration=1", "-shortest", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

func frameCount(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// A continued clip loses its first frame at the seam (it repeats the last frame of the clip before); a new scene
// keeps all of its frames.
func TestJoinAndLastFrame(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	a, b, c := filepath.Join(dir, "a.mp4"), filepath.Join(dir, "b.mp4"), filepath.Join(dir, "c.mp4")
	clip(t, ffmpeg, a, 440)
	clip(t, ffmpeg, b, 660)
	clip(t, ffmpeg, c, 880)

	png := filepath.Join(dir, "last.png")
	if err := lastFrame(context.Background(), ffmpeg, a, png); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(png); err != nil || st.Size() == 0 {
		t.Fatal("no last frame")
	}

	out := filepath.Join(dir, "movie.mp4")
	script := []Clip{{Prompt: "a"}, {Prompt: "b", Continue: true}, {Prompt: "c"}}
	if err := join(context.Background(), ffmpeg, []string{a, b, c}, script, 24, true, out); err != nil {
		t.Fatal(err)
	}
	if got, want := frameCount(t, out), 24+23+24; got != want {
		t.Fatalf("%d frames, want %d", got, want)
	}
	streams, _ := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", out).Output()
	if !strings.Contains(string(streams), "audio") {
		t.Fatal("no sound in the movie")
	}

	silent := filepath.Join(dir, "silent.mp4")
	if err := join(context.Background(), ffmpeg, []string{a, b}, script[:2], 24, false, silent); err != nil {
		t.Fatal(err)
	}
	if frameCount(t, silent) != 47 {
		t.Fatal("video-only join")
	}
}

func TestFrames8k1(t *testing.T) {
	for in, want := range map[int]int{0: 0, 1: 9, 9: 9, 24: 17, 97: 97, 100: 97, 121: 121} {
		if got := frames8k1(in); got != want {
			t.Errorf("frames8k1(%d) = %d, want %d", in, got, want)
		}
	}
}

// A retry renders only what changed: finished clips are reused, an edited clip and the continuations after it are
// rendered again.
func TestMovieResumes(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	var rendered []string
	b := &LTXBackend{renderClip: func(prompt string, frames int, images []string, seed int64, out string) error {
		rendered = append(rendered, filepath.Base(out))
		clip(t, ffmpeg, out, 300+int(seed%10)*50)
		return nil
	}}
	out := t.TempDir()
	r := Request{Width: 128, Height: 64, FPS: 24, Seed: 7, Script: []Clip{
		{Prompt: "a fox"}, {Prompt: "the fox runs", Continue: true}, {Prompt: "a new day"}}}
	run := func(name string) {
		t.Helper()
		rendered = nil
		if _, err := b.movie(context.Background(), ltxRun{}, "ltx", r, out, name, func(Event) {}); err != nil {
			t.Fatal(err)
		}
	}
	run("first")
	if len(rendered) != 3 {
		t.Fatalf("first run rendered %v", rendered)
	}
	run("again")
	if len(rendered) != 0 {
		t.Fatalf("a retry rendered %v again", rendered)
	}
	r.Script[0].Prompt = "a red fox" // the first clip changes: the one continuing from it too, not the new scene
	run("edited")
	if strings.Join(rendered, ",") != "clip-01.mp4,clip-02.mp4" {
		t.Fatalf("after an edit rendered %v", rendered)
	}
	if frameCount(t, filepath.Join(out, "edited.mp4")) != 24+23+24 {
		t.Fatal("the joined film")
	}
}
