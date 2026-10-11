package audio

import (
	"bufio"
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/philipparndt/ai-connector/internal/audio/audiotest"
)

// tone: seconds of a 220 Hz tone at amplitude amp(t) (t in seconds), the same in every channel.
func tone(rate, channels int, seconds float64, amp func(t float64) float64) [][]float64 {
	out := make([][]float64, int(seconds*float64(rate)))
	for i := range out {
		t := float64(i) / float64(rate)
		v := amp(t) * math.Sin(2*math.Pi*220*t)
		out[i] = make([]float64, channels)
		for c := range out[i] {
			out[i][c] = v
		}
	}
	return out
}

// fadeFile writes a WAV, runs FadeCut on it and returns what it said, the bytes before and after, and the header.
func fadeFile(t *testing.T, rate, channels, bits int, float bool, frames [][]float64) (bool, []byte, []byte, wavData) {
	t.Helper()
	b := audiotest.WAV(rate, channels, bits, float, false, frames)
	path := filepath.Join(t.TempDir(), "song.wav")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	faded, err := FadeCut(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := readHeader(bufio.NewReader(bytes.NewReader(after)), int64(len(after)))
	if err != nil {
		t.Fatal(err)
	}
	return faded, b, after, w
}

// frameAt is frame i's largest sample magnitude.
func frameAt(b []byte, w wavData, i int64) float64 {
	bps := int64(w.Bits / 8)
	at := w.offset + i*int64(w.Channels)*bps
	var m float64
	for c := int64(0); c < int64(w.Channels); c++ {
		m = max(m, math.Abs(sample(b[at+c*bps:at+(c+1)*bps], w.Bits, w.Float)))
	}
	return m
}

func TestFadeCutLoudEnd(t *testing.T) {
	for _, x := range []struct {
		name           string
		channels, bits int
		float          bool
	}{
		{"16-bit stereo", 2, 16, false},
		{"24-bit mono", 1, 24, false},
		{"float32 stereo", 2, 32, true},
		{"8-bit", 1, 8, false},
	} {
		const rate = 8000
		loud := func(float64) float64 { return 0.5 }
		faded, before, after, w := fadeFile(t, rate, x.channels, x.bits, x.float, tone(rate, x.channels, 10, loud))
		if !faded {
			t.Fatalf("%s: a constant-loud end was not faded", x.name)
		}
		n := int64(fadeLength * rate)
		start := w.offset + (w.Frames-n)*int64(w.Channels*w.Bits/8)
		if !bytes.Equal(before[:start], after[:start]) {
			t.Fatalf("%s: frames before the fade changed", x.name)
		}
		if bytes.Equal(before[start:], after[start:]) {
			t.Fatalf("%s: the fade changed nothing", x.name)
		}
		if v := frameAt(after, w, w.Frames-1); v > 1e-6 && !(x.bits == 8 && v <= 1.0/128) {
			t.Fatalf("%s: the last frame is %g, not 0", x.name, v)
		}
		// the envelope falls: the loudest frame of each half second is quieter than the one before
		prev := 1.0
		for s := w.Frames - n; s < w.Frames; s += rate / 2 {
			var m float64
			for i := s; i < min(s+rate/2, w.Frames); i++ {
				m = max(m, frameAt(after, w, i))
			}
			if m >= prev && m > 1.0/128 {
				t.Fatalf("%s: the fade does not fall at %d (%g after %g)", x.name, s, m, prev)
			}
			prev = m
		}
		// half-way down: −20 dB of the curve's 40 (+ the pull to zero)
		var m float64
		for i := w.Frames - n/2 - 50; i < w.Frames-n/2+50; i++ {
			m = max(m, frameAt(after, w, i))
		}
		if g := dB(m / 0.5); g > -19 || g < -22 {
			t.Fatalf("%s: half-way the fade is at %.1f dB, want about −20", x.name, g)
		}
		// a second pass: the end is quiet now
		path := filepath.Join(t.TempDir(), "again.wav")
		_ = os.WriteFile(path, after, 0o644)
		if again, err := FadeCut(path); err != nil || again {
			t.Fatalf("%s: a faded song faded again (%v, %v)", x.name, again, err)
		}
	}
}

func TestFadeCutLeavesQuietEnds(t *testing.T) {
	const rate = 8000
	for name, amp := range map[string]func(float64) float64{
		// the model faded out itself, over its last 3 s
		"fades": func(t float64) float64 { return 0.5 * min(1, max(0, (10-t)/3)) },
		// a final hit ringing out: down 20 dB a second after 8 s
		"decays": func(t float64) float64 { return 0.5 * math.Pow(10, -max(0, t-8)) },
		// the music stops, then silence
		"silence": func(t float64) float64 {
			if t > 8.5 {
				return 0
			}
			return 0.5
		},
		"all silent": func(float64) float64 { return 0 },
	} {
		faded, before, after, _ := fadeFile(t, rate, 2, 16, false, tone(rate, 2, 10, amp))
		if faded || !bytes.Equal(before, after) {
			t.Fatalf("%s: a quiet end was touched (faded %v)", name, faded)
		}
	}
}

func TestFadeCutSilentIntro(t *testing.T) {
	// most of the clip silent, then music to the end: the silence does not count toward the song's loudness
	const rate = 8000
	amp := func(t float64) float64 {
		if t < 7 {
			return 0
		}
		return 0.3
	}
	if faded, _, _, _ := fadeFile(t, rate, 1, 16, false, tone(rate, 1, 12, amp)); !faded {
		t.Fatal("a cut after a silent intro was not faded")
	}
}

func TestFadeCutShortClip(t *testing.T) {
	const rate = 8000
	loud := func(float64) float64 { return 0.5 }
	for _, seconds := range []float64{0.6, 2, 6} {
		faded, before, after, w := fadeFile(t, rate, 2, 16, false, tone(rate, 2, seconds, loud))
		if !faded {
			t.Fatalf("%g s: not faded", seconds)
		}
		n := w.Frames / 3 // a third of the clip
		start := w.offset + (w.Frames-n)*4
		if !bytes.Equal(before[:start], after[:start]) || bytes.Equal(before[start:], after[start:]) {
			t.Fatalf("%g s: not just the last third faded", seconds)
		}
		if v := frameAt(after, w, w.Frames-1); v > 1e-6 {
			t.Fatalf("%g s: the last frame is %g", seconds, v)
		}
	}
	// a clip of one frame: nothing to fade
	if faded, _, _, _ := fadeFile(t, rate, 1, 16, false, [][]float64{{0.5}}); faded {
		t.Fatal("a one-frame clip faded")
	}
}
