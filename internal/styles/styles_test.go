package styles

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// tone writes a sine tone of the given length (mp3, mono 44.1 kHz: the conversion has work to do).
func tone(t *testing.T, seconds string, freq string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	f := filepath.Join(t.TempDir(), "tone.mp3")
	out, err := exec.Command("ffmpeg", "-y", "-v", "error", "-f", "lavfi", "-i", "sine=frequency="+freq+":duration="+seconds,
		"-ar", "44100", f).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	return f
}

func TestSplit(t *testing.T) {
	for _, c := range []struct {
		sec, seg float64
		want     [][2]float64
	}{
		{3, 30, nil},                                        // too short
		{12, 30, [][2]float64{{0, 12}}},                     // shorter than a segment: itself
		{30, 30, [][2]float64{{0, 30}}},                     // exactly one
		{65, 30, [][2]float64{{0, 30}, {30, 30}}},           // 5 s left over: dropped
		{70, 30, [][2]float64{{0, 30}, {30, 30}, {40, 30}}}, // 10 s left: one more, ending at the end
		{90, 30, [][2]float64{{0, 30}, {30, 30}, {60, 30}}},
	} {
		if got := Split(c.sec, c.seg); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Split(%v, %v) = %v, want %v", c.sec, c.seg, got, c.want)
		}
	}
	long := Split(15*60, 10)
	if len(long) != 30 || long[0][0] != 0 || long[29][0] != 15*60-10 {
		t.Errorf("a long track: %d segments, last at %v", len(long), long[len(long)-1])
	}
}

