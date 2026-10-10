package audio

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Encoders are the programs that make AAC, in the order they are looked for: afconvert (part of macOS) and ffmpeg.
// afconvert only counts on macOS (tests use a fake ffmpeg on the PATH).
var Encoders = []string{"afconvert", "ffmpeg"}

// Encoder is the program that makes AAC here ("" none) and its path.
func Encoder() (name, path string) {
	for _, e := range Encoders {
		if e == "afconvert" && runtime.GOOS != "darwin" {
			continue
		}
		if p, err := exec.LookPath(e); err == nil {
			return e, p
		}
	}
	return "", ""
}

// ToAAC makes an AAC file in an MP4 container (.m4a, audio/mp4) of a WAV: bitrate bits a second, at most channels
// channels (a mono source stays mono). A rate AAC does not take well (Kokoro's and MOSS's 24 kHz) becomes 48 kHz.
// It returns the encoder's name; no encoder: ErrNoEncoder.
func ToAAC(ctx context.Context, in, out string, bitrate, channels int) (string, error) {
	name, path := Encoder()
	if name == "" {
		return "", ErrNoEncoder
	}
	info, err := ReadInfo(in)
	if err != nil {
		return name, err
	}
	ch := max(1, min(channels, info.Channels))
	rate := info.Rate
	if rate != 44100 && rate != 48000 {
		rate = 48000
	}
	tmp := out + ".part"
	defer os.Remove(tmp)
	var cmd *exec.Cmd
	if name == "afconvert" {
		cmd = exec.CommandContext(ctx, path, "-f", "m4af", "-d", "aac@"+strconv.Itoa(rate), "-b", strconv.Itoa(bitrate),
			"-c", strconv.Itoa(ch), in, tmp)
	} else {
		cmd = exec.CommandContext(ctx, path, "-y", "-v", "error", "-i", in, "-vn", "-c:a", "aac", "-b:a", strconv.Itoa(bitrate),
			"-ac", strconv.Itoa(ch), "-ar", strconv.Itoa(rate), "-movflags", "+faststart", "-f", "mp4", tmp)
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		return name, fmt.Errorf("%s: %v %s", name, err, strings.TrimSpace(string(b)))
	}
	if st, err := os.Stat(tmp); err != nil || st.Size() == 0 {
		return name, fmt.Errorf("%s made no file", name)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return name, err
	}
	return name, os.Rename(tmp, out)
}

// ErrNoEncoder: neither afconvert nor ffmpeg is there.
var ErrNoEncoder = fmt.Errorf("no AAC encoder (afconvert or ffmpeg)")
