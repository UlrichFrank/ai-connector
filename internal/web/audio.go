package web

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/philipparndt/ai-connector/internal/audio"
)

// POST /api/audio: a song's, sound's or speech's WAV made ready for vboard's board (docs/api.md, "Music, sound and
// speech") — here, where the WAV is, so only the small AAC file goes through the tunnel. Body {"file": "/files/…wav",
// "bitrate": 192000, "channels": 2, "peaks": 256}; the answer {"file": "/files/…m4a", "mime": "audio/mp4", "size",
// "seconds", "peaks": [256 numbers 0..1], "encoder"} — without an encoder the WAV itself ("mime": "audio/wav").
func (s *Server) encodeAudio(w http.ResponseWriter, r *http.Request) {
	var body struct {
		File     string `json:"file"`
		Bitrate  int    `json:"bitrate"`
		Channels int    `json:"channels"`
		Peaks    int    `json:"peaks"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		directError(w, http.StatusBadRequest, "request", err.Error())
		return
	}
	in, ok := s.local(body.File)
	if !ok || strings.ToLower(filepath.Ext(in)) != ".wav" {
		directError(w, http.StatusNotFound, "file", "no WAV file "+body.File)
		return
	}
	bitrate := min(max(body.Bitrate, 32000), 320000)
	if body.Bitrate == 0 {
		bitrate = 192000
	}
	ch := min(max(body.Channels, 1), 2)
	if body.Channels == 0 {
		ch = 2
	}
	n := min(max(body.Peaks, 0), 4096)
	if body.Peaks == 0 {
		n = 256
	}
	peaks, info, err := audio.PeaksFile(in, n)
	if err != nil {
		directError(w, http.StatusBadRequest, "wav", err.Error())
		return
	}
	file, mime := body.File, "audio/wav"
	out := strings.TrimSuffix(in, filepath.Ext(in)) + ".m4a"
	t0 := time.Now()
	enc, err := audio.ToAAC(r.Context(), in, out, bitrate, ch)
	switch {
	case errors.Is(err, audio.ErrNoEncoder):
	case err != nil:
		directError(w, http.StatusInternalServerError, "encode", err.Error())
		return
	default:
		file, mime = strings.TrimSuffix(body.File, filepath.Ext(body.File))+".m4a", "audio/mp4"
		log.Printf("audio: %s → AAC %d kbit/s (%s, %.1f s)", filepath.Base(in), bitrate/1000, enc, time.Since(t0).Seconds())
	}
	p, _ := s.local(file)
	st, err := os.Stat(p)
	if err != nil {
		directError(w, http.StatusInternalServerError, "encode", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"file": file, "mime": mime, "size": st.Size(), "seconds": info.Seconds,
		"peaks": peaks, "encoder": enc})
}
