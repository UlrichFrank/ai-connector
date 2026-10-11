package audio

import (
	"bufio"
	"encoding/binary"
	"io"
	"math"
	"os"
	"slices"
)

// A song made to a length often does not end: the model is still mid-song when the length runs out and the audio
// just stops (HeartMuLa 3B, 150 s asked: −18…−20 dB in every 5 s window up to the last). FadeCut finds such a hard
// cut and fades it out; a song that already ends quietly is left alone.
const (
	cutWindow  = 1.0  // seconds: the song's loudness is measured in windows this long, the end is the last one
	cutWithin  = 10.0 // dB: an end this close to the median window (or louder) is a cut
	cutSilent  = -60  // dBFS: windows quieter than this are no music (silence before or between parts)
	fadeLength = 3.0  // seconds of fade; a clip shorter than 3× this fades over its last third
	fadeDepth  = 40.0 // dB the fade's curve falls before it is pulled down to zero
)

// FadeCut fades out a WAV whose end is a hard cut — its last second within cutWithin dB of the median second
// (silent ones left out) — over its last fadeLength seconds, in place: only the faded frames are rewritten, every
// frame before them stays as it was. It says whether it faded.
//
// The curve is exponential: the gain falls by the same number of dB every moment (fadeDepth over the fade, about
// 13 dB a second), which the ear hears as an even fade, unlike a linear ramp (which hangs on and then drops in its
// last part) or an equal-power quarter-sine (made for crossfades: still −3 dB at half-time, then a fast drop). It is
// shifted and scaled to start at exactly 1 and land on exactly 0: g(t) = (10^(−d·t/20) − 10^(−d/20)) / (1 − 10^(−d/20)).
func FadeCut(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	w, err := readHeader(bufio.NewReaderSize(f, 1<<16), st.Size())
	if err != nil {
		return false, err
	}
	if w.Frames < 2 {
		return false, nil
	}
	// a clip shorter than a window: windows of all of it (one window is its own median: a cut)
	win := min(int64(cutWindow*float64(w.Rate)), w.Frames)
	if _, err := f.Seek(w.offset, io.SeekStart); err != nil {
		return false, err
	}
	levels, err := windowLevels(bufio.NewReaderSize(f, 1<<16), w, win)
	if err != nil {
		return false, err
	}
	if !isCut(levels) {
		return false, nil
	}

	n := min(int64(fadeLength*float64(w.Rate)), w.Frames/3)
	if n < 2 {
		return false, nil
	}
	bps := int64(w.Bits / 8)
	frame := int64(w.Channels) * bps
	at := w.offset + (w.Frames-n)*frame
	buf := make([]byte, n*frame)
	if _, err := f.ReadAt(buf, at); err != nil {
		return false, err
	}
	floor := math.Pow(10, -fadeDepth/20)
	for i := int64(0); i < n; i++ {
		t := float64(i+1) / float64(n) // the fade's first frame already falls a little, its last is 0
		g := (math.Pow(10, -fadeDepth*t/20) - floor) / (1 - floor)
		for c := int64(0); c < int64(w.Channels); c++ {
			b := buf[i*frame+c*bps : i*frame+(c+1)*bps]
			putSample(b, w.Bits, w.Float, sample(b, w.Bits, w.Float)*g)
		}
	}
	if _, err := f.WriteAt(buf, at); err != nil {
		return false, err
	}
	return true, nil
}

// windowLevels are the levels (dBFS, the RMS of every channel's samples) of the clip in windows of win frames,
// counted back from its end: the last level is its last win frames (a part shorter than win at the start is dropped
// unless it is all there is).
func windowLevels(r io.Reader, w wavData, win int64) ([]float64, error) {
	bps := w.Bits / 8
	frame := make([]byte, w.Channels*bps)
	skip := w.Frames % win // the start's remainder, so the last window ends on the last frame
	if skip == w.Frames {
		skip = 0
	}
	var levels []float64
	var sum float64
	var count int64
	for i := int64(0); i < w.Frames; i++ {
		if _, err := io.ReadFull(r, frame); err != nil {
			return nil, err
		}
		if i < skip {
			continue
		}
		for c := 0; c < w.Channels; c++ {
			v := sample(frame[c*bps:(c+1)*bps], w.Bits, w.Float)
			sum += v * v
		}
		if count++; count == win {
			levels = append(levels, dB(math.Sqrt(sum/float64(win*int64(w.Channels)))))
			sum, count = 0, 0
		}
	}
	return levels, nil
}

// isCut: the last level is music and within cutWithin dB of the median of the music's levels.
func isCut(levels []float64) bool {
	if len(levels) == 0 || levels[len(levels)-1] < cutSilent {
		return false
	}
	var music []float64
	for _, l := range levels {
		if l >= cutSilent {
			music = append(music, l)
		}
	}
	slices.Sort(music)
	median := music[len(music)/2]
	if len(music)%2 == 0 {
		median = (music[len(music)/2-1] + median) / 2
	}
	return levels[len(levels)-1] >= median-cutWithin
}

func dB(rms float64) float64 {
	if rms <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(rms)
}

// putSample writes v (-1..1, clipped) as one sample: the reverse of sample.
func putSample(b []byte, bits int, float bool, v float64) {
	switch {
	case float && bits == 32:
		binary.LittleEndian.PutUint32(b, math.Float32bits(float32(v)))
		return
	case float:
		binary.LittleEndian.PutUint64(b, math.Float64bits(v))
		return
	}
	v = max(-1, min(v, 1))
	switch bits {
	case 8:
		b[0] = byte(max(0, min(255, math.Round(v*128+128))))
	case 16:
		binary.LittleEndian.PutUint16(b, uint16(int16(max(-1<<15, min(1<<15-1, math.Round(v*(1<<15)))))))
	case 24:
		x := int32(max(-1<<23, min(1<<23-1, math.Round(v*(1<<23)))))
		b[0], b[1], b[2] = byte(x), byte(x>>8), byte(x>>16)
	default:
		binary.LittleEndian.PutUint32(b, uint32(int32(max(-1<<31, min(1<<31-1, math.Round(v*(1<<31)))))))
	}
}
