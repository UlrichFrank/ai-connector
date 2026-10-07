package engine

import (
	"context"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestTempoPlan(t *testing.T) {
	off := false
	for _, c := range []struct {
		r       Request
		task    string
		bpm     int
		measure bool
		lock    bool
		takes   int
	}{
		{Request{}, "generate", 138, true, true, 1},         // a tempo: locked by default
		{Request{}, "generate", 0, false, false, 1},         // no tempo: nothing to lock to
		{Request{Takes: 3}, "generate", 0, false, false, 1}, // nor to choose by
		{Request{LockTempo: &off}, "generate", 138, false, false, 1},
		{Request{LockTempo: &off, Takes: 2}, "generate", 138, true, false, 2},
		{Request{Takes: 9}, "generate", 124, true, true, 3}, // at most 3
		{Request{}, "extend", 138, false, false, 1},         // only new songs
	} {
		got := tempoPlan(c.r, c.task, c.bpm)
		if got.measure != c.measure || got.lock != c.lock || got.takes != c.takes {
			t.Errorf("tempoPlan(%+v, %s, %d) = %+v", c.r, c.task, c.bpm, got)
		}
	}
}

func TestTempoReadingOff(t *testing.T) {
	r := tempoReading{Measured: 130, Tracked: 129.2, OK: true}
	if d := r.off(138); d < 0.057 || d > 0.059 {
		t.Errorf("off = %v", d)
	}
	if r.got() != 130 {
		t.Errorf("got = %v", r.got())
	}
	r.OK = false
	if !math.IsInf(r.off(138), 1) || r.got() != 129.2 {
		t.Errorf("no clear pulse: off %v got %v", r.off(138), r.got())
	}
}

func TestStretchTempo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.wav"), filepath.Join(dir, "out.wav")
	if b, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=10", in).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	how, err := stretchTempo(context.Background(), in, out, 138.0/130.0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", out).Output()
	if err != nil {
		t.Fatal(err)
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if want := 10 * 130.0 / 138.0; math.Abs(d-want) > 0.05 {
		t.Errorf("%s: %v s, want %v", how, d, want)
	}
}
