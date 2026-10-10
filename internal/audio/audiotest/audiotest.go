// Package audiotest makes WAV files and a fake encoder for tests.
package audiotest

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// WAV makes a WAV of frames (each frame one value per channel, -1..1) in the given sample format; extensible writes
// the WAVE_FORMAT_EXTENSIBLE header, and a "LIST" chunk comes before the data (readers must skip it).
func WAV(rate, channels, bits int, float, extensible bool, frames [][]float64) []byte {
	var data bytes.Buffer
	for _, fr := range frames {
		for _, v := range fr {
			switch {
			case float && bits == 32:
				_ = binary.Write(&data, binary.LittleEndian, float32(v))
			case float:
				_ = binary.Write(&data, binary.LittleEndian, v)
			case bits == 8:
				data.WriteByte(byte(int(math.Round(v*127)) + 128))
			case bits == 16:
				_ = binary.Write(&data, binary.LittleEndian, int16(math.Round(v*32767)))
			case bits == 24:
				x := int32(math.Round(v * 8388607))
				data.Write([]byte{byte(x), byte(x >> 8), byte(x >> 16)})
			default:
				_ = binary.Write(&data, binary.LittleEndian, int32(math.Round(v*2147483647)))
			}
		}
	}
	format := uint16(1)
	if float {
		format = 3
	}
	var fmtc bytes.Buffer
	tag := format
	if extensible {
		tag = 0xfffe
	}
	le := func(v any) { _ = binary.Write(&fmtc, binary.LittleEndian, v) }
	le(tag)
	le(uint16(channels))
	le(uint32(rate))
	le(uint32(rate * channels * bits / 8))
	le(uint16(channels * bits / 8))
	le(uint16(bits))
	if extensible {
		le(uint16(22))
		le(uint16(bits))
		le(uint32(0))
		le(format)
		fmtc.Write([]byte{0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71})
	}
	var out bytes.Buffer
	chunk := func(id string, b []byte) {
		out.WriteString(id)
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(b)))
		out.Write(b)
		if len(b)%2 == 1 {
			out.WriteByte(0)
		}
	}
	var body bytes.Buffer
	body.WriteString("WAVE")
	out.Reset()
	chunk("fmt ", fmtc.Bytes())
	chunk("LIST", []byte("INFOxyz"))
	chunk("data", data.Bytes())
	body.Write(out.Bytes())
	var file bytes.Buffer
	file.WriteString("RIFF")
	_ = binary.Write(&file, binary.LittleEndian, uint32(body.Len()))
	file.Write(body.Bytes())
	return file.Bytes()
}

// FakeFFmpeg puts an "ffmpeg" on the PATH that copies its input (the argument after -i) to its output (the last)
// and logs its arguments into logFile (the caller makes ffmpeg the only encoder: audio.Encoders).
func FakeFFmpeg(t testing.TB) (logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" > " + logFile + "\nin=\nprev=\nfor a in \"$@\"; do\n  if [ \"$prev\" = \"-i\" ]; then in=\"$a\"; fi\n  prev=\"$a\"\n  out=\"$a\"\ndone\nprintf 'AAC:' > \"$out\"\ncat \"$in\" >> \"$out\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	return logFile
}
