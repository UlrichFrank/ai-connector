package agents

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"hash/crc32"
	"os"
	"strings"
	"testing"
	"time"
)

// redPNG is a 32×32 red PNG.
func redPNG() []byte {
	var raw bytes.Buffer
	for y := 0; y < 32; y++ {
		raw.WriteByte(0)
		for x := 0; x < 32; x++ {
			raw.Write([]byte{255, 0, 0})
		}
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(raw.Bytes())
	zw.Close()
	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(typ string, data []byte) {
		binary.Write(&out, binary.BigEndian, uint32(len(data)))
		out.WriteString(typ)
		out.Write(data)
		binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 32)
	binary.BigEndian.PutUint32(ihdr[4:], 32)
	ihdr[8], ihdr[9] = 8, 2
	chunk("IHDR", ihdr)
	chunk("IDAT", z.Bytes())
	chunk("IEND", nil)
	return out.Bytes()
}

// TestImagesLive: one turn of the REAL Claude Code (AGENTS_IMAGES_LIVE=1, claude on the PATH, logged in; a haiku
// turn, about 1–2 US cents) with an image block through the json driver.
func TestImagesLive(t *testing.T) {
	if os.Getenv("AGENTS_IMAGES_LIVE") == "" {
		t.Skip("AGENTS_IMAGES_LIVE=1 runs one real Claude Code turn with an image")
	}
	m := &Manager{Root: t.TempDir(), Command: "claude"}
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := m.Turn(ctx, Options{Model: "haiku", Images: []Image{{ID: "image-1", MIME: "image/png", Data: redPNG()}}},
		"What colour is [image-1]? Answer with one word.", nil)
	t.Logf("answer %q, $%.4f, %d+%d tokens", res.Text, res.CostUSD, res.InputTokens+res.CacheReadTokens+res.CacheWriteTokens, res.OutputTokens)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(res.Text), "red") {
		t.Fatalf("answer %q", res.Text)
	}
}
