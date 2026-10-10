package engine

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

	"github.com/philipparndt/ai-connector/internal/llm"
)

// solidPNG is a w×h PNG of one colour.
func solidPNG(w, h int, r, g, b byte) []byte {
	var raw bytes.Buffer
	for y := 0; y < h; y++ {
		raw.WriteByte(0)
		for x := 0; x < w; x++ {
			raw.Write([]byte{r, g, b})
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
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9] = 8, 2 // 8 bits, RGB
	chunk("IHDR", ihdr)
	chunk("IDAT", z.Bytes())
	chunk("IEND", nil)
	return out.Bytes()
}

// TestRuntimeImagesLive asks a real runtime's vision model about a red square (MLXLAB_IMAGES_LIVE=<runtime model id
// as the catalogue lists it>, e.g. lmstudio/qwen/qwen3.6-27b; MLXLAB_RUNTIMES as for the worker, default the known
// ones). The model must be marked as taking images and answer "red".
func TestRuntimeImagesLive(t *testing.T) {
	id := os.Getenv("MLXLAB_IMAGES_LIVE")
	if id == "" {
		t.Skip("MLXLAB_IMAGES_LIVE=<runtime model id> asks a vision model about an image")
	}
	rts, err := ParseRuntimes(os.Getenv("MLXLAB_RUNTIMES"))
	if err != nil {
		t.Fatal(err)
	}
	cat := &Catalog{}
	b := &OpenAIBackend{Catalog: cat, Runtimes: rts}
	b.Discover(context.Background())
	m := cat.Find(id)
	if m == nil || !m.Images {
		t.Fatalf("%s: %+v (not found, or not marked as taking images)", id, m)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var text strings.Builder
	msgs := []llm.Message{{Role: "user", Content: "What colour is [image-1]? Answer with one word.",
		Images: []llm.Image{{ID: "image-1", MIME: "image/png", Data: solidPNG(64, 64, 255, 0, 0)}}}}
	err = b.Run(ctx, m, Request{Messages: msgs, MaxTokens: 400}, "", func(e Event) {
		if e.Type == "text" {
			text.WriteString(e.Text)
		}
	})
	t.Logf("answer: %q", text.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(text.String()), "red") {
		t.Fatalf("answer %q", text.String())
	}
}