func TestLibrary(t *testing.T) {
	ctx := context.Background()
	lib := Open(filepath.Join(t.TempDir(), "styles"))
	if all, err := lib.List(); err != nil || len(all) != 0 {
		t.Fatalf("empty: %v %v", all, err)
	}
	if _, err := lib.Create("  ", "m", Defaults()); err == nil {
		t.Fatal("a style needs a name")
	}
	s, err := lib.Create("  Deep  Trance ", "ace-step-1.5-turbo", Defaults())
	must(t, err)
	if s.Name != "Deep Trance" || !strings.HasPrefix(s.ID, "deep-trance-") || !strings.HasPrefix(s.Trigger, "deep-trance-") ||
		s.Trigger == s.ID || s.Strength != 1 || s.Status != New || s.Settings != Defaults() {
		t.Fatalf("created %+v", s)
	}

	// a track: converted to 48 kHz stereo wav, segments planned
	f, _ := os.Open(tone(t, "70", "220"))
	defer f.Close()
	s2, tr, err := lib.AddTrack(ctx, s.ID, "my track.mp3", f, false)
	must(t, err)
	if tr.Name != "my track" || len(tr.ID) != 16 || tr.Segments != 3 || tr.Seconds < 69.9 || tr.Seconds > 70.1 ||
		filepath.Dir(tr.File) != filepath.Join(lib.Path(s.ID), "tracks") || s2.Status != Ready || len(s2.Tracks) != 1 {
		t.Fatalf("track %+v, style %+v", tr, s2)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=sample_rate,channels,codec_name",
		"-of", "default=noprint_wrappers=1", tr.File).Output()
	must(t, err)
	for _, want := range []string{"pcm_s16le", "sample_rate=48000", "channels=2"} {
		if !strings.Contains(string(probe), want) {
			t.Errorf("track: want %s in %s", want, probe)
		}
	}
	// the same audio again: one track
	f2, _ := os.Open(tone(t, "70", "220"))
	defer f2.Close()
	s2, tr2, err := lib.AddTrack(ctx, s.ID, "again.mp3", f2, false)
	must(t, err)
	if tr2.ID != tr.ID || len(s2.Tracks) != 1 {
		t.Fatalf("duplicate: %+v", s2.Tracks)
	}
	// too short
	f3, _ := os.Open(tone(t, "2", "330"))
	defer f3.Close()
	if _, _, err := lib.AddTrack(ctx, s.ID, "short.mp3", f3, false); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("short: %v", err)
	}
	f4, _ := os.Open(tone(t, "20", "440"))
	defer f4.Close()
	_, short, err := lib.AddTrack(ctx, s.ID, "short vocal.mp3", f4, true)
	must(t, err)

	// the segments: cut, 30 s each (20 s for the short track)
	segs, err := lib.Prepare(ctx, s.ID)
	must(t, err)
	if len(segs) != 4 || segs[2].Start != 40 || segs[3].Seconds != 20 || !segs[3].Vocals {
		t.Fatalf("segments %+v", segs)
	}
	for _, sg := range segs {
		if d := Duration(sg.File); d < sg.Seconds-0.1 || d > sg.Seconds+0.1 {
			t.Errorf("%s: %v s, want %v", sg.File, d, sg.Seconds)
		}
	}

	// settings: other segments; Prepare drops the old files
	set := Defaults()
	set.SegmentSeconds = 20
	s3, err := lib.Update(s.ID, Patch{Settings: &set})
	must(t, err)
	if s3.Tracks[0].Segments != 4 || s3.SegmentCount() != 5 {
		t.Fatalf("segments after the change: %+v", s3.Tracks)
	}
	segs, err = lib.Prepare(ctx, s.ID)
	must(t, err)
	files, _ := filepath.Glob(filepath.Join(lib.Path(s.ID), "work", "segments", "*.wav"))
	if len(segs) != 5 || len(files) != 5 {
		t.Fatalf("after the change: %d segments, %d files", len(segs), len(files))
	}
	bad := Defaults()
	bad.SegmentSeconds = 500
	if _, err := lib.Update(s.ID, Patch{Settings: &bad}); err == nil {
		t.Fatal("500 s segments")
	}

	// rename, strength
	name, str := "Trance", 0.6
	s3, err = lib.Update(s.ID, Patch{Name: &name, Strength: &str})
	must(t, err)
	if s3.Name != "Trance" || s3.Strength != 0.6 {
		t.Fatalf("patched %+v", s3)
	}
	str = 3
	if _, err := lib.Update(s.ID, Patch{Strength: &str}); err == nil {
		t.Fatal("strength 3")
	}

	// a track goes with its segments
	s3, err = lib.DeleteTrack(s.ID, short.ID)
	must(t, err)
	files, _ = filepath.Glob(filepath.Join(lib.Path(s.ID), "work", "segments", short.ID+"-*"))
	if len(s3.Tracks) != 1 || len(files) != 0 || fileExists(short.File) {
		t.Fatalf("deleted track: %+v %v", s3.Tracks, files)
	}
	if _, err := lib.DeleteTrack(s.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown track: %v", err)
	}

	// training state: Reset keeps the tracks and encoded segments, drops the rest
	must(t, os.MkdirAll(filepath.Join(lib.Path(s.ID), "work", "checkpoint"), 0o755))
	must(t, os.MkdirAll(filepath.Join(lib.Path(s.ID), "lora"), 0o755))
	_, err = lib.Mutate(s.ID, func(x *Style) error {
		x.Status, x.Weights, x.Progress.Checkpoint = Trained, filepath.Join(lib.Path(s.ID), "lora"), 100
		return nil
	})
	must(t, err)
	s3, err = lib.Reset(s.ID)
	must(t, err)
	if s3.Status != Ready || s3.Weights != "" || s3.Progress.Checkpoint != 0 || len(s3.Tracks) != 1 ||
		fileExists(filepath.Join(lib.Path(s.ID), "lora")) || fileExists(filepath.Join(lib.Path(s.ID), "work", "checkpoint")) {
		t.Fatalf("reset %+v", s3)
	}

	// listed; a folder without style.json is not a style; ids do not leave the folder
	must(t, os.MkdirAll(filepath.Join(lib.Dir, "junk"), 0o755))
	all, err := lib.List()
	must(t, err)
	if len(all) != 1 || all[0].ID != s.ID {
		t.Fatalf("list %+v", all)
	}
	for _, id := range []string{"..", "../x", "", ".hidden", "junk"} {
		if _, err := lib.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q): %v", id, err)
		}
	}
	must(t, lib.Delete(s.ID))
	if fileExists(lib.Path(s.ID)) {
		t.Fatal("not deleted")
	}
	if err := lib.Delete(s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestLossesAndEstimate(t *testing.T) {
	var p Progress
	for i := 1; i <= MaxPoints+10; i++ {
		p.AddLoss(i, float64(i))
	}
	if len(p.Losses) > MaxPoints || p.Losses[len(p.Losses)-1].Step != MaxPoints+10 {
		t.Fatalf("%d points, last %v", len(p.Losses), p.Losses[len(p.Losses)-1])
	}
	for i := 1; i < len(p.Losses); i++ {
		if p.Losses[i].Step <= p.Losses[i-1].Step {
			t.Fatalf("not in order at %d: %v %v", i, p.Losses[i-1], p.Losses[i])
		}
	}
	// a resumed run repeats the steps after its checkpoint: the later points go
	var q Progress
	for i := 1; i <= 10; i++ {
		q.AddLoss(i, 1)
	}
	q.AddLoss(7, 2)
	if len(q.Losses) != 7 || q.Losses[6] != (Point{7, 2}) {
		t.Fatalf("resumed %v", q.Losses)
	}

	s := Style{Settings: Defaults(), Tracks: []Track{{Seconds: 120, Segments: 4}}}
	e := Estimate(s, 2.5, 20)
	// 90 s + 4 × 20 s + 1000 × 2.5 s + 4 samples × 27 s
	if e != 90+80+2500+4*27 {
		t.Fatalf("estimate %v", e)
	}
	s.Progress.Checkpoint, s.Progress.SecondsPerStep = 500, 2
	if e := Estimate(s, 2.5, 20); e != 90+80+1000+2*27 {
		t.Fatalf("estimate when resuming %v", e)
	}
}
