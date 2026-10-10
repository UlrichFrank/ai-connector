package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/philipparndt/ai-connector/internal/audio/audiotest"
)

// ramp: n frames whose mono mix rises in magnitude from 0 to 1, every other frame negative.
func ramp(n, channels int) [][]float64 {
	out := make([][]float64, n)
	for i := range out {
		v := float64(i) / float64(n-1)
		if channels == 1 {
			out[i] = []float64{v}
		} else {
			out[i] = []float64{v, v}
		}
		if i%2 == 1 {
			for c := range out[i] {
				out[i][c] = -out[i][c] // negative samples count by their magnitude
			}
		}
	}
	return out
}

func TestPeaks(t *testing.T) {
	for _, x := range []struct {
		name              string
		channels, bits    int
		float, extensible bool
	}{
		{"16-bit mono", 1, 16, false, false},
		{"24-bit mono", 1, 24, false, false},
		{"32-bit int", 1, 32, false, false},
		{"float32", 1, 32, true, false},
		{"float64", 1, 64, true, false},
		{"16-bit stereo", 2, 16, false, false},
		{"24-bit stereo extensible", 2, 24, false, true},
		{"float stereo extensible", 2, 32, true, true},
		{"8-bit", 1, 8, false, false},
	} {
		b := audiotest.WAV(1000, x.channels, x.bits, x.float, x.extensible, ramp(2560, x.channels))
		p, info, err := Peaks(b, 256)
		if err != nil {
			t.Fatalf("%s: %v", x.name, err)
		}
		if info.Rate != 1000 || info.Channels != x.channels || info.Bits != x.bits || info.Float != x.float ||
			info.Frames != 2560 || math.Abs(info.Seconds-2.56) > 1e-9 {
			t.Fatalf("%s: info %+v", x.name, info)
		}
		if len(p) != 256 {
			t.Fatalf("%s: %d peaks", x.name, len(p))
		}
		tol := 0.002
		if x.bits == 8 {
			tol = 0.01
		}
		for i, v := range p {
			want := float64(i*10+9) / 2559 // the slice's last frame is its loudest
			if math.Abs(v-want) > tol || v < 0 || v > 1 {
				t.Fatalf("%s: peak %d = %v, want %v", x.name, i, v, want)
			}
		}
	}
	// the channels are mixed: opposite channels cancel
	frames := make([][]float64, 100)
	for i := range frames {
		frames[i] = []float64{0.5, -0.5}
	}
	if p, _, _ := Peaks(audiotest.WAV(8000, 2, 16, false, false, frames), 4); p[0] != 0 || p[3] != 0 {
		t.Fatalf("mixed %v", p)
	}
	// fewer frames than slices: the empty slices are 0, the rest still found
	p, _, err := Peaks(audiotest.WAV(8000, 1, 16, false, false, [][]float64{{0.25}, {1}}), 4)
	if err != nil || p[0] != 0.25 || p[2] != 1 || p[1] != 0 || p[3] != 0 {
		t.Fatalf("short %v %v", p, err)
	}
	// not a WAV, an unknown format
	if _, _, err := Peaks([]byte("ID3 an mp3"), 8); err == nil {
		t.Fatal("an mp3 read as WAV")
	}
	bad := audiotest.WAV(8000, 1, 16, false, false, nil)
	binary.LittleEndian.PutUint16(bad[20:], 2) // ADPCM
	if _, _, err := Peaks(bad, 8); err == nil {
		t.Fatal("ADPCM read")
	}
	// a streamed WAV (data size 0): the rest of the file
	st := audiotest.WAV(1000, 1, 16, false, false, ramp(500, 1))
	binary.LittleEndian.PutUint32(st[len(st)-1000-4:], 0)
	if _, info, err := Peaks(st, 8); err != nil || info.Frames != 500 {
		t.Fatalf("streamed %+v %v", info, err)
	}
	// from a file
	f := filepath.Join(t.TempDir(), "a.wav")
	_ = os.WriteFile(f, audiotest.WAV(1000, 2, 24, false, false, ramp(1000, 2)), 0o644)
	if p, info, err := PeaksFile(f, 10); err != nil || info.Seconds != 1 || p[9] != 1 {
		t.Fatalf("file %v %+v %v", p, info, err)
	}
}

func TestGuessLanguage(t *testing.T) {
	for in, want := range map[string]string{
		"Guten Morgen, wie geht es dir heute?": "de",
		"Good morning, how are you today?":     "en",
		"你好，世界。今天天气很好。":                        "zh",
		"こんにちは、世界。今日はいい天気です。":                  "ja",
		"안녕하세요":         "ko",
		"नमस्ते दुनिया": "hi",
		"Привет, мир":   "ru",
		"Xyz":           "",
	} {
		if got := GuessLanguage(in); got != want {
			t.Errorf("GuessLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToAACFake(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a shell script as the encoder")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	old := Encoders
	Encoders = []string{"ffmpeg"}
	t.Cleanup(func() { Encoders = old })
	args := audiotest.FakeFFmpeg(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "a.wav")
	_ = os.WriteFile(in, audiotest.WAV(24000, 1, 16, false, false, ramp(100, 1)), 0o644)
	name, err := ToAAC(context.Background(), in, filepath.Join(dir, "a.m4a"), 96000, 2)
	if err != nil || name != "ffmpeg" {
		t.Fatalf("%s %v", name, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.m4a"))
	a, _ := os.ReadFile(args)
	// mono stays mono, 24 kHz becomes 48 kHz
	if !bytes.HasPrefix(b, []byte("AAC:RIFF")) || !bytes.Contains(a, []byte("-b:a 96000 -ac 1 -ar 48000")) {
		t.Fatalf("out %q args %s", b[:8], a)
	}
	// none: ErrNoEncoder
	t.Setenv("PATH", t.TempDir())
	if _, err := ToAAC(context.Background(), in, filepath.Join(dir, "b.m4a"), 96000, 1); !errors.Is(err, ErrNoEncoder) {
		t.Fatalf("no encoder: %v", err)
	}
}

// The real encoder (afconvert on macOS, else ffmpeg) when there is one.
func TestToAACReal(t *testing.T) {
	if name, _ := Encoder(); name == "" {
		t.Skip("no encoder")
	}
	dir := t.TempDir()
	for _, x := range []struct {
		rate, ch, bitrate int
	}{{24000, 1, 96000}, {44100, 2, 192000}, {48000, 2, 192000}} {
		frames := make([][]float64, x.rate*2)
		for i := range frames {
			v := 0.4 * math.Sin(float64(i)*0.07)
			frames[i] = []float64{v, v}[:x.ch]
		}
		in := filepath.Join(dir, "in.wav")
		_ = os.WriteFile(in, audiotest.WAV(x.rate, x.ch, 16, false, false, frames), 0o644)
		out := filepath.Join(dir, "out.m4a")
		if name, err := ToAAC(context.Background(), in, out, x.bitrate, 2); err != nil {
			t.Fatalf("%d Hz %d ch with %s: %v", x.rate, x.ch, name, err)
		}
		b, _ := os.ReadFile(out)
		if len(b) < 1000 || !bytes.Contains(b[:64], []byte("ftyp")) {
			t.Fatalf("%d Hz: not an MP4 (%d bytes)", x.rate, len(b))
		}
	}
}
