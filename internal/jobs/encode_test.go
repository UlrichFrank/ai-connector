package jobs

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/audio"
	"github.com/philipparndt/ai-connector/internal/engine"
)

// wavMaker is a backend that writes one second of a 440 Hz tone as a WAV (16 bit, mono, 8 kHz).
type wavMaker struct{}

func (wavMaker) Name() string   { return "wav" }
func (wavMaker) Heavy() bool    { return false }
func (wavMaker) Loaded() string { return "" }
func (wavMaker) Unload()        {}
func (wavMaker) Run(_ context.Context, _ *engine.Model, _ engine.Request, out string, emit engine.Emit) error {
	const rate, n = 8000, 8000
	b := make([]byte, 44+2*n)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+2*n))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1) // PCM
	binary.LittleEndian.PutUint16(b[22:], 1) // mono
	binary.LittleEndian.PutUint32(b[24:], rate)
	binary.LittleEndian.PutUint32(b[28:], rate*2)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 2*n)
	for i := range n {
		v := int16(math.Sin(2*math.Pi*440*float64(i)/rate) * 16000)
		binary.LittleEndian.PutUint16(b[44+2*i:], uint16(v))
	}
	f := filepath.Join(out, "tone.wav")
	if err := os.WriteFile(f, b, 0o644); err != nil {
		return err
	}
	emit(engine.Event{Type: "result", Data: map[string]any{"text": "kept"}})
	emit(engine.Event{Type: "file", File: f})
	return nil
}

// A job that asks for it gets its WAV as AAC (when this computer has an encoder) and the waveform's peaks.
func TestEncode(t *testing.T) {
	cat := &engine.Catalog{Models: []*engine.Model{{ID: "tone", Kind: engine.Sound, Backend: "wav"}}}
	m := New(cat, []engine.Backend{wavMaker{}}, t.TempDir())
	settle(t, m)
	j, err := m.Submit(engine.Request{Model: "tone", Encode: &engine.Encode{Format: "aac", Channels: 1, Peaks: 16}})
	if err != nil {
		t.Fatal(err)
	}
	j = wait(t, m, j.ID)
	enc, _ := j.Result["encoded"].(map[string]any)
	if j.State != Done || enc == nil || j.Result["text"] != "kept" {
		t.Fatalf("job %+v", j)
	}
	peaks, _ := enc["peaks"].([]float64)
	if len(peaks) != 16 || peaks[3] < 0.3 || enc["seconds"].(float64) < 0.99 {
		t.Fatalf("encoded %+v", enc)
	}
	if name, _ := audio.Encoder(); name == "" {
		if enc["mime"] != "audio/wav" || len(j.Files) != 1 {
			t.Fatalf("without an encoder %+v %v", enc, j.Files)
		}
		return
	}
	if enc["mime"] != "audio/mp4" || len(j.Files) != 2 || !strings.HasSuffix(j.Files[1], ".m4a") || enc["file"] != j.Files[1] {
		t.Fatalf("encoded %+v, files %v", enc, j.Files)
	}

	// without the option: the WAV alone
	j, _ = m.Submit(engine.Request{Model: "tone"})
	if j = wait(t, m, j.ID); len(j.Files) != 1 || j.Result["encoded"] != nil {
		t.Fatalf("not asked %+v", j)
	}
}
