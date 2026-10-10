// Package audio reads WAV files (their format, length and waveform) and makes AAC of them with the system's
// encoder (afconvert on macOS, else ffmpeg): the songs, sounds and speech that go to vboard's board.
package audio

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// Info is a WAV's format and length.
type Info struct {
	Rate, Channels, Bits int
	Float                bool // IEEE float samples (else integer PCM)
	Frames               int64
	Seconds              float64
}

// a WAV's data: where it starts and how many bytes it has
type wavData struct {
	Info
	offset, size int64
}

// readHeader reads the chunks up to "data". size is the file's length (a streamed WAV says 0 or 0xffffffff for its
// data: the rest of the file).
func readHeader(r io.Reader, size int64) (wavData, error) {
	var w wavData
	var hdr [12]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil || string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return w, errors.New("not a WAV file")
	}
	pos := int64(12)
	for {
		var ch [8]byte
		if _, err := io.ReadFull(r, ch[:]); err != nil {
			return w, errors.New("a WAV file without data")
		}
		pos += 8
		n := int64(binary.LittleEndian.Uint32(ch[4:8]))
		switch string(ch[0:4]) {
		case "fmt ":
			if n < 16 || n > 1<<16 {
				return w, errors.New("a WAV file with a broken format chunk")
			}
			b := make([]byte, n+n%2)
			if _, err := io.ReadFull(r, b); err != nil {
				return w, err
			}
			pos += int64(len(b))
			format := binary.LittleEndian.Uint16(b[0:2])
			w.Channels = int(binary.LittleEndian.Uint16(b[2:4]))
			w.Rate = int(binary.LittleEndian.Uint32(b[4:8]))
			w.Bits = int(binary.LittleEndian.Uint16(b[14:16]))
			if format == 0xfffe && n >= 26 { // extensible: the sub-format's first two bytes are the format
				format = binary.LittleEndian.Uint16(b[24:26])
			}
			switch {
			case format == 1 && (w.Bits == 8 || w.Bits == 16 || w.Bits == 24 || w.Bits == 32):
			case format == 3 && (w.Bits == 32 || w.Bits == 64):
				w.Float = true
			default:
				return w, fmt.Errorf("a WAV file of format %d with %d bits (PCM or float are read)", format, w.Bits)
			}
			if w.Channels < 1 || w.Rate < 1 {
				return w, errors.New("a WAV file without channels or rate")
			}
		case "data":
			if w.Rate == 0 {
				return w, errors.New("a WAV file whose data comes before its format")
			}
			if n == 0 || n == 0xffffffff || size > 0 && pos+n > size {
				n = max(0, size-pos)
			}
			w.offset, w.size = pos, n
			frame := int64(w.Channels * w.Bits / 8)
			w.Frames = n / frame
			w.Seconds = float64(w.Frames) / float64(w.Rate)
			return w, nil
		default:
			if _, err := io.CopyN(io.Discard, r, n+n%2); err != nil {
				return w, errors.New("a WAV file without data")
			}
			pos += n + n%2
		}
	}
}

// ReadInfo reads a WAV file's format and length.
func ReadInfo(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	w, err := readHeader(bufio.NewReader(f), st.Size())
	return w.Info, err
}

// PeaksFile is the waveform of a WAV file in n numbers 0..1: the largest magnitude of the mono mix (the channels'
// mean) in each of n equal slices of its length.
func PeaksFile(path string, n int) ([]float64, Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, Info{}, err
	}
	return peaks(bufio.NewReaderSize(f, 1<<16), st.Size(), n)
}

// Peaks is PeaksFile of a WAV in memory.
func Peaks(b []byte, n int) ([]float64, Info, error) {
	return peaks(bufio.NewReader(bytes.NewReader(b)), int64(len(b)), n)
}

func peaks(r *bufio.Reader, size int64, n int) ([]float64, Info, error) {
	w, err := readHeader(r, size)
	if err != nil {
		return nil, w.Info, err
	}
	out := make([]float64, n)
	if w.Frames == 0 || n <= 0 {
		return out, w.Info, nil
	}
	bps := w.Bits / 8
	frame := make([]byte, w.Channels*bps)
	for i := int64(0); i < w.Frames; i++ {
		if _, err := io.ReadFull(r, frame); err != nil {
			break // a short file: what was there
		}
		var sum float64
		for c := 0; c < w.Channels; c++ {
			sum += sample(frame[c*bps:(c+1)*bps], w.Bits, w.Float)
		}
		v := math.Abs(sum / float64(w.Channels))
		if s := int(i * int64(n) / w.Frames); v > out[s] {
			out[s] = v
		}
	}
	for i, v := range out {
		out[i] = math.Round(min(v, 1)*1000) / 1000
	}
	return out, w.Info, nil
}

// sample is one sample as -1..1.
func sample(b []byte, bits int, float bool) float64 {
	switch {
	case float && bits == 32:
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
	case float:
		return math.Float64frombits(binary.LittleEndian.Uint64(b))
	case bits == 8: // unsigned
		return (float64(b[0]) - 128) / 128
	case bits == 16:
		return float64(int16(binary.LittleEndian.Uint16(b))) / (1 << 15)
	case bits == 24:
		v := int32(uint32(b[0])<<8|uint32(b[1])<<16|uint32(b[2])<<24) >> 8
		return float64(v) / (1 << 23)
	}
	return float64(int32(binary.LittleEndian.Uint32(b))) / (1 << 31)
}
