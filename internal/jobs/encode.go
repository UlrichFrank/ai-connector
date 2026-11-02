package jobs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/audio"
	"github.com/philipparndt/ai-connector/internal/engine"
)

// encode makes a finished job's WAV ready for an application when its request asks (engine.Request.Encode): AAC,
// added to the job's files, and result.encoded {file, mime, size, seconds, peaks, encoder}. Without an encoder
// (afconvert, ffmpeg) the WAV is the file ("mime": "audio/wav"). A job without a WAV has nothing to encode.
func (m *Manager) encode(ctx context.Context, j *Job) error {
	e := j.Request.Encode
	if e == nil {
		return nil
	}
	m.mu.Lock()
	wav := ""
	for _, f := range j.Files {
		if strings.EqualFold(filepath.Ext(f), ".wav") {
			wav = f
		}
	}
	m.mu.Unlock()
	if wav == "" {
		return nil
	}
	bitrate, channels, n := encodeSettings(*e)
	peaks, info, err := audio.PeaksFile(wav, n)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	file, mime := wav, "audio/wav"
	out := strings.TrimSuffix(wav, filepath.Ext(wav)) + ".m4a"
	t0 := time.Now()
	enc, err := audio.ToAAC(ctx, wav, out, bitrate, channels)
	switch {
	case errors.Is(err, audio.ErrNoEncoder):
	case err != nil:
		return fmt.Errorf("encode: %w", err)
	default:
		file, mime = out, "audio/mp4"
		log.Printf("audio: %s → AAC %d kbit/s (%s, %.1f s)", filepath.Base(wav), bitrate/1000, enc, time.Since(t0).Seconds())
		m.event(j, engine.Event{Type: "file", File: out})
	}
	st, err := os.Stat(file)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	m.mu.Lock()
	res := mapWith(j.Result, "encoded", map[string]any{"file": file, "mime": mime, "size": st.Size(),
		"seconds": info.Seconds, "peaks": peaks, "encoder": enc})
	m.mu.Unlock()
	m.event(j, engine.Event{Type: "result", Data: res})
	return nil
}

// encodeSettings are the request's settings within their limits, the defaults for those not given.
func encodeSettings(e engine.Encode) (bitrate, channels, peaks int) {
	bitrate, channels, peaks = 192000, 2, 256
	if e.Bitrate != 0 {
		bitrate = min(max(e.Bitrate, 32000), 320000)
	}
	if e.Channels != 0 {
		channels = min(max(e.Channels, 1), 2)
	}
	if e.Peaks != 0 {
		peaks = min(max(e.Peaks, 0), 4096)
	}
	return bitrate, channels, peaks
}
